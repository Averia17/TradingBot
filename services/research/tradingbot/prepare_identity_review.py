"""Prepare a frozen stable paper review request using local Go receipts."""

import json
import subprocess
from argparse import Namespace
from datetime import datetime, timedelta
from decimal import Decimal
from fractions import Fraction
from pathlib import Path
from typing import Annotated, Literal

from pydantic import Field, JsonValue, TypeAdapter

from tradingbot.contracts import DECIMAL_PATTERN, Contract, Horizon, PortfolioSnapshot, Symbol
from tradingbot.paper_cli import (
    MAX_DOCUMENT_BYTES,
    GoRunner,
    check_owner_quote_tape,
    invoke_paper_cli,
    read_paper_snapshot,
)
from tradingbot.paper_memory import USD, LedgerID, PaperInstrument, RawQuantity
from tradingbot.security_identity import (
    SecurityBinding,
    _resolve_entry,
    load_identity_registry,
    load_identity_snapshot,
)
from tradingbot.snapshots import (
    SHA256,
    ArchiveReference,
    CaptureTime,
    decode_pinned_json,
)

_Clock = TypeAdapter(CaptureTime)
_UUID = TypeAdapter(LedgerID)
_Budget = TypeAdapter(Annotated[int, Field(strict=True, ge=1, le=64)])
_Quantity = Annotated[str, Field(pattern=r"^(?:0|[1-9][0-9]{0,37})$")]
_ContextVersion = Annotated[int, Field(strict=True, ge=1, le=2**63 - 1)]
_PortfolioAmount = TypeAdapter(Annotated[str, Field(pattern=DECIMAL_PATTERN)])
_PositionCap = TypeAdapter(Annotated[int, Field(strict=True, ge=1, le=10000)])
_MarkAge = TypeAdapter(Annotated[int, Field(strict=True, ge=1, le=3600)])


class _OpeningIdentity(SecurityBinding):
    instrument: PaperInstrument
    identity_registry: ArchiveReference
    application_sha256: SHA256


class _ClockReview(Contract):
    trigger_id: LedgerID
    kind: Literal["review"]
    reason: Literal["cadence", "expiry"]
    at: CaptureTime


class _Review(Contract):
    paper_thesis_schema_version: Literal["0.2"]
    account_id: LedgerID
    account_version: _ContextVersion
    as_of: CaptureTime
    research_job_id: LedgerID
    decision_id: LedgerID
    opening_order_id: LedgerID
    instrument: PaperInstrument
    symbol: Symbol
    state: Literal["open"]
    opening_order_state: Literal["reserved", "unknown", "partially_filled", "cancelled", "filled"]
    first_fill_event_id: LedgerID
    opened_at: CaptureTime
    next_review_at: CaptureTime | None = None
    thesis_expires_at: CaptureTime
    horizon: Horizon
    source_quantity_raw: RawQuantity
    held_quantity_raw: RawQuantity
    reserved_quantity_raw: _Quantity
    free_quantity_raw: _Quantity
    cash_usd: USD
    reserved_cash_usd: USD
    clock_review: _ClockReview | None = None
    opening_identity: _OpeningIdentity


class _Rule(Contract):
    instrument: PaperInstrument
    symbol: Symbol
    group: str


class _Mark(Contract):
    instrument: PaperInstrument
    price_usd: USD
    at: CaptureTime
    available_at: CaptureTime


_Rules = TypeAdapter(Annotated[list[_Rule], Field(min_length=1, max_length=100)])
_Marks = TypeAdapter(Annotated[list[_Mark], Field(min_length=1, max_length=100)])


class _SnapshotContext(Contract):
    account_id: LedgerID
    snapshot_version: _ContextVersion
    policy: dict[str, JsonValue]
    marks: Annotated[list[_Mark], Field(min_length=1, max_length=100)]


class _Snapshot(Contract):
    snapshot_context: _SnapshotContext
    portfolio: dict[str, JsonValue]


def _read_document(source: Path | bytes) -> bytes:
    if isinstance(source, bytes):
        raw = source
    else:
        if not source.is_file():
            raise ValueError("review inputs must be local regular files")
        with source.open("rb") as stream:
            raw = stream.read(MAX_DOCUMENT_BYTES + 1)
    if len(raw) > MAX_DOCUMENT_BYTES:
        raise ValueError("review input exceeds the document limit")
    return raw


