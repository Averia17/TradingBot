"""Derive daily evidence by replaying bounded synthetic prefixes through UTC day-end."""

from datetime import UTC, datetime, time, timedelta
from decimal import Decimal
from importlib.metadata import version

import msgspec

from tradingbot_evaluation.benchmarks import benchmark_nav_series
from tradingbot_evaluation.replay import (
    MAX_PREFIX_EVENTS,
    MAX_PREFIX_SYMBOLS,
    Replay,
    _nanoseconds,
    executable_quotes,
    replay_event_count,
    replay_nav_series,
    replay_sha256,
)
from tradingbot_evaluation.replay_identity import ReplayIdentity
from tradingbot_evaluation.research_replay import (
    _index_tape,
    prepare_research_replay,
)
from tradingbot_evaluation.statistics import (
    DailyNav,
    EvaluationEvidence,
    EvaluationProtocol,
    IdentityReplayInputs,
    ReplayBenchmark,
    ReplayProvenance,
    ReplaySource,
    load_evidence,
    verify_frozen_protocol,
)
from tradingbot_evaluation.trend import TrendConfig

MAX_MARK_DAYS = 64


def derive_replay_evidence(
    protocol: EvaluationProtocol,
    replay: Replay,
    *,
    expected_protocol_sha256: str,
) -> EvaluationEvidence:
    frozen, source = _frozen_replay_source(protocol, expected_protocol_sha256)
    if source.identity is not msgspec.UNSET:
        raise ValueError("Stable identity evidence requires the original research export, tape and registry")
    return _make_evidence(protocol, replay, frozen, source, source.benchmarks)


def derive_identity_replay_evidence(
    protocol: EvaluationProtocol,
    tape_raw: bytes,
    export_raw: bytes,
    identity_registry_raw: bytes,
    *,
    expected_protocol_sha256: str,
) -> EvaluationEvidence:
    frozen, source = _frozen_replay_source(protocol, expected_protocol_sha256)
    pins = source.identity
    if pins is msgspec.UNSET:
        raise ValueError("Frozen stable identity replay source is required")
    prepared = prepare_research_replay(
        export_raw,
        tape_raw,
        expected_export_sha256=pins.research_export_sha256,
        expected_tape_sha256=pins.tape_sha256,
        identity_registry_raw=identity_registry_raw,
        expected_identity_registry_sha256=pins.identity_registry_sha256,
    )
    identity = prepared.identity
    if identity is None:
        raise ValueError("Stable identity evidence requires a version 0.2 synthetic tape")
    replay = prepared.replay
    validate_identity_quotes(replay, identity)
    benchmarks = resolve_identity_benchmarks(source.benchmarks, identity)
    return _make_evidence(protocol, replay, frozen, source, benchmarks, identity=identity)


def validate_identity_quotes(replay: Replay, identity: ReplayIdentity) -> None:
    # A raw quote outside its registry window may be harmless to research order
    # matching but could form a benchmark or value a position. Reject it before
    # using the same frozen tape in both the strategy and benchmark engines.
    for quote in replay.quotes:
        _, active = identity.describe_holding(quote.symbol, quote.available_at)
        if quote.observed_at < active.effective_at:
            raise ValueError("Replay quote precedes its stable identity's effective time")


def _frozen_replay_source(
    protocol: EvaluationProtocol, expected_protocol_sha256: str
) -> tuple[str, ReplaySource]:
    frozen = verify_frozen_protocol(protocol, expected_protocol_sha256)
    source = protocol.replay_source
    if source is msgspec.UNSET:
        raise ValueError("A frozen synthetic replay source is required")
    return frozen, source


def resolve_identity_benchmarks(
    mappings: tuple[ReplayBenchmark, ...], identity: ReplayIdentity
) -> tuple[ReplayBenchmark, ...]:
    keys = {instrument.security_id: instrument.key for instrument in identity.instruments}
    resolved = []
    for mapping in mappings:
        if mapping.security_id is msgspec.UNSET:
            resolved.append(mapping)
            continue
        key = keys.get(mapping.security_id)
        if key is None:
            raise ValueError("Frozen benchmark security_id is absent from the exact replay instruments")
        resolved.append(msgspec.structs.replace(mapping, symbol=key))
    return tuple(resolved)


def _make_evidence(
    protocol: EvaluationProtocol,
    replay: Replay,
    frozen: str,
    source: ReplaySource,
    benchmarks: tuple[ReplayBenchmark, ...],
    *,
    identity: ReplayIdentity | None = None,
) -> EvaluationEvidence:
    if replay.source_mode != "synthetic_fixture":
        raise ValueError("A frozen synthetic replay source is required")
    digest = replay_sha256(replay)
    if digest != source.replay_sha256:
        raise ValueError("Normalized replay SHA-256 differs from the frozen replay source")
    evidence = EvaluationEvidence(
        schema_version=protocol.schema_version,
        protocol_sha256=frozen,
        daily_nav=native_daily_nav(
            protocol, replay, benchmarks,
            mark_max_age_seconds=(
                None if source.identity is msgspec.UNSET else source.identity.mark_max_age_seconds
            ),
            identity=identity,
        ),
        daily_nav_basis="execution_net" if protocol.schema_version == "0.2" else msgspec.UNSET,
        replay_provenance=native_replay_provenance(digest, identity=source.identity),
    )
    # Use the same wire decoder as statistics to catch out-of-range derived NAV.
    return load_evidence(msgspec.json.encode(evidence))


