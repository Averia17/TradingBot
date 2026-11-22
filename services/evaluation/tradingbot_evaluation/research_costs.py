"""Reprice archived research invocations under an independently pinned USD schedule."""

import json
import re
from collections import defaultdict
from dataclasses import dataclass
from datetime import UTC, date, datetime, time, timedelta
from decimal import ROUND_CEILING, Decimal, localcontext
from hashlib import sha256
from typing import Annotated, Literal
from uuid import UUID

import msgspec

from tradingbot_evaluation.forward_paper import PinnedArtifact
from tradingbot_evaluation.operating_costs import (
    USD,
    CostCoverage,
    Expense,
    OperatingCosts,
    load_operating_costs,
)
from tradingbot_evaluation.replay import IntentID, Timestamp, _unique_fields

MAX_CALLS = 8192
MAX_ARCHIVES = 64
MAX_CALLS_PER_ARCHIVE = 4096
MAX_PERMITS_PER_JOB = 64
MAX_CALLBACK_BYTES = 4096
TokenCount = Annotated[int, msgspec.Meta(ge=0, le=1_000_000_000)]
ModelKey = Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9_.:/@-]{0,191}$")]
_PREPARED_MODEL_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")


class ResearchInvocation(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    run_id: UUID
    provider: ModelKey | None
    model: ModelKey | None
    role: Literal["research", "portfolio_allocator", "unknown"]
    status: Literal["completed", "failed", "unfinished"]
    started_at: Timestamp
    input_tokens: TokenCount | None
    output_tokens: TokenCount | None
    cache_read_tokens: TokenCount | None
    cache_write_tokens: TokenCount | None
    reasoning_tokens: TokenCount | None
    usage_source: Literal["langchain_usage_metadata", "unavailable"]


class ResearchUsage(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture", "provider_reported"]
    llm_calls: Annotated[int, msgspec.Meta(ge=0, le=MAX_CALLS_PER_ARCHIVE)]
    capture_status: Literal["complete", "partial"]
    calls: Annotated[tuple[ResearchInvocation, ...], msgspec.Meta(max_length=MAX_CALLS_PER_ARCHIVE)]


class ResearchPermit(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    call_id: UUID
    attempt: Annotated[int, msgspec.Meta(ge=1, le=2**31 - 1)]
    granted_at: Timestamp
    callback_payload: (
        Annotated[bytes, msgspec.Meta(min_length=1, max_length=MAX_CALLBACK_BYTES)] | None
    )
    callback_sha256: Annotated[str, msgspec.Meta(pattern=r"^[0-9a-f]{64}$")] | None
    recorded_at: Timestamp | None


class ResearchPermitArchive(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture", "provider_reported"]
    job_id: UUID
    job_status: Literal["queued", "leased", "completed", "failed", "expired"]
    attempts: Annotated[int, msgspec.Meta(ge=0, le=2**31 - 1)]
    legacy_budget_unknown: bool
    permit_history_known: bool
    permits: Annotated[tuple[ResearchPermit, ...], msgspec.Meta(max_length=MAX_PERMITS_PER_JOB)]


@dataclass(frozen=True)
class PricedUsageInput:
    source_mode: str
    capture_status: str
    calls: tuple[tuple[ResearchInvocation | None, ResearchPermit | None], ...]
    permit_provenance: dict | None = None


class ModelRate(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    rate_id: IntentID
    provider: ModelKey
    model: ModelKey
    valid_from: date
    end_exclusive: date
    uncached_input_usd_per_million: USD
    cache_read_usd_per_million: USD
    cache_write_usd_per_million: USD
    output_usd_per_million: USD
    reasoning_usd_per_million: USD
    fixed_provider_fee_usd: USD | None


class ResearchRates(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture", "archived_schedule"]
    currency: Literal["USD"]
    rates: Annotated[tuple[ModelRate, ...], msgspec.Meta(min_length=1, max_length=256)]


def _load(artifact: PinnedArtifact, contract: type, description: str):
    return msgspec.convert(
        json.loads(artifact.read(description), object_pairs_hook=_unique_fields), type=contract
    )


def _rate_index(rates: ResearchRates) -> dict[tuple[str, str], list[ModelRate]]:
    index = defaultdict(list)
    unique = set()
    for rate in sorted(rates.rates, key=lambda item: item.valid_from):
        group = index[rate.provider, rate.model]
        if rate.rate_id in unique or rate.valid_from >= rate.end_exclusive:
            raise ValueError("Rate IDs must be unique with nonempty UTC date ranges")
        if group and group[-1].end_exclusive > rate.valid_from:
            raise ValueError("Rate windows overlap for the same provider and model")
        unique.add(rate.rate_id)
        group.append(rate)
    return index


def _validate_call(call: ResearchInvocation) -> None:
    if call.started_at.utcoffset() != timedelta(0):
        raise ValueError("Research invocation time must be UTC")
    if (call.provider is None) != (call.model is None):
        raise ValueError("Provider and model identity must be present together")
    reads, writes, reasoning = (
        call.cache_read_tokens,
        call.cache_write_tokens,
        call.reasoning_tokens,
    )
    unavailable = call.input_tokens is None and call.output_tokens is None
    if unavailable != (call.usage_source == "unavailable") or (
        call.status == "unfinished" and not unavailable
    ):
        raise ValueError("Research usage source or invocation status disagrees with token totals")
    for total, parts in ((call.input_tokens, (reads, writes)), (call.output_tokens, (reasoning,))):
        known = [count for count in parts if count is not None]
        if known and (total is None or sum(known) > total):
            raise ValueError("Token categories exceed or lack their inclusive token total")


def _utc_text(value: datetime) -> str:
    return value.isoformat().replace("+00:00", "Z")


def _load_usage(artifact: PinnedArtifact) -> PricedUsageInput:
    archive = _load(artifact, ResearchUsage, "Research usage archive")
    if archive.llm_calls != len(archive.calls):
        raise ValueError("Captured invocation count differs from its declaration")
    if archive.capture_status == "complete" and any(
        call.status == "unfinished" for call in archive.calls
    ):
        raise ValueError("Unfinished invocations require partial capture")
    return PricedUsageInput(
        archive.source_mode, archive.capture_status, tuple((call, None) for call in archive.calls)
    )


def _load_permits(artifact: PinnedArtifact) -> PricedUsageInput:
    archive = _load(artifact, ResearchPermitArchive, "Research permit archive")
    if archive.job_status in {"leased", "completed"} and archive.attempts == 0:
        raise ValueError("Research job state requires at least one recorded attempt")
    keys = tuple((permit.granted_at, permit.call_id) for permit in archive.permits)
    if keys != tuple(sorted(set(keys))):
        raise ValueError("Research permits must be ordered by UTC grant time and call ID")
    calls = []
    missing = 0
    for permit in archive.permits:
        if permit.attempt > archive.attempts or permit.granted_at.utcoffset() != timedelta(0):
            raise ValueError("Research permit disagrees with its job attempt or UTC grant time")
        raw = permit.callback_payload
        stored = permit.recorded_at
        if raw is None:
            if stored is not None or permit.callback_sha256 is not None:
                raise ValueError("Missing research callback must not have receipt fields")
            calls.append((None, permit))
            missing += 1
            continue
        if sha256(raw).hexdigest() != permit.callback_sha256:
            raise ValueError("Research callback SHA-256 does not match the stored bytes")
        if stored is None or stored.utcoffset() != timedelta(0) or stored < permit.granted_at:
            raise ValueError("Research callback time differs from its permit")
        call = msgspec.convert(
            json.loads(raw, object_pairs_hook=_unique_fields), type=ResearchInvocation
        )
        if call.run_id != permit.call_id or call.status == "unfinished":
            raise ValueError("Research callback does not match its permit")
        if archive.source_mode == "provider_reported":
            if (
                call.role != "portfolio_allocator"
                or call.provider not in {"openai", "chatgpt"}
                or call.model is None
                or _PREPARED_MODEL_ID.fullmatch(call.model) is None
                or call.model == "scripted-offline-v1"
            ):
                raise ValueError("Research callback does not match its provider permit")
        elif (call.provider, call.model) not in {(None, None), ("offline", "scripted-offline-v1")}:
            raise ValueError("Research callback does not match its offline permit")
        calls.append((call, permit))
    complete = (
        not missing
        and archive.job_status == "completed"
        and not archive.legacy_budget_unknown
        and archive.permit_history_known
    )
    return PricedUsageInput(
        archive.source_mode,
        "complete" if complete else "partial",
        tuple(calls),
        {
            "job_id": str(archive.job_id),
            "job_status": archive.job_status,
            "attempts": archive.attempts,
            "legacy_budget_unknown": archive.legacy_budget_unknown,
            "permit_history_known": archive.permit_history_known,
            "permit_count": len(calls),
            "permits_without_callback": missing,
        },
    )


def _price_observation(
    call: ResearchInvocation | None,
    permit: ResearchPermit | None,
    rates: dict[tuple[str, str], list[ModelRate]],
) -> tuple[dict, Decimal | None]:
    identity = permit.call_id if permit is not None else call.run_id
    attributed_at = permit.granted_at if permit is not None else call.started_at
    row = {
        "run_id": str(identity),
        "started_at": None,
        "cost_attribution_utc": _utc_text(attributed_at),
        "status": "permit_without_callback",
        "provider": None,
        "model": None,
        "role": None,
        "rate_id": None,
        "estimated_cost_usd": None,
        "estimated_token_cost_usd": None,
        "unknown": ["missing_terminal_callback"],
    }
    if permit is not None:
        row["permit"] = {
            "attempt": permit.attempt,
            "granted_at": _utc_text(permit.granted_at),
            "callback_sha256": permit.callback_sha256,
            "recorded_at": _utc_text(permit.recorded_at)
            if permit.recorded_at is not None
            else None,
        }
    if call is None:
        return row, None
    _validate_call(call)
    day = attributed_at.date()
    rate = next(
        (
            rate
            for rate in rates.get((call.provider, call.model), ())
            if rate.valid_from <= day < rate.end_exclusive
        ),
        None,
    )
    cost, token_cost, unknown = _price_call(call, rate)
    return row | {
        "started_at": _utc_text(call.started_at),
        "status": call.status,
        "provider": call.provider,
        "model": call.model,
        "role": call.role,
        "rate_id": rate.rate_id if rate is not None else None,
        "estimated_cost_usd": _usd(cost),
        "estimated_token_cost_usd": _usd(token_cost),
        "unknown": unknown,
    }, cost


def _price_call(
    call: ResearchInvocation, rate: ModelRate | None
) -> tuple[Decimal | None, Decimal | None, list[str]]:
    if call.status == "unfinished":
        return None, None, ["unfinished_invocation"]
    if rate is None:
        problem = "rate_not_found" if call.provider and call.model else "model_identity"
        return None, None, [problem]
    unknown = []
    if call.input_tokens is None:
        unknown.append("input_tokens")
    if call.output_tokens is None:
        unknown.append("output_tokens")
    input_rate = Decimal(rate.uncached_input_usd_per_million)
    output_rate = Decimal(rate.output_usd_per_million)
    categories = (
        ("cache_read_tokens", input_rate, Decimal(rate.cache_read_usd_per_million)),
        ("cache_write_tokens", input_rate, Decimal(rate.cache_write_usd_per_million)),
        ("reasoning_tokens", output_rate, Decimal(rate.reasoning_usd_per_million)),
    )
    correction = Decimal(0)
    for field, base_rate, component_rate in categories:
        count = getattr(call, field)
        if count is None and base_rate != component_rate:
            unknown.append(field)
        elif count is not None:
            correction += count * (component_rate - base_rate)
    if unknown:
        return None, None, unknown
    token_cost = (
        call.input_tokens * input_rate + call.output_tokens * output_rate + correction
    ).scaleb(-6)
    if rate.fixed_provider_fee_usd is None:
        return None, token_cost, ["provider_fees"]
    return token_cost + Decimal(rate.fixed_provider_fee_usd), token_cost, []


def _usd(amount: Decimal | None) -> str | None:
    return None if amount is None else format(amount.normalize(), "f")


def value_research_costs(
    rate_artifact: PinnedArtifact,
    archives: tuple[PinnedArtifact, ...],
    *,
    permit_archives: tuple[PinnedArtifact, ...] = (),
) -> dict:
    inputs = (*archives, *permit_archives)
    if not 1 <= len(inputs) <= MAX_ARCHIVES:
        raise ValueError("Research valuation requires between 1 and 64 pinned usage archives")
    if len({item.sha256 for item in inputs}) != len(inputs):
        raise ValueError("Duplicate research usage archive")
    schedule = _load(rate_artifact, ResearchRates, "Research rate schedule")
    rates = _rate_index(schedule)
    source = (
        "synthetic_fixture" if schedule.source_mode == "synthetic_fixture" else "provider_reported"
    )
    rows, complete = [], []
    seen, seen_jobs = set(), set()
    with localcontext() as context:
        context.prec = 50
        known_total = Decimal(0)
        for index, artifact in enumerate(inputs):
            archive = _load_usage(artifact) if index < len(archives) else _load_permits(artifact)
            if archive.source_mode != source:
                raise ValueError("Usage source differs from the rate schedule source")
            provenance = archive.permit_provenance
            if provenance is not None:
                job_id = provenance["job_id"]
                if job_id in seen_jobs:
                    raise ValueError("Duplicate research job across permit archives")
                seen_jobs.add(job_id)
            if len(seen) + len(archive.calls) > MAX_CALLS:
                raise ValueError("Research valuation exceeds the total invocation limit")
            priced = Decimal(0)
            items = []
            for call, permit in archive.calls:
                identity = permit.call_id if permit is not None else call.run_id
                if identity in seen:
                    raise ValueError("Duplicate research invocation ID")
                seen.add(identity)
                report, cost = _price_observation(call, permit, rates)
                if cost is not None:
                    priced += cost
                items.append(report)
            known_total += priced
            value = None
            if archive.capture_status == "complete" and not any(item["unknown"] for item in items):
                value = priced
                complete.append(priced)
            row = {
                "usage_sha256": artifact.sha256,
                "capture_status": archive.capture_status,
                "estimated_cost_usd": _usd(value),
                "fully_valued_calls_cost_usd": _usd(priced),
                "calls": items,
            }
            if provenance is not None:
                row["permit_provenance"] = provenance
            rows.append(row)
        complete.sort()
        p95_position = (95 * len(complete) + 99) // 100
        return {
            "schema_version": "0.1",
            "source_mode": source,
            "currency": schedule.currency,
            "pricing_basis": "schedule_estimate",
            "rates_sha256": rate_artifact.sha256,
            "estimated_cost_usd": _usd(known_total) if len(complete) == len(rows) else None,
            "fully_valued_calls_cost_usd": _usd(known_total),
            "complete_archive_p95_usd": _usd(complete[p95_position - 1]) if complete else None,
            "complete_archive_count": len(complete),
            "actual_billed_cost_usd": None,
            "archives": rows,
        }


def synthetic_operating_costs(
    template: PinnedArtifact, series: str, valuation: dict
) -> OperatingCosts:
    base = load_operating_costs(template.read("Synthetic operating cost template"))
    if base.source_mode != "synthetic_fixture" or valuation["source_mode"] != "synthetic_fixture":
        raise ValueError(
            "Valued LLM costs require synthetic usage and an exclusively synthetic ledger"
        )
    if any(
        row.series_id == series and row.category == "llm"
        for row in (*base.coverage, *base.expenses)
    ):
        raise ValueError("Operating cost template already contains LLM coverage or expenses")
    source = msgspec.json.encode(
        [
            series,
            valuation["rates_sha256"],
            sorted(row["usage_sha256"] for row in valuation["archives"]),
        ]
    )
    reference = "llm-estimate-" + sha256(source).hexdigest()
    expenses = []
    with localcontext() as context:
        context.prec = 50
        daily = defaultdict(Decimal)
        for archive in valuation["archives"]:
            for call in archive["calls"]:
                day = date.fromisoformat(call["cost_attribution_utc"][:10])
                if not base.first_mark_day < day <= base.last_mark_day:
                    raise ValueError("Research invocation falls outside the operating cost window")
                if call["estimated_cost_usd"] is not None:
                    daily[day] += Decimal(call["estimated_cost_usd"])
        for day, amount in sorted(daily.items()):
            if amount == 0:
                continue
            expenses.append(
                Expense(
                    cost_id=f"{reference}:{day.isoformat()}",
                    series_id=series,
                    category="llm",
                    incurred_at=datetime.combine(day, time.min, tzinfo=UTC),
                    amount_usd=format(
                        amount.quantize(Decimal("0.00000001"), rounding=ROUND_CEILING), "f"
                    ),
                    reference_id=reference,
                )
            )
    status = "unknown"
    if valuation["estimated_cost_usd"] is not None:
        status = "complete" if expenses else "zero"
    result = msgspec.structs.replace(
        base,
        expenses=(*base.expenses, *expenses),
        coverage=(*base.coverage, CostCoverage(series, "llm", status, reference)),
    )
    return load_operating_costs(msgspec.json.encode(result))