def _check_review(raw: dict, account: str, order: str, cutoff: datetime) -> _Review:
    view = _Review.model_validate(raw)
    if view.account_id != account or view.opening_order_id != order or view.as_of != cutoff:
        raise ValueError("opening review differs from the requested account, order or cutoff")
    opening = view.opening_identity
    if (
        opening.symbol != view.symbol
        or opening.instrument != view.instrument
        or view.opened_at > cutoff
        or view.thesis_expires_at <= view.opened_at
        or view.clock_review is not None
        and view.clock_review.at > cutoff
        or int(view.source_quantity_raw) > int(view.held_quantity_raw)
        or int(view.free_quantity_raw) + int(view.reserved_quantity_raw)
        != int(view.held_quantity_raw)
    ):
        raise ValueError("opening review has inconsistent stable lineage or quantities")
    return view


def _canonical_portfolio(
    raw: dict,
    view: _Review,
    requested_policy: dict,
    requested_marks: list[_Mark],
) -> PortfolioSnapshot:
    snapshot = _Snapshot.model_validate(raw)
    context = snapshot.snapshot_context
    if context.account_id != view.account_id or context.snapshot_version != view.account_version:
        raise ValueError("Go snapshot differs from the review account or account version")
    ordinary_policy = requested_policy.copy()
    if ordinary_policy.get("max_daily_turnover_usd") == "":
        ordinary_policy.pop("max_daily_turnover_usd")
    if ordinary_policy.get("block_new_buys_on_unknown") is False:
        ordinary_policy.pop("block_new_buys_on_unknown")
    if context.policy != ordinary_policy or context.marks != requested_marks:
        raise ValueError("Go snapshot changed the supplied owner policy or marks")

    wire = snapshot.portfolio
    if wire.keys() != PortfolioSnapshot.model_fields.keys():
        raise ValueError("Go snapshot has incomplete portfolio wire fields")
    _UUID.validate_python(wire["snapshot_id"])
    cutoff = _Clock.validate_python(wire["as_of"])
    for field in ("cash_usd", "reserved_cash_usd", "max_position_pct"):
        _PortfolioAmount.validate_python(wire[field])
    portfolio = PortfolioSnapshot.model_validate(wire)
    cap = _PositionCap.validate_python(context.policy.get("max_position_bps"))
    if (
        portfolio.cash_asset != "USD"
        or cutoff != view.as_of
        or portfolio.cash_usd != Decimal(view.cash_usd)
        or portfolio.reserved_cash_usd != Decimal(view.reserved_cash_usd)
        or portfolio.max_position_pct != Decimal(cap).scaleb(-4)
    ):
        raise ValueError("Go snapshot differs from the review cutoff or paper balances")

    rows = _Rules.validate_python(context.policy.get("instruments"))
    rules = {rule.symbol: rule.instrument for rule in rows}
    marks = {mark.instrument: mark for mark in context.marks}
    max_age = timedelta(
        seconds=_MarkAge.validate_python(context.policy.get("max_mark_age_seconds"))
    )
    if (
        len(rules) != len(rows)
        or len(marks) != len(context.marks)
        or any(
            mark.instrument not in rules.values()
            or not mark.at <= mark.available_at <= cutoff
            or cutoff - mark.at > max_age
            or Decimal(mark.price_usd) <= 0
            for mark in context.marks
        )
    ):
        raise ValueError("Go snapshot has ambiguous instruments or unavailable marks")
    for wire_position, position in zip(wire["positions"], portfolio.positions, strict=True):
        _PortfolioAmount.validate_python(wire_position["quantity"])
        _PortfolioAmount.validate_python(wire_position["mark_price_usd"])
        instrument = rules.get(position.symbol)
        mark = marks.get(instrument)
        if (
            mark is None
            or position.instrument_id != instrument.instrument_id
            or position.mark_price_usd != Decimal(mark.price_usd)
        ):
            raise ValueError(
                "Go snapshot position does not match the exact owner instrument or mark"
            )
    held = next(
        (position for position in portfolio.positions if position.symbol == view.symbol), None
    )
    quantity = Fraction(int(view.held_quantity_raw), 10**view.instrument.decimals)
    if (
        held is None
        or rules.get(view.symbol) != view.instrument
        or Fraction(held.quantity) != quantity
    ):
        raise ValueError("Go snapshot does not hold the reviewed opening instrument and quantity")
    return portfolio