def native_replay_provenance(
    digest: str, *, identity: IdentityReplayInputs | msgspec.UnsetType = msgspec.UNSET,
) -> ReplayProvenance:
    return ReplayProvenance(
        method="nautilus_prefix",
        normalized_replay_sha256=digest,
        nautilus_version=version("nautilus-trader"),
        mark_policy="last_known_bid_at_utc_day_end",
        execution_costs="bid_ask_and_simulated_commission",
        external_costs_usd=None,
        identity=identity,
    )


def native_daily_nav(
    protocol: EvaluationProtocol,
    replay: Replay,
    benchmarks: tuple[ReplayBenchmark, ...],
    *,
    mark_max_age_seconds: int | None = None,
    strategy: str | None = None,
    identity: ReplayIdentity | None = None,
) -> tuple[DailyNav, ...]:
    days = protocol.expected_nav_dates
    if (
        len(days) > MAX_MARK_DAYS
        or len({quote.symbol for quote in replay.quotes}) > MAX_PREFIX_SYMBOLS
        or replay_event_count(replay) > MAX_PREFIX_EVENTS
    ):
        raise ValueError(
            f"Replay evidence is bounded to {MAX_MARK_DAYS} marks, "
            f"{MAX_PREFIX_SYMBOLS} symbols and {MAX_PREFIX_EVENTS} events"
        )
    start_at = datetime.combine(days[0], time.max, tzinfo=UTC)
    cutoffs = tuple(datetime.combine(day, time.max, tzinfo=UTC) for day in days)
    _nanoseconds(start_at)
    _nanoseconds(cutoffs[-1])
    if any(order.ready_at < start_at for order in replay.orders):
        raise ValueError("Strategy intents must be ready at or after the initial NAV anchor")

    cash = format(Decimal(replay.starting_cash_usd), ".2f")
    if strategy is None:
        strategy = protocol.comparisons[0].strategy
    funding = {strategy: cash} | {benchmark.series_id: cash for benchmark in benchmarks}
    buy_and_hold = next(
        (benchmark.symbol for benchmark in benchmarks if benchmark.kind == "buy_and_hold"),
        None,
    )
    equal_weight = any(benchmark.kind == "equal_weight_once" for benchmark in benchmarks)
    scheduled_equal_weight = next(
        (
            mapping.rebalance_schedule
            for mapping in benchmarks
            if mapping.kind == "equal_weight_scheduled"
        ),
        None,
    )
    trend_mapping = next(
        (benchmark for benchmark in benchmarks if benchmark.kind == "trend_momentum"),
        None,
    )
    trend = None
    if trend_mapping is not None:
        trend = TrendConfig(
            trend_mapping.symbol, trend_mapping.lookback_quotes, trend_mapping.max_gap_seconds
        )
        if not any(quote.symbol == trend.symbol for quote in replay.quotes):
            raise ValueError("Trend replay benchmark symbol must appear in the frozen quotes")
    needs_formation = buy_and_hold is not None or equal_weight or scheduled_equal_weight is not None
    if needs_formation and all(
        quote.available_at > start_at for quote in executable_quotes(replay)
    ):
        raise ValueError("Replay benchmark requires quotes known at the initial NAV anchor")

    # The anchor is initial funding. Orders and benchmark purchases can first
    # execute on quotes observed strictly after it.
    marks = [DailyNav(day=days[0], nav_usd=funding)]
    quote_index = _index_tape(replay)
    strategy_marks = replay_nav_series(replay, cutoffs[1:])
    benchmark_marks = (
        benchmark_nav_series(
            replay,
            cutoffs[1:],
            start_at=start_at,
            buy_and_hold_symbol=buy_and_hold,
            include_equal_weight=equal_weight,
            trend=trend,
            equal_weight_schedule=scheduled_equal_weight,
        )
        if needs_formation or trend is not None
        else {}
    )
    for cutoff in cutoffs[1:]:
        nav = funding.copy()
        mark = strategy_marks[cutoff]
        nav[strategy] = mark.nav_usd
        held = set(mark.held_symbols)
        for mapping in benchmarks:
            series = benchmark_marks.get(mapping.kind)
            if series is not None:
                mark = series[cutoff]
                nav[mapping.series_id] = mark.nav_usd
                held.update(mark.held_symbols)
        if mark_max_age_seconds is not None:
            max_age = timedelta(seconds=mark_max_age_seconds)
            for symbol in sorted(held):
                mark_floor = None
                if identity is not None:
                    _, active = identity.describe_holding(symbol, cutoff)
                    mark_floor = active.effective_at
                quote = quote_index[symbol].reference_at(cutoff, mark_floor=mark_floor)
                if quote is None or cutoff - quote.observed_at > max_age:
                    source = "Walk-forward" if identity is None else "Identity replay"
                    raise ValueError(
                        f"{source} missing or stale mark for {symbol} on {cutoff.date()}"
                    )
        if any(Decimal(amount) <= 0 for amount in nav.values()):
            raise ValueError("Replay evidence requires positive NAV on every frozen date")
        marks.append(DailyNav(day=cutoff.date(), nav_usd=nav))

    return tuple(marks)
