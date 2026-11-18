"""Bound provider calls and report observed usage without inventing costs."""

from contextlib import contextmanager
from contextvars import ContextVar
from datetime import UTC, datetime
from pathlib import Path
from threading import Lock
from typing import Literal
from uuid import UUID

from langchain_core.callbacks import BaseCallbackHandler

from tradingbot.llm_usage import (
    MAX_CAPTURE_CALLS,
    NO_USAGE,
    CallRole,
    CaptureSource,
    LLMCall,
    LLMUsageReport,
    observe_usage,
)

ModelIdentity = tuple[str, str]


class CallBudgetExceeded(RuntimeError):
    pass


class CallBudget(BaseCallbackHandler):
    raise_error = True

    def __init__(
        self,
        max_calls: int,
        *,
        identities: dict[ModelIdentity, ModelIdentity] | None = None,
        fixed_identity: ModelIdentity | None = None,
    ):
        if max_calls <= 0:
            raise ValueError("max_calls must be positive")
        self.max_calls = max_calls
        self._lock = Lock()
        self._attempts: dict[UUID, LLMCall] = {}
        self._role: ContextVar[CallRole] = ContextVar("llm_call_role", default="unknown")
        self._identities = dict(identities or {})
        self._fixed_identity = fixed_identity

    def configure_identities(self, identities: dict[ModelIdentity, ModelIdentity]) -> None:
        with self._lock:
            if self._attempts:
                raise RuntimeError("model identities cannot change after the first call")
            self._identities = dict(identities)

    @contextmanager
    def role(self, value: CallRole):
        token = self._role.set(value)
        try:
            yield
        finally:
            self._role.reset(token)

    def on_chat_model_start(self, serialized, messages, *, run_id, metadata=None, **kwargs):
        metadata = metadata or {}
        callback_identity = metadata.get("ls_provider"), metadata.get("ls_model_name")
        with self._lock:
            if len(self._attempts) >= self.max_calls:
                raise CallBudgetExceeded(f"LLM call budget of {self.max_calls} exhausted")
            if run_id in self._attempts:
                raise RuntimeError("LLM call run ID has already been admitted")
            identity = self._fixed_identity
            if identity is None and all(isinstance(value, str) for value in callback_identity):
                identity = self._identities.get(callback_identity)
            provider, model = identity or (None, None)
            self._attempts[run_id] = LLMCall(
                run_id=run_id,
                provider=provider,
                model=model,
                role=self._role.get(),
                status="unfinished",
                started_at=datetime.now(UTC),
                **NO_USAGE.model_dump(),
            )

    def on_llm_end(self, response, *, run_id, **kwargs):
        self._finish(run_id, "completed", response)

    def on_llm_error(self, error, *, run_id, **kwargs):
        self._finish(run_id, "failed", kwargs.get("response"))

    def _finish(
        self, run_id: UUID, status: Literal["completed", "failed"], response
    ) -> LLMCall | None:
        with self._lock:
            call = self._attempts.get(run_id)
            if call is None or call.status != "unfinished":
                return None
            usage, standard_tariff = observe_usage(response, call.provider)
            update = usage.model_dump() | {"status": status}
            if not standard_tariff:
                update.update(provider=None, model=None)
            finished = call.model_copy(update=update)
            self._attempts[run_id] = finished
            return finished

    def usage_report(self, source: CaptureSource, *, complete: bool) -> LLMUsageReport:
        with self._lock:
            calls = tuple(self._attempts.values())
        complete = complete and all(call.status != "unfinished" for call in calls)
        return LLMUsageReport(
            schema_version="0.1",
            source_mode=source,
            llm_calls=len(calls),
            capture_status="complete" if complete else "partial",
            calls=calls,
        )

    def summary(self) -> dict:
        with self._lock:
            calls = tuple(self._attempts.values())
        complete = [
            call
            for call in calls
            if call.input_tokens is not None and call.output_tokens is not None
        ]
        return {
            "llm_calls": len(calls),
            "input_tokens": sum(call.input_tokens for call in complete),
            "output_tokens": sum(call.output_tokens for call in complete),
            "missing_usage_calls": len(calls) - len(complete),
            "usd_cost": None,
        }


@contextmanager
def capture_usage(path: Path | None, budget: CallBudget, source: CaptureSource):
    if path is None:
        yield
        return
    if budget.max_calls > MAX_CAPTURE_CALLS:
        raise ValueError("max_calls exceeds the usage capture limit")
    # Claim the destination before invoking a model. An interrupted write is
    # distinguishable from valid evidence by the strict document parser.
    with path.open("x", encoding="utf-8") as destination:
        complete = False
        try:
            yield
            complete = True
        finally:
            destination.write(
                budget.usage_report(source, complete=complete).model_dump_json(indent=2)
            )
            destination.write("\n")
