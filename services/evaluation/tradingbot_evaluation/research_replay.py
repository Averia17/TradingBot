"""Replay durably accepted research proposals against a pinned synthetic equity tape."""

import json
import re
from collections import defaultdict
from dataclasses import dataclass
from datetime import datetime, timedelta
from decimal import ROUND_CEILING, Decimal, localcontext
from hashlib import sha256
from math import isfinite
from typing import Annotated, Literal

import msgspec

from tradingbot_evaluation.replay import (
    EPOCH,
    MAX_PREFIX_EVENTS,
    MAX_PREFIX_SYMBOLS,
    Order,
    Quote,
    Replay,
    _nanoseconds,
    _unique_fields,
    load_replay,
    replay_prefix,
    run_replay,
)

MAX_RESEARCH_EXPORT_BYTES = 48 * 1024 * 1024
_MAX_RESEARCH_RECORD_BYTES = 32 * 1024 * 1024
_TIME = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)")
_NATIVE_SYMBOL = re.compile(r"[A-Z][A-Z0-9.]{0,11}")
_CENT = Decimal("0.01")
_BASIS_POINTS = Decimal(10_000)

_UUID = Annotated[
    str, msgspec.Meta(pattern=r"^[a-fA-F0-9]{8}(?:-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}$")
]
_JOB_ID = Annotated[str, msgspec.Meta(pattern=r"^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$")]
_RECEIPT_TIME = Annotated[
    str,
    msgspec.Meta(pattern=r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,6})?Z$"),
]
_HASH = Annotated[str, msgspec.Meta(pattern=r"^[a-f0-9]{64}$")]
_AMOUNT = Annotated[str, msgspec.Meta(pattern=r"^(0|[1-9][0-9]{0,11})(\.[0-9]{1,8})?$")]
_SYMBOL = Annotated[str, msgspec.Meta(pattern=r"^[A-Z][A-Z0-9.-]{0,15}$")]
_ATTEMPT = Annotated[int, msgspec.Meta(ge=1, le=3)]
_BUDGET = Annotated[int, msgspec.Meta(ge=1, le=64)]
_NONEMPTY = Annotated[str, msgspec.Meta(min_length=1)]
_ID = Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")]


