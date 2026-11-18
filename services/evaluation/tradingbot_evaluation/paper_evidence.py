"""Generate paired paper-ledger and idle USD cash marks for a frozen protocol."""

import json
from datetime import date, datetime, timedelta
from decimal import ROUND_FLOOR, Decimal, localcontext
from hashlib import sha256
from typing import Annotated, Literal

import msgspec

from tradingbot_evaluation.replay import MAX_INPUT_BYTES, IntentID, _unique_fields
from tradingbot_evaluation.statistics import (
    SHA256,
    DailyNav,
    EvaluationEvidence,
    EvaluationProtocol,
    PaperNavProvenance,
    load_evidence,
    protocol_sha256,
)

CASH_SERIES = "uninvested-cash"
ACCOUNT = Annotated[str, msgspec.Meta(pattern=r"^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$")]
LEDGER_USD = Annotated[str, msgspec.Meta(pattern=r"^(0|[1-9][0-9]{0,29})(\.[0-9]{1,8})?$")]
NAV_USD = Annotated[str, msgspec.Meta(pattern=r"^(0|[1-9][0-9]*)\.[0-9]{8}$")]
RAW = Annotated[str, msgspec.Meta(pattern=r"^[1-9][0-9]{0,37}$")]
UTC_STAMP = Annotated[
    str,
    msgspec.Meta(
        pattern=r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$"
    ),
]
SCALE = Decimal("0.00000001")
NS_PER_SECOND = 1_000_000_000


