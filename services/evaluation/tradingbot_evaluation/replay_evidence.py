"""Derive daily evidence by replaying bounded synthetic prefixes through UTC day-end."""

from datetime import UTC, datetime, time
from decimal import Decimal
from importlib.metadata import version

import msgspec

from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.replay import Replay, _nanoseconds, replay_sha256, run_replay
from tradingbot_evaluation.statistics import (
    DailyNav,
    EvaluationEvidence,
    EvaluationProtocol,
    ReplayProvenance,
    _validate_protocol,
    load_evidence,
    protocol_sha256,
)
from tradingbot_evaluation.trend import TrendConfig

MAX_MARK_DAYS = 64
MAX_SYMBOLS = 8
MAX_EVENTS = 2048


def _prefix(replay: Replay, through: datetime) -> Replay:
    quotes = tuple(quote for quote in replay.quotes if quote.available_at <= through)
    symbols = {quote.symbol for quote in quotes}
    return msgspec.structs.replace(
        replay,
        quotes=quotes,
        orders=tuple(order for order in replay.orders if order.ready_at <= through),
        cash_dividends=tuple(
            action
            for action in replay.cash_dividends
            if action.symbol in symbols and action.available_at <= through
        ),
        stock_splits=tuple(
            action
            for action in replay.stock_splits
            if action.symbol in symbols and action.available_at <= through
        ),
    )


def derive_replay_evidence(
    protocol: EvaluationProtocol,
    replay: Replay,
    *,
    expected_protocol_sha256: str,
) -> EvaluationEvidence:
    frozen = protocol_sha256(protocol)
    if expected_protocol_sha256 != frozen:
        raise ValueError("Frozen protocol SHA-256 differs from the expected digest")
    _validate_protocol(protocol)
    source = protocol.replay_source
    if source is msgspec.UNSET or replay.source_mode != "synthetic_fixture":
        raise ValueError("A frozen synthetic replay source is required")
    digest = replay_sha256(replay)
    if digest != source.replay_sha256:
        raise ValueError("Normalized replay SHA-256 differs from the frozen replay source")
    days = protocol.expected_nav_dates
    if (
        len(days) > MAX_MARK_DAYS
        or len({quote.symbol for quote in replay.quotes}) > MAX_SYMBOLS
        or len(replay.quotes)
        + len(replay.orders)
        + len(replay.cash_dividends)
        + len(replay.stock_splits)
        > MAX_EVENTS
    ):
        raise ValueError(
            f"Replay evidence is bounded to {MAX_MARK_DAYS} marks, {MAX_SYMBOLS} symbols and {MAX_EVENTS} events"
        )
    start_at = datetime.combine(days[0], time.max, tzinfo=UTC)
    cutoffs = tuple(datetime.combine(day, time.max, tzinfo=UTC) for day in days)
    _nanoseconds(start_at)
    _nanoseconds(cutoffs[-1])
    if any(order.ready_at < start_at for order in replay.orders):
        raise ValueError("Strategy intents must be ready at or after the initial NAV anchor")

    cash = format(Decimal(replay.starting_cash_usd), ".2f")
    strategy = protocol.comparisons[0].strategy
    funding = {strategy: cash} | {benchmark.series_id: cash for benchmark in source.benchmarks}
    buy_and_hold = next(
        (benchmark.symbol for benchmark in source.benchmarks if benchmark.kind == "buy_and_hold"),
        None,
    )
    equal_weight = any(benchmark.kind == "equal_weight_once" for benchmark in source.benchmarks)
    trend_mapping = next(
        (benchmark for benchmark in source.benchmarks if benchmark.kind == "trend_momentum"),
        None,
    )
    trend = None
    if trend_mapping is not None:
        trend = TrendConfig(
            trend_mapping.symbol, trend_mapping.lookback_quotes, trend_mapping.max_gap_seconds
        )
        if not any(quote.symbol == trend.symbol for quote in replay.quotes):
            raise ValueError("Trend replay benchmark symbol must appear in the frozen quotes")
    needs_formation = buy_and_hold is not None or equal_weight
    if needs_formation and all(quote.available_at > start_at for quote in replay.quotes):
        raise ValueError("Replay benchmark requires quotes known at the initial NAV anchor")

    # The anchor is initial funding. Orders and benchmark purchases can first
    # execute on quotes observed strictly after it.
    marks = [DailyNav(day=days[0], nav_usd=funding)]
    for cutoff in cutoffs[1:]:
        prefix = _prefix(replay, cutoff)
        nav = funding.copy()
        if prefix.quotes:
            nav[strategy] = run_replay(prefix, through=cutoff)["ending_bid_mark_nav_usd"]
        if prefix.quotes and (needs_formation or trend is not None):
            report = run_benchmarks(
                prefix,
                start_at=start_at,
                through=cutoff,
                buy_and_hold_symbol=buy_and_hold,
                include_equal_weight=equal_weight,
                trend=trend,
            )["benchmarks"]
            for mapping in source.benchmarks:
                nav[mapping.series_id] = report[mapping.kind]["ending_bid_mark_nav_usd"]
        if any(Decimal(amount) <= 0 for amount in nav.values()):
            raise ValueError("Replay evidence requires positive NAV on every frozen date")
        marks.append(DailyNav(day=cutoff.date(), nav_usd=nav))

    evidence = EvaluationEvidence(
        schema_version=protocol.schema_version,
        protocol_sha256=frozen,
        daily_nav=tuple(marks),
        daily_nav_basis="execution_net" if protocol.schema_version == "0.2" else msgspec.UNSET,
        replay_provenance=ReplayProvenance(
            method="nautilus_prefix",
            normalized_replay_sha256=digest,
            nautilus_version=version("nautilus-trader"),
            mark_policy="last_known_bid_at_utc_day_end",
            execution_costs="bid_ask_and_simulated_commission",
            external_costs_usd=None,
        ),
    )
    # Use the same wire decoder as statistics to catch out-of-range derived NAV.
    return load_evidence(msgspec.json.encode(evidence))