class _Record(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    job_id: _JOB_ID
    attempt: _ATTEMPT
    created_at: _RECEIPT_TIME
    completed_at: _RECEIPT_TIME
    request_base64: bytes
    request_sha256: _HASH
    result_base64: bytes
    result_sha256: _HASH


class _Export(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    jobs: Annotated[tuple[_Record, ...], msgspec.Meta(min_length=1, max_length=32)]


class _Manifest(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    ref: Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$")]
    sha256: _HASH


class _Position(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    symbol: _SYMBOL
    instrument_id: _NONEMPTY
    quantity: _AMOUNT
    mark_price_usd: _AMOUNT


class _Portfolio(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    snapshot_id: _UUID
    as_of: str
    currency: Literal["USD"]
    cash_asset: Literal["USD", "USDC"]
    cash_usd: _AMOUNT
    reserved_cash_usd: _AMOUNT
    max_position_pct: _AMOUNT
    positions: tuple[_Position, ...]


class _Request(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    job_schema_version: Literal["0.1"]
    job_id: _UUID
    portfolio: _Portfolio
    requested_symbols: Annotated[tuple[_SYMBOL, ...], msgspec.Meta(min_length=1, max_length=100)]
    data_manifest: _Manifest
    policy_version: _ID
    max_llm_calls: _BUDGET


class _Horizon(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    min_hours: Annotated[int, msgspec.Meta(gt=0)]
    max_hours: Annotated[int, msgspec.Meta(gt=0)]


class _Intent(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    symbol: _SYMBOL
    action: Literal["BUY", "SELL", "HOLD", "ADD", "REDUCE"]
    notional_usd: _AMOUNT
    confidence: Annotated[float, msgspec.Meta(ge=0, le=1)] | None
    expected_return_pct: float | None
    horizon: _Horizon
    thesis: Annotated[tuple[_NONEMPTY, ...], msgspec.Meta(min_length=1)]
    invalidation_conditions: Annotated[tuple[_NONEMPTY, ...], msgspec.Meta(min_length=1)]
    evidence_ids: tuple[_NONEMPTY, ...]
    max_execution_cost_bps: Annotated[int, msgspec.Meta(ge=0, le=1000)]
    decision_id: _UUID


class _Batch(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    execution_mode: Literal["research"]
    run_id: _UUID
    portfolio_snapshot_id: _UUID
    as_of: str
    created_at: str
    expires_at: str
    evidence_hashes: dict[_NONEMPTY, _HASH]
    intents: Annotated[tuple[_Intent, ...], msgspec.Meta(min_length=1, max_length=100)]


class _FixtureMetadata(
    msgspec.Struct,
    frozen=True,
    forbid_unknown_fields=True,
    tag="synthetic_fixture",
    tag_field="source_mode",
):
    alpha_measured: Literal[False]
    llm_calls: Literal[0]


class _Provenance(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    job_id: _UUID
    attempt: _ATTEMPT
    policy_version: _ID
    data_manifest: _Manifest
    worker_version: _ID
    upstream_version: _ID
    upstream_sha: Annotated[str, msgspec.Meta(pattern=r"^[a-f0-9]{40}$")]
    config_sha256: _HASH
    model_id: Literal["scripted-offline-v1"]


class _OfflineMetadata(
    msgspec.Struct,
    frozen=True,
    forbid_unknown_fields=True,
    tag="tradingagents_offline",
    tag_field="source_mode",
):
    alpha_measured: Literal[False]
    intraday_pit_verified: Literal[False]
    llm_calls: _BUDGET
    input_tokens: int
    output_tokens: int
    missing_usage_calls: Annotated[int, msgspec.Meta(ge=0, le=64)]
    usd_cost: None
    provenance: _Provenance


class _Result(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    result_schema_version: Literal["0.1", "0.2"]
    batch: _Batch
    reports: dict[_SYMBOL, dict[_NONEMPTY, _NONEMPTY]]
    metadata: _FixtureMetadata | _OfflineMetadata


@dataclass(frozen=True)
class PreparedResearchReplay:
    replay: Replay
    export_sha256: str
    tape_sha256: str
    jobs: tuple[dict, ...]
    decisions: tuple[dict, ...]
    validated_evaluation: dict | None


def _reject_nonfinite(_value: str):
    raise ValueError("Nonfinite JSON numbers are unsupported")


def _decode(data: bytes, cls):
    return msgspec.convert(
        json.loads(data, object_pairs_hook=_unique_fields, parse_constant=_reject_nonfinite),
        type=cls,
    )


def _time_ns(value: str) -> int:
    match = _TIME.fullmatch(value) if isinstance(value, str) else None
    if match is None:
        raise ValueError("Research replay requires exact RFC 3339 timestamps")
    whole, fraction, offset = match.groups()
    if offset != "Z" and (int(offset[1:3]) > 23 or int(offset[4:6]) > 59):
        raise ValueError("Invalid research replay timestamp offset")
    base = _nanoseconds(datetime.fromisoformat(whole + offset))
    return base + int((fraction or "").ljust(9, "0"))


def _floor_microseconds(ns: int) -> datetime:
    return EPOCH + timedelta(microseconds=ns // 1000)


def _at(at: datetime) -> str:
    return at.isoformat().replace("+00:00", "Z")


def _pinned(data: bytes, expected: str, label: str) -> str:
    digest = sha256(data).hexdigest()
    if digest != expected:
        raise ValueError(f"Research {label} SHA-256 does not match")
    return digest


def _validate_record(record: _Record, request: _Request, result: _Result) -> tuple[int, int, int]:
    if record.job_id != request.job_id:
        raise ValueError("Research job ID does not match the stored request")

    metadata = result.metadata
    if isinstance(metadata, _OfflineMetadata):
        provenance = metadata.provenance
        if (
            result.result_schema_version != "0.2"
            or provenance.job_id != record.job_id
            or provenance.attempt != record.attempt
            or provenance.policy_version != request.policy_version
            or provenance.data_manifest != request.data_manifest
            or not metadata.missing_usage_calls <= metadata.llm_calls <= request.max_llm_calls
            or not all(
                0 <= count < 2**64 for count in (metadata.input_tokens, metadata.output_tokens)
            )
            or (
                metadata.missing_usage_calls == metadata.llm_calls
                and (metadata.input_tokens or metadata.output_tokens)
            )
        ):
            raise ValueError("Offline result provenance or job budget differs from stored request")
    elif result.result_schema_version != "0.1":
        raise ValueError("Unsupported research result metadata and version")

    batch = result.batch
    cutoff = _time_ns(batch.as_of)
    completed = _time_ns(record.completed_at)
    expiry = _time_ns(batch.expires_at)
    if (
        batch.portfolio_snapshot_id.lower() != request.portfolio.snapshot_id.lower()
        or cutoff != _time_ns(request.portfolio.as_of)
    ):
        raise ValueError("Research result does not match the stored portfolio snapshot")
    if not (
        cutoff <= _time_ns(record.created_at) <= completed
        and cutoff <= _time_ns(batch.created_at) <= completed < expiry
    ):
        raise ValueError("Invalid research snapshot, completion or decision time")

    symbols = set(request.requested_symbols)
    if (
        len(symbols) != len(request.requested_symbols)
        or len(batch.intents) != len(symbols)
        or {intent.symbol for intent in batch.intents} != symbols
        or result.reports.keys() != symbols
    ):
        raise ValueError("Research result does not cover the exact requested symbols")
    fields = {
        "market_report",
        "fundamentals_report",
        "news_report",
        "sentiment_report",
        "trader_investment_plan",
        "final_trade_decision",
    }
    hashes = {}
    for symbol, reports in result.reports.items():
        if "final_trade_decision" not in reports or reports.keys() - fields:
            raise ValueError("Invalid stored report fields")
        for field, text in reports.items():
            if not text.strip():
                raise ValueError("Empty stored research report")
            hashes[f"{symbol}:{field}"] = sha256(text.encode()).hexdigest()
    if batch.evidence_hashes != hashes:
        raise ValueError("Research report hashes do not match the stored evidence")
    for intent in batch.intents:
        if (
            not set(intent.evidence_ids) <= hashes.keys()
            or intent.horizon.min_hours > intent.horizon.max_hours
            or any(
                estimate is not None and not isfinite(estimate)
                for estimate in (intent.confidence, intent.expected_return_pct)
            )
            or (
                Decimal(intent.notional_usd) != 0
                if intent.action == "HOLD"
                else (
                    Decimal(intent.notional_usd) <= 0
                    or intent.confidence is None
                    or intent.expected_return_pct is None
                    or not intent.evidence_ids
                )
            )
        ):
            raise ValueError("Invalid durable research intent")
    return cutoff, completed, expiry


def _load_tape(raw: bytes) -> Replay:
    tape = load_replay(raw)
    if tape.orders:
        raise ValueError("A research replay tape must have empty orders")
    # Nautilus adapter accepts Python datetimes. Ensure that decoding the tape
    # did not round a quote or corporate event onto either side of a boundary.
    wire = json.loads(raw)
    for name, time_fields in (
        ("quotes", ("observed_at", "available_at")),
        ("cash_dividends", ("available_at", "entitlement_at", "paid_at")),
        ("stock_splits", ("available_at", "effective_at")),
    ):
        for row in wire.get(name, ()):
            if any(_time_ns(row[field]) % 1000 for field in time_fields):
                raise ValueError("Synthetic replay tape timestamps require microsecond precision")
    return tape


@dataclass(frozen=True)
class _Window:
    cutoff: datetime
    completed: datetime
    expiry: datetime


def _reference_quote(quotes: list[Quote], splits: list[datetime], cutoff: datetime) -> Quote | None:
    mark = max(
        (q for q in quotes if q.available_at <= cutoff),
        default=None,
        key=lambda q: (q.observed_at, q.available_at),
    )
    if mark is None or any(mark.observed_at < at <= cutoff for at in splits):
        return None
    return mark


def _midpoint(quote: Quote) -> Decimal:
    return (Decimal(quote.bid_usd) + Decimal(quote.ask_usd)) / 2


def _matching_order(
    intent: _Intent,
    held: _Position | None,
    quotes: list[Quote],
    splits: list[datetime],
    window: _Window,
    commission_bps: int,
    audit: dict,
) -> tuple[Order | None, str]:
    if intent.action == "HOLD":
        return None, "hold"
    symbol = intent.symbol
    if not _NATIVE_SYMBOL.fullmatch(symbol) or (
        held is not None and held.instrument_id != f"fixture:{symbol}"
    ):
        return None, "unsupported_instrument"
    if intent.action != "BUY" and held is None:
        return None, "missing_held_snapshot_position"
    if intent.action == "BUY" and held is not None:
        return None, "position_already_held"
    if window.expiry <= window.completed:
        return None, "expired"
    execution = next((q for q in quotes if window.completed < q.observed_at), None)
    if execution is None:
        return None, "no_executable_quote"
    if window.expiry <= execution.available_at:
        return None, "expired"
    audit["execution_quote_observed_at"] = _at(execution.observed_at)
    audit["execution_quote_available_at"] = _at(execution.available_at)
    if any(window.cutoff < at <= execution.available_at for at in splits):
        return None, "cancelled_stock_split"

    if held is None:
        mark = _reference_quote(quotes, splits, window.cutoff)
        if mark is None:
            return None, "unavailable_reference_mark"
        reference = _midpoint(mark)
        audit["reference_quote_available_at"] = _at(mark.available_at)
    else:
        reference = Decimal(held.mark_price_usd)
    notional = Decimal(intent.notional_usd)
    buying = intent.action in {"BUY", "ADD"}
    price = Decimal(execution.ask_usd if buying else execution.bid_usd)
    audit["reference_price_usd"] = format(reference, "f")
    audit["execution_quote_price_usd"] = format(price, "f")
    # Reserve worst cash required at the first quote and cap size by the
    # decision's reference notional. Round the modeled cost conservatively
    # per share so even a partial fill stays within the requested cost cap.
    fee = (price * commission_bps / _BASIS_POINTS).quantize(_CENT, rounding=ROUND_CEILING)
    adverse = price - reference if buying else reference - price
    modeled_cost = max(adverse, Decimal(0)) + fee
    audit["modeled_cost_ceiling_per_share_usd"] = format(modeled_cost, "f")
    if modeled_cost * _BASIS_POINTS > reference * intent.max_execution_cost_bps:
        return None, "rejected_execution_cost"
    if buying:
        quantity = int(notional // max(price, reference))
    else:
        held_quantity = Decimal(held.quantity)
        if held_quantity != held_quantity.to_integral_value():
            return None, "unsupported_held_quantity"
        if intent.action == "SELL" and held_quantity * reference != notional:
            return None, "size_mismatch"
        quantity = int(notional // reference)
        if quantity > held_quantity or (intent.action == "REDUCE" and quantity == held_quantity):
            return None, "size_mismatch"
    if not 1 <= quantity <= 1_000_000:
        return None, "unsupported_order_quantity"

    audit["quantity"] = quantity
    return Order(
        intent.decision_id.lower(),
        symbol,
        "BUY" if buying else "SELL",
        quantity,
        window.completed,
        window.expiry,
    ), "submitted_to_replay"


def _verify_snapshots(
    replay: Replay,
    snapshots: list[tuple[_Portfolio, datetime]],
    jobs: list[dict],
    timeline: dict[str, list[Quote]],
    splits: dict[str, list[datetime]],
) -> None:
    if (
        len(timeline) > MAX_PREFIX_SYMBOLS
        or len(replay.quotes)
        + len(replay.orders)
        + len(replay.cash_dividends)
        + len(replay.stock_splits)
        > MAX_PREFIX_EVENTS
    ):
        raise ValueError(
            f"Snapshot continuity is bounded to {MAX_PREFIX_SYMBOLS} symbols "
            f"and {MAX_PREFIX_EVENTS} replay events"
        )
    if len({portfolio.cash_asset for portfolio, _ in snapshots}) != 1:
        raise ValueError("Snapshot continuity requires a consistent cash asset")

    states = {}
    for (portfolio, cutoff), job in zip(snapshots, jobs, strict=True):
        description = f"Job {job['job_id']} snapshot"
        if Decimal(portfolio.reserved_cash_usd):
            raise ValueError(f"{description} has reserved cash outside the synthetic account")
        if cutoff not in states:
            states[cutoff] = replay_account_at(replay, cutoff)
        cash, positions = states[cutoff]
        if Decimal(portfolio.cash_usd) != Decimal(cash):
            raise ValueError(f"{description} cash differs from the native replay at its cutoff")
        for position in portfolio.positions:
            if position.instrument_id != f"fixture:{position.symbol}":
                raise ValueError(f"{description} uses an unsupported held instrument")
        if {pos.symbol: Decimal(pos.quantity) for pos in portfolio.positions} != {
            symbol: Decimal(quantity) for symbol, quantity in positions.items()
        }:
            raise ValueError(f"{description} positions differ from the native replay at its cutoff")
        for position in portfolio.positions:
            quote = _reference_quote(timeline[position.symbol], splits[position.symbol], cutoff)
            if quote is None or Decimal(position.mark_price_usd) != _midpoint(quote):
                raise ValueError(
                    f"{description} held mark differs from the pinned point-in-time quote"
                )
        job["snapshot_continuity"] = {"cash_usd": cash, "positions": positions}


def replay_account_at(replay: Replay, cutoff: datetime) -> tuple[str, dict[str, str]]:
    prefix = replay_prefix(replay, cutoff)
    if not prefix.quotes:
        return format(Decimal(replay.starting_cash_usd), ".2f"), {}
    result = run_replay(prefix, through=cutoff)
    return result["ending_cash_usd"], result["open_positions"]


def prepare_research_replay(
    export_raw: bytes,
    tape_raw: bytes,
    *,
    expected_export_sha256: str,
    expected_tape_sha256: str,
    require_snapshot_continuity: bool = False,
) -> PreparedResearchReplay:
    if len(export_raw) > MAX_RESEARCH_EXPORT_BYTES:
        raise ValueError("Research export exceeds the input limit")
    export_hash = _pinned(export_raw, expected_export_sha256, "export")
    tape_hash = _pinned(tape_raw, expected_tape_sha256, "tape")
    export = _decode(export_raw, _Export)
    return _prepare_records(
        export.jobs,
        _load_tape(tape_raw),
        export_hash,
        tape_hash,
        require_snapshot_continuity=require_snapshot_continuity,
    )


def _prepare_records(
    records: tuple[_Record, ...],
    tape: Replay,
    export_hash: str,
    tape_hash: str,
    *,
    require_snapshot_continuity: bool = False,
) -> PreparedResearchReplay:
    decoded_bytes = sum(len(row.request_base64) + len(row.result_base64) for row in records)
    if decoded_bytes > _MAX_RESEARCH_RECORD_BYTES:
        raise ValueError("Decoded research export records exceed the input limit")
    timeline, splits = _tape_timeline(tape)

    issued_orders = []
    jobs = []
    snapshots = []
    decisions = []
    seen_jobs = set()
    seen_decisions = set()
    previous_key = None
    for record in records:
        if record.job_id.lower() in seen_jobs:
            raise ValueError("Research export contains a duplicate job")
        seen_jobs.add(record.job_id.lower())
        _pinned(record.request_base64, record.request_sha256, "request")
        _pinned(record.result_base64, record.result_sha256, "result")
        request = _decode(record.request_base64, _Request)
        result = _decode(record.result_base64, _Result)
        cutoff, completed, expiry = _validate_record(record, request, result)
        window = _Window(*map(_floor_microseconds, (cutoff, completed, expiry)))
        job_key = (completed, record.job_id)
        if previous_key is not None and job_key <= previous_key:
            raise ValueError("Research export jobs must be in completion order")
        previous_key = job_key
        portfolio = request.portfolio
        held = {position.symbol: position for position in portfolio.positions}
        if len(held) != len(portfolio.positions):
            raise ValueError("Research snapshot contains a duplicate held symbol")
        if (
            not 0 < Decimal(portfolio.max_position_pct) <= 1
            or Decimal(portfolio.reserved_cash_usd) > Decimal(portfolio.cash_usd)
            or any(
                Decimal(pos.quantity) <= 0 or Decimal(pos.mark_price_usd) <= 0
                for pos in portfolio.positions
            )
        ):
            raise ValueError("Research snapshot contains invalid cash, limits or positions")

        jobs.append(
            {
                "job_id": record.job_id,
                "attempt": record.attempt,
                "job_created_at": record.created_at,
                "completed_at": record.completed_at,
                "request_sha256": record.request_sha256,
                "result_sha256": record.result_sha256,
                "snapshot_id": portfolio.snapshot_id,
                "snapshot_as_of": portfolio.as_of,
                "snapshot_cash_asset": portfolio.cash_asset,
                "decision_run_id": result.batch.run_id,
                "decision_created_at": result.batch.created_at,
                "decision_expires_at": result.batch.expires_at,
                "data_manifest": msgspec.to_builtins(request.data_manifest),
                "policy_version": request.policy_version,
                "research_metadata": msgspec.to_builtins(result.metadata),
            }
        )
        snapshots.append((portfolio, window.cutoff))
        for intent in result.batch.intents:
            decision_id = intent.decision_id.lower()
            if decision_id in seen_decisions:
                raise ValueError("Research export contains a duplicate decision")
            seen_decisions.add(decision_id)
            audit = {
                "job_id": record.job_id,
                "decision_id": decision_id,
                "symbol": intent.symbol,
                "action": intent.action,
                "requested_notional_usd": intent.notional_usd,
                "max_execution_cost_bps": intent.max_execution_cost_bps,
            }
            with localcontext(prec=64):
                order, status = _matching_order(
                    intent,
                    held.get(intent.symbol),
                    timeline[intent.symbol],
                    splits[intent.symbol],
                    window,
                    tape.commission_bps,
                    audit,
                )
            if order is not None:
                issued_orders.append(order)
            decisions.append(audit | {"status": status})

    replay = msgspec.structs.replace(tape, orders=tuple(issued_orders))
    evaluation = None
    if require_snapshot_continuity:
        # An order cannot execute before the cutoff of its own research job.
        # Running each prefix also handles analyses with overlapping clocks.
        _verify_snapshots(replay, snapshots, jobs, timeline, splits)
        horizon = max(
            max(quote.available_at for quote in replay.quotes),
            max(cutoff for _, cutoff in snapshots),
        )
        evaluation = run_replay(replay, through=horizon)
        _verify_execution_quotes(evaluation["fills"], tuple(decisions))
        _verify_execution_state(evaluation, jobs, decisions)
    return PreparedResearchReplay(
        replay,
        export_hash,
        tape_hash,
        tuple(jobs),
        tuple(decisions),
        evaluation,
    )


def _tape_timeline(tape: Replay) -> tuple[dict[str, list[Quote]], dict[str, list[datetime]]]:
    timeline = defaultdict(list)
    for quote in sorted(tape.quotes, key=lambda q: q.available_at):
        timeline[quote.symbol].append(quote)
    splits = defaultdict(list)
    for action in tape.stock_splits:
        splits[action.symbol].append(action.effective_at)
    return timeline, splits


def run_research_replay(
    export_raw: bytes,
    tape_raw: bytes,
    *,
    expected_export_sha256: str,
    expected_tape_sha256: str,
    require_snapshot_continuity: bool = False,
) -> dict:
    prepared = prepare_research_replay(
        export_raw,
        tape_raw,
        expected_export_sha256=expected_export_sha256,
        expected_tape_sha256=expected_tape_sha256,
        require_snapshot_continuity=require_snapshot_continuity,
    )
    verified = prepared.validated_evaluation is not None
    evaluation = prepared.validated_evaluation
    if evaluation is None:
        evaluation = run_replay(prepared.replay)
        _verify_execution_quotes(evaluation["fills"], prepared.decisions)
    return evaluation | {
        "research_replay_schema_version": "0.1",
        "intent_pipeline_connected": True,
        "research_export_sha256": prepared.export_sha256,
        "synthetic_tape_sha256": prepared.tape_sha256,
        "snapshot_continuity_verified": verified,
        "replay_portfolio_mode": (
            "verified_synthetic_cash_account" if verified else "independent_synthetic_cash_account"
        ),
        "research_jobs": list(prepared.jobs),
        "decisions": [
            row | {"status": evaluation["intents"].get(row["decision_id"], row["status"])}
            for row in prepared.decisions
        ],
    }


def _verify_execution_state(evaluation: dict, jobs: list[dict], decisions: list[dict]) -> None:
    cutoffs = {job["job_id"]: _time_ns(job["snapshot_as_of"]) for job in jobs}
    owners = {row["decision_id"]: row["job_id"] for row in decisions}
    changes = [
        (int(fill["quote_available_at_ns"]), owners[fill["intent_id"]])
        for fill in evaluation["fills"]
    ]
    changes.extend(
        (int(row["paid_at_ns"]), None)
        for row in evaluation.get("cash_dividend_events", ())
        if row["status"] == "paid" and Decimal(row["cash_credited_usd"])
    )
    changes.extend(
        (int(row["effective_at_ns"]), None)
        for row in evaluation.get("stock_split_events", ())
        if row["shares_before"] != row["shares_after"]
    )
    for row in decisions:
        if row["status"] != "submitted_to_replay":
            continue
        job = row["job_id"]
        execution = _time_ns(row["execution_quote_available_at"])
        if any(
            cutoffs[job] < instant <= execution and (owner != job or instant != execution)
            for instant, owner in changes
        ):
            raise ValueError(f"Job {job} snapshot became stale before execution")


def _verify_execution_quotes(fills: list[dict], decisions: tuple[dict, ...]) -> None:
    by_id = {row["decision_id"]: row for row in decisions}
    for fill in fills:
        row = by_id[fill["intent_id"]]
        if int(fill["quote_available_at_ns"]) != _time_ns(
            row["execution_quote_available_at"]
        ) or Decimal(fill["price_usd"]) != Decimal(row["execution_quote_price_usd"]):
            raise RuntimeError("Native research replay fill does not match its selected quote")