class MarkSource(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    kind: Literal["synthetic_fixture", "local_archive"]
    id: IntentID


class Instrument(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    instrument_id: IntentID
    registry_version: IntentID
    decimals: Annotated[int, msgspec.Meta(ge=0, le=18)]


class Mark(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    mark_id: IntentID
    instrument: Instrument
    price_usd: LEDGER_USD
    observed_at: UTC_STAMP
    available_at: UTC_STAMP


class Position(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    instrument: Instrument
    quantity_raw: RAW
    mark: Mark | msgspec.UnsetType = msgspec.UNSET
    issue: Literal["mark_missing", "mark_stale"] | msgspec.UnsetType = msgspec.UNSET


class LedgerDay(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    day: date
    cutoff: UTC_STAMP
    account_version: Annotated[int, msgspec.Meta(ge=1)]
    last_event_id: ACCOUNT
    ledger_prefix_sha256: SHA256
    cash_usd: LEDGER_USD
    recorded_fees_usd: LEDGER_USD
    nav_usd: NAV_USD | None
    positions: tuple[Position, ...]


class Funding(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    cash_usd: LEDGER_USD
    recorded_at: UTC_STAMP


class PaperNAV(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    status: Literal["complete", "incomplete"]
    account_id: ACCOUNT
    input_sha256: SHA256
    valuation_source: MarkSource
    mark_max_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]
    nav_basis: Literal["execution_net"]
    external_costs_usd: None
    mark_policy: Literal["last_locally_available_observation_at_utc_day_end"]
    ledger_hash_policy: Literal["recorded_event_json_lines_v1"]
    initial_funding: Funding
    daily_nav: Annotated[tuple[LedgerDay, ...], msgspec.Meta(min_length=1, max_length=366)]


def _source_snapshot(day: LedgerDay) -> tuple:
    positions = tuple(
        sorted(
            (
                position.instrument.instrument_id,
                position.instrument.registry_version,
                position.instrument.decimals,
                position.quantity_raw,
            )
            for position in day.positions
        )
    )
    return (
        day.last_event_id,
        day.ledger_prefix_sha256,
        day.cash_usd,
        day.recorded_fees_usd,
        positions,
    )


def _utc_nanoseconds(value: str) -> int:
    seconds, _, fraction = value[:-1].partition(".")
    instant = datetime.fromisoformat(seconds)
    whole = instant.toordinal() * 86400 + instant.hour * 3600 + instant.minute * 60 + instant.second
    return whole * NS_PER_SECOND + int(fraction.ljust(9, "0"))


def _verify_day(day: LedgerDay, max_age_seconds: int) -> str:
    if day.nav_usd is None or day.cutoff != f"{day.day.isoformat()}T23:59:59.999999999Z":
        raise ValueError("Paper NAV requires a complete mark for each closed UTC day")
    cutoff = _utc_nanoseconds(day.cutoff)
    with localcontext() as exact:
        exact.prec = 100
        total = Decimal(day.cash_usd)
        instruments = set()
        for position in day.positions:
            mark = position.mark
            if (
                mark is msgspec.UNSET
                or position.issue is not msgspec.UNSET
                or mark.instrument != position.instrument
                or position.instrument in instruments
                or Decimal(mark.price_usd) <= 0
            ):
                raise ValueError("Paper NAV has a missing or mismatched position mark")
            observed = _utc_nanoseconds(mark.observed_at)
            available = _utc_nanoseconds(mark.available_at)
            if (
                not observed <= available <= cutoff
                or cutoff - observed > max_age_seconds * NS_PER_SECOND
            ):
                raise ValueError("Paper NAV mark timing does not match its cutoff or maximum age")
            instruments.add(position.instrument)
            units = Decimal(position.quantity_raw).scaleb(-position.instrument.decimals)
            total += units * Decimal(mark.price_usd)
        if total <= 0 or total.quantize(SCALE, rounding=ROUND_FLOOR) != Decimal(day.nav_usd):
            raise ValueError("Paper NAV does not reconcile to cash and marked positions")
    return day.nav_usd


def build_paper_evidence(
    protocol: EvaluationProtocol, raw: bytes, *, expected_protocol_sha256: str
) -> EvaluationEvidence:
    frozen = protocol_sha256(protocol)
    if protocol.schema_version != "0.2" or frozen != expected_protocol_sha256:
        raise ValueError("Paper ledger evidence requires the exact frozen protocol 0.2")
    digest = sha256(raw).hexdigest()
    if protocol.paper_nav_sha256 != digest or len(raw) > MAX_INPUT_BYTES:
        raise ValueError("Frozen paper NAV SHA-256 or report size does not match")
    report = msgspec.convert(json.loads(raw, object_pairs_hook=_unique_fields), type=PaperNAV)
    if (
        report.status != "complete"
        or tuple(day.day for day in report.daily_nav) != protocol.expected_nav_dates
    ):
        raise ValueError("Paper NAV must cover every frozen day with a complete mark")
    if (
        report.valuation_source.kind == "synthetic_fixture"
        and protocol.source_mode != "synthetic_fixture"
    ):
        raise ValueError("Synthetic marks require a synthetic protocol")
    comparisons = protocol.comparisons
    if (
        len(comparisons) != 1
        or comparisons[0].benchmark != CASH_SERIES
        or comparisons[0].benchmark_access != "synthetic_fixture"
    ):
        raise ValueError("Paper ledger evidence supports the uninvested-cash synthetic benchmark")
    baseline = report.initial_funding.cash_usd
    first = report.daily_nav[0]
    if (
        Decimal(baseline) <= 0
        or first.account_version != 1
        or first.positions
        or first.last_event_id != report.account_id
        or Decimal(first.cash_usd) != Decimal(baseline)
        or Decimal(first.recorded_fees_usd) != 0
        or _utc_nanoseconds(report.initial_funding.recorded_at) > _utc_nanoseconds(first.cutoff)
    ):
        raise ValueError("Paper NAV requires an initial mark before portfolio activity")
    rows = []
    previous = None
    for day in report.daily_nav:
        if previous is not None and (
            day.day - previous.day != timedelta(days=1)
            or day.account_version < previous.account_version
            or Decimal(day.recorded_fees_usd) < Decimal(previous.recorded_fees_usd)
            or day.account_version == previous.account_version
            and _source_snapshot(day) != _source_snapshot(previous)
        ):
            raise ValueError("Paper NAV ledger lineage or UTC dates are inconsistent")
        mark = _verify_day(day, report.mark_max_age_seconds)
        rows.append(
            DailyNav(day=day.day, nav_usd={comparisons[0].strategy: mark, CASH_SERIES: baseline})
        )
        previous = day
    evidence = EvaluationEvidence(
        schema_version="0.2",
        protocol_sha256=frozen,
        daily_nav=tuple(rows),
        daily_nav_basis="execution_net",
        paper_nav_provenance=PaperNavProvenance(
            method="paper_ledger_export",
            paper_nav_sha256=digest,
            account_id=report.account_id,
        ),
    )
    return load_evidence(msgspec.json.encode(evidence))
