"""Generate synchronized paper-ledger and benchmark marks for a frozen protocol."""

import json
from datetime import date, datetime, timedelta
from decimal import ROUND_FLOOR, Decimal, localcontext
from fractions import Fraction
from hashlib import sha256
from typing import Annotated, Literal

import msgspec

from tradingbot_evaluation.replay import MAX_INPUT_BYTES, IntentID, _unique_fields
from tradingbot_evaluation.statistics import (
    CASH_SERIES,
    SHA256,
    DailyNav,
    EvaluationEvidence,
    EvaluationProtocol,
    ForwardPaperSource,
    PaperNavProvenance,
    load_evidence,
    paper_benchmark_access_matches,
    protocol_sha256,
)

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


class BenchmarkLevel(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    day: date
    observed_at: UTC_STAMP
    available_at: UTC_STAMP
    level: LEDGER_USD


class PaperBenchmark(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture", "local_archive"]
    series_id: IntentID
    basis: Literal["usd_total_return_index"]
    modeled_entry_cost_bps: Annotated[int, msgspec.Meta(ge=0, le=9999)]
    max_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]
    levels: Annotated[tuple[BenchmarkLevel, ...], msgspec.Meta(min_length=2, max_length=366)]
    archive_source_id: IntentID | msgspec.UnsetType = msgspec.UNSET


def load_paper_benchmark(raw: bytes) -> PaperBenchmark:
    if len(raw) > MAX_INPUT_BYTES:
        raise ValueError("Paper benchmark source size exceeds the limit")
    source = msgspec.convert(json.loads(raw, object_pairs_hook=_unique_fields), type=PaperBenchmark)
    if (source.source_mode == "local_archive") != (source.archive_source_id is not msgspec.UNSET):
        raise ValueError("Paper benchmark archive identity must match its source mode")
    return source


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


def _benchmark_nav(
    source: PaperBenchmark, days: tuple[LedgerDay, ...], funding: str
) -> tuple[str, ...]:
    if tuple(mark.day for mark in source.levels) != tuple(day.day for day in days):
        raise ValueError("Paper benchmark levels must cover every frozen UTC day")
    anchor = Decimal(source.levels[0].level)
    if anchor <= 0:
        raise ValueError("Paper benchmark levels must be positive")
    invested = Fraction(funding) * Fraction(10_000 - source.modeled_entry_cost_bps, 10_000)
    nav = [funding]
    previous = None
    for day, mark in zip(days, source.levels, strict=True):
        cutoff = _utc_nanoseconds(day.cutoff)
        observed = _utc_nanoseconds(mark.observed_at)
        available = _utc_nanoseconds(mark.available_at)
        level = Decimal(mark.level)
        if not observed <= available <= cutoff or (
            cutoff - observed > source.max_age_seconds * NS_PER_SECOND
        ):
            raise ValueError("Paper benchmark level timing differs from cutoff or freshness policy")
        if level <= 0:
            raise ValueError("Paper benchmark levels must be positive")
        if previous is not None:
            last_cutoff, last_observed, last_available, last_level = previous
            if (
                observed < last_observed
                or observed == last_observed
                and (available, level) != (last_available, last_level)
                or observed > last_observed
                and available <= last_cutoff
            ):
                raise ValueError("Paper benchmark observations disagree with prior day-end marks")
            units = invested * Fraction(level) / Fraction(anchor) * 100_000_000
            whole_units = units.numerator // units.denominator
            if whole_units == 0:
                raise ValueError("Paper benchmark NAV is below the supported precision")
            dollars, remainder = divmod(whole_units, 100_000_000)
            nav.append(f"{dollars}.{remainder:08d}")
        previous = cutoff, observed, available, level
    return tuple(nav)


def build_paper_evidence(
    protocol: EvaluationProtocol,
    raw: bytes,
    *,
    expected_protocol_sha256: str,
    benchmark_raw: bytes | None = None,
    forward_source: ForwardPaperSource | None = None,
) -> EvaluationEvidence:
    evidence, _ = _build_paper_evidence(
        protocol, raw, expected_protocol_sha256, benchmark_raw, forward_source
    )
    return evidence


def build_paper_evidence_with_cost_diagnostics(
    protocol: EvaluationProtocol,
    raw: bytes,
    *,
    expected_protocol_sha256: str,
    benchmark_raw: bytes | None = None,
    forward_source: ForwardPaperSource | None = None,
) -> tuple[EvaluationEvidence, dict]:
    evidence, report = _build_paper_evidence(
        protocol, raw, expected_protocol_sha256, benchmark_raw, forward_source
    )
    daily_nav = []
    with localcontext() as exact:
        exact.prec = 100
        for mark, day in zip(evidence.daily_nav, report.daily_nav, strict=True):
            net = mark.nav_usd[protocol.comparisons[0].strategy]
            before_fees = Decimal(net) + Decimal(day.recorded_fees_usd)
            daily_nav.append(
                {
                    "day": mark.day.isoformat(),
                    "execution_net_nav_usd": net,
                    "cumulative_recorded_fill_fees_usd": day.recorded_fees_usd,
                    "nav_before_recorded_execution_fees_usd": format(before_fees, ".8f"),
                }
            )
    return evidence, {
        "basis": "execution_net_plus_recorded_fill_fees",
        "assumption": "fixed_recorded_fills_and_end_of_day_marks",
        "gross_nav": {"status": "unavailable", "reason": "independent_execution_reference_missing"},
        "daily_nav": daily_nav,
    }


def _build_paper_evidence(
    protocol: EvaluationProtocol,
    raw: bytes,
    expected_protocol_sha256: str,
    benchmark_raw: bytes | None,
    forward_source: ForwardPaperSource | None,
) -> tuple[EvaluationEvidence, PaperNAV]:
    frozen = protocol_sha256(protocol)
    if protocol.schema_version != "0.2" or frozen != expected_protocol_sha256:
        raise ValueError("Paper ledger evidence requires the exact frozen protocol 0.2")
    digest = sha256(raw).hexdigest()
    if protocol.paper_nav_sha256 != digest or len(raw) > MAX_INPUT_BYTES:
        raise ValueError("Frozen paper NAV SHA-256 or report size does not match")
    report = msgspec.convert(json.loads(raw, object_pairs_hook=_unique_fields), type=PaperNAV)
    if forward_source is not None:
        _verify_registered_source(report, forward_source, protocol.expected_nav_dates[0])
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
    benchmarks = {row.benchmark for row in comparisons}
    benchmark_pin = protocol.paper_benchmark_sha256
    if (
        len({row.strategy for row in comparisons}) != 1
        or len(comparisons) != len(benchmarks)
        or not paper_benchmark_access_matches(protocol)
    ):
        raise ValueError(
            "Paper ledger evidence requires one strategy and distinct eligible benchmarks"
        )
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
    benchmark_values = {}
    benchmark_archive_source_id = msgspec.UNSET
    if CASH_SERIES in benchmarks:
        benchmark_values[CASH_SERIES] = (baseline,) * len(report.daily_nav)
    if benchmark_pin is msgspec.UNSET:
        if benchmark_raw is not None or benchmarks != {CASH_SERIES}:
            raise ValueError("Paper ledger evidence requires a pin for a noncash benchmark")
    else:
        if (
            benchmark_raw is None
            or len(benchmark_raw) > MAX_INPUT_BYTES
            or sha256(benchmark_raw).hexdigest() != benchmark_pin
        ):
            raise ValueError("Frozen paper benchmark SHA-256 or source size does not match")
        source = load_paper_benchmark(benchmark_raw)
        if (source.source_mode == "synthetic_fixture") != (
            protocol.source_mode == "synthetic_fixture"
        ):
            raise ValueError("Paper benchmark source mode differs from the registered protocol")
        if forward_source is not None:
            registered = forward_source.benchmark
            if registered is msgspec.UNSET or (
                source.series_id,
                source.modeled_entry_cost_bps,
                source.max_age_seconds,
                source.archive_source_id,
            ) != (
                registered.series_id,
                registered.modeled_entry_cost_bps,
                registered.max_age_seconds,
                registered.archive_source_id,
            ):
                raise ValueError("Paper benchmark does not match its registered settings")
        if source.series_id == CASH_SERIES or benchmarks - {CASH_SERIES} != {source.series_id}:
            raise ValueError("Paper benchmark identity differs from the registered noncash series")
        benchmark_archive_source_id = source.archive_source_id
        benchmark_values[source.series_id] = _benchmark_nav(source, report.daily_nav, baseline)
    rows = []
    previous = None
    for index, day in enumerate(report.daily_nav):
        if previous is not None and (
            day.day - previous.day != timedelta(days=1)
            or day.account_version < previous.account_version
            or Decimal(day.recorded_fees_usd) < Decimal(previous.recorded_fees_usd)
            or day.account_version == previous.account_version
            and _source_snapshot(day) != _source_snapshot(previous)
        ):
            raise ValueError("Paper NAV ledger lineage or UTC dates are inconsistent")
        mark = _verify_day(day, report.mark_max_age_seconds)
        nav = {name: values[index] for name, values in benchmark_values.items()}
        nav[comparisons[0].strategy] = mark
        rows.append(DailyNav(day=day.day, nav_usd=nav))
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
            paper_benchmark_sha256=benchmark_pin,
            paper_benchmark_archive_source_id=benchmark_archive_source_id,
        ),
    )
    return load_evidence(msgspec.json.encode(evidence)), report


def _verify_registered_source(
    report: PaperNAV, source: ForwardPaperSource, first_day: date
) -> None:
    if (
        report.account_id != source.account_id
        or Decimal(report.initial_funding.cash_usd) != Decimal(source.initial_cash_usd)
        or report.initial_funding.recorded_at[:10] != first_day.isoformat()
        or report.valuation_source.kind != source.valuation_source.kind
        or report.valuation_source.id != source.valuation_source.id
        or report.mark_max_age_seconds != source.mark_max_age_seconds
    ):
        raise ValueError("Paper NAV does not match the registered paper source and initial funding")