def prepare_identity_review(
    *,
    invoke: GoRunner,
    archive_root: Path,
    account: str,
    order: str,
    as_of: str,
    policy: Path | bytes,
    marks: Path | bytes,
    data_manifest: ArchiveReference | dict,
    identity_registry: ArchiveReference | dict,
    quote_tape: ArchiveReference | dict,
    max_llm_calls: int,
) -> dict:
    """Build, but never submit, a Go review schedule from two consistent read receipts."""
    account = _UUID.validate_python(account)
    order = _UUID.validate_python(order)
    cutoff = _Clock.validate_python(as_of)
    budget = _Budget.validate_python(max_llm_calls)
    market, registry, tape = (
        ArchiveReference.model_validate(reference)
        for reference in (data_manifest, identity_registry, quote_tape)
    )
    if (
        len({market.ref, registry.ref, tape.ref}) != 3
        or len({market.sha256, registry.sha256, tape.sha256}) != 3
    ):
        raise ValueError("paper review requires independent archive, registry and quote tape pins")
    owner_registry = load_identity_registry(archive_root, registry)
    check_owner_quote_tape(archive_root, tape, account, registry)
    owner_policy, owner_marks = _read_document(policy), _read_document(marks)
    parsed_policy = decode_pinned_json(owner_policy)
    if not isinstance(parsed_policy, dict):
        raise ValueError("paper review policy must be a JSON object")
    parsed_marks = _Marks.validate_python(decode_pinned_json(owner_marks))

    view = _check_review(
        invoke("review", "--account", account, "--order", order, "--as-of", as_of),
        account,
        order,
        cutoff,
    )
    opening = view.opening_identity
    security = SecurityBinding(symbol=opening.symbol, security_id=opening.security_id)
    if _resolve_entry(owner_registry.entries, security, cutoff).instrument != opening.instrument:
        raise ValueError("current owner registry replaces the opening instrument")

    snapshot = read_paper_snapshot(
        invoke, account=account, as_of=as_of, policy=owner_policy, marks=owner_marks
    )
    portfolio = _canonical_portfolio(snapshot, view, parsed_policy, parsed_marks)
    projection = load_identity_snapshot(
        archive_root,
        market,
        registry,
        cutoff,
        (security,),
        positions=portfolio.positions,
    )
    schedule = {
        "review_schedule_schema_version": "0.2",
        "opening_order_id": order,
        "snapshot_context": snapshot["snapshot_context"],
        "portfolio": snapshot["portfolio"],
        "data_manifest": market.model_dump(mode="json"),
        "max_llm_calls": budget,
        "identity_binding": {
            "identity_registry": registry.model_dump(mode="json"),
            "quote_tape": tape.model_dump(mode="json"),
            "projected_snapshot_sha256": projection.sha256,
        },
    }
    if len(json.dumps(schedule, separators=(",", ":")).encode()) + 1 > MAX_DOCUMENT_BYTES:
        raise ValueError("prepared paper review exceeds Go's document limit")
    return schedule


def prepare_from_args(args: Namespace) -> dict:
    binary = args.paper_cli.resolve(strict=True)
    if not binary.is_file():
        raise ValueError("paper must be an explicit local binary")

    def invoke(*arguments: str) -> dict:
        try:
            return invoke_paper_cli(binary, *arguments, timeout=30)
        except subprocess.SubprocessError as exc:
            raise ValueError(f"paper {arguments[0]} read failed") from exc

    return prepare_identity_review(
        invoke=invoke,
        archive_root=args.archive_root,
        account=args.account,
        order=args.order,
        as_of=args.as_of,
        policy=args.policy,
        marks=args.marks,
        data_manifest={"ref": args.data_manifest_ref, "sha256": args.data_manifest_sha256},
        identity_registry={
            "ref": args.identity_registry_ref,
            "sha256": args.identity_registry_sha256,
        },
        quote_tape={"ref": args.quote_tape_ref, "sha256": args.quote_tape_sha256},
        max_llm_calls=args.max_llm_calls,
    )
