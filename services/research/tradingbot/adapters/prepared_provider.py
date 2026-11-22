"""Run one leased allocator call over identity-bound, locally archived evidence."""

import hashlib
import json
from collections.abc import Callable
from datetime import UTC, datetime
from importlib.metadata import version
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Event, Lock
from typing import Annotated, Literal
from uuid import UUID

from langsmith import tracing_context
from pydantic import Field

from tradingbot.adapters.offline import Identifier, IdentityOfflineJob, _verified_upstream
from tradingbot.adapters.tradingagents import research_config
from tradingbot.budget import CallBudget, CallBudgetExceeded
from tradingbot.contracts import Contract, DecisionBatch
from tradingbot.llm_usage import LLMCall
from tradingbot.prepared_analysis import build_prepared_reports
from tradingbot.research import create_decisions
from tradingbot.security_identity import load_identity_snapshot
from tradingbot.snapshots import ArchiveReference

_OPENAI_ENDPOINT = "https://api.openai.com/v1"
_MAX_REQUEST_SECONDS = 60


class ProviderConfig(Contract):
    llm_provider: Literal["chatgpt", "openai"]
    deep_think_llm: Identifier


class PreparedProviderJob(IdentityOfflineJob):
    job_schema_version: Literal["0.3"]
    analysis_mode: Literal["prepared_provider"]
    max_llm_calls: Annotated[int, Field(strict=True, ge=1, le=1)]


class _LeasedBudget(CallBudget):
    def __init__(
        self,
        config: ProviderConfig,
        ensure_active: Callable[[], None],
        authorize_call: Callable[[UUID], None],
        record_call: Callable[[LLMCall], None],
    ):
        self._provider = config.llm_provider
        self._model = config.deep_think_llm
        super().__init__(1, fixed_identity=(self._provider, self._model))
        self._ensure_active = ensure_active
        self._authorize_call = authorize_call
        self._record_call = record_call
        self._admission = Lock()
        self._admitted = False
        self._recorded = Event()

    def on_chat_model_start(self, serialized, messages, *, run_id, **kwargs):
        self._ensure_active()
        with self._admission:
            if self._admitted:
                raise CallBudgetExceeded("prepared-provider research admits one model call")
            self._admitted = True
        self._authorize_call(run_id)
        self._ensure_active()
        super().on_chat_model_start(serialized, messages, run_id=run_id, **kwargs)

    def on_llm_new_token(self, token: str, **kwargs):
        self._ensure_active()

    def _finish(self, run_id, status, response):
        call = super()._finish(run_id, status, response)
        if call is not None:
            # The durable receipt attributes the configured destination. The
            # result carries no price, including for non-standard usage tiers.
            receipt = call.model_copy(update={"provider": self._provider, "model": self._model})
            self._record_call(receipt)
            if status == "completed":
                self._recorded.set()
        return call

    def require_completed_receipt(self) -> None:
        self._ensure_active()
        if not self._recorded.is_set():
            raise RuntimeError("prepared-provider research requires a completed usage receipt")


def _create_model(config: dict, *, callbacks: list, timeout: float):
    from tradingagents.llm_clients import create_tier_client

    return create_tier_client(config, "deep", callbacks=callbacks, timeout=timeout).get_llm()


def analyze_prepared_provider(
    request: dict,
    attempt: int,
    attempt_deadline: datetime,
    cancelled: Event,
    *,
    archive_root: Path,
    owner_identity_registry: ArchiveReference,
    provider_config: ProviderConfig,
    authorize_call: Callable[[UUID], None],
    record_call: Callable[[LLMCall], None],
) -> dict:
    def ensure_active() -> None:
        if cancelled.is_set() or datetime.now(UTC) >= attempt_deadline:
            raise RuntimeError("prepared-provider research attempt is no longer active")

    ensure_active()
    job = PreparedProviderJob.model_validate(request)
    if job.identity_registry != owner_identity_registry or job.portfolio.as_of > datetime.now(UTC):
        raise ValueError("prepared-provider research requires a current owner-pinned portfolio")
    snapshot = load_identity_snapshot(
        archive_root, job.data_manifest.model_dump(), job.identity_registry, job.portfolio.as_of,
        job.security_bindings, positions=job.portfolio.positions,
    )
    if snapshot.source_mode != "local_archive":
        raise ValueError("prepared-provider research requires a local archive")
    reports = build_prepared_reports(snapshot, job.requested_symbols)
    upstream_version, upstream_sha = _verified_upstream()
    settings = provider_config.model_dump()
    with TemporaryDirectory(prefix="tradingbot-prepared-") as runtime, tracing_context(enabled=False):
        config = research_config(Path(runtime), settings | {
            "quick_think_llm": provider_config.deep_think_llm,
            "quick_think_provider": provider_config.llm_provider,
            "deep_think_provider": provider_config.llm_provider,
            "backend_url": _OPENAI_ENDPOINT,
            "quick_think_backend_url": None,
            "deep_think_backend_url": None,
        })
        stable_config = {
            key: value for key, value in config.items()
            if key not in {"results_dir", "data_cache_dir", "memory_log_path"}
        }
        config_hash = hashlib.sha256(json.dumps(
            {"analysis_mode": job.analysis_mode, "config": stable_config},
            sort_keys=True, separators=(",", ":"), allow_nan=False,
        ).encode()).hexdigest()
        budget = _LeasedBudget(provider_config, ensure_active, authorize_call, record_call)
        ensure_active()
        timeout = min(_MAX_REQUEST_SECONDS, (attempt_deadline - datetime.now(UTC)).total_seconds())
        model = _create_model(config, callbacks=[budget], timeout=timeout)
        with budget.role("portfolio_allocator"):
            decision = create_decisions(model, reports, job.portfolio)
        budget.require_completed_receipt()
        batch = DecisionBatch.model_validate(
            decision.model_dump() | {"expires_at": min(decision.expires_at, attempt_deadline)}
        )
    ensure_active()
    return {
        "result_schema_version": "0.4",
        "batch": batch.model_dump(mode="json"),
        "reports": reports,
        "metadata": budget.summary() | {
            "source_mode": "prepared_archive_research",
            "alpha_measured": False,
            "intraday_pit_verified": False,
            "provenance": {
                "job_id": str(job.job_id),
                "attempt": attempt,
                "policy_version": job.policy_version,
                "data_manifest": job.data_manifest.model_dump(mode="json"),
                "worker_version": version("self-custody-ai-trader"),
                "upstream_version": upstream_version,
                "upstream_sha": upstream_sha,
                "config_sha256": config_hash,
                "model_id": provider_config.deep_think_llm,
                "model_provider": provider_config.llm_provider,
                "archive_source_mode": snapshot.source_mode,
                "identity_registry": job.identity_registry.model_dump(mode="json"),
                "security_bindings": [binding.model_dump(mode="json") for binding in job.security_bindings],
                "projected_snapshot_sha256": snapshot.sha256,
            },
        },
    }
