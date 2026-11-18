"""Deterministic baselines for a frozen synthetic Nautilus quote replay."""

from collections import defaultdict
from datetime import UTC, datetime, timedelta
from decimal import Decimal

import msgspec

from tradingbot_evaluation.replay import (
    Order,
    Quote,
    Replay,
    _nanoseconds,
    _SplitUnits,
    executable_quotes,
    replay_sha256,
    run_replay,
)
from tradingbot_evaluation.trend import TrendConfig, run_trend

BENCHMARK_VERSION = "0.1"


def _money(amount: Decimal) -> str:
    return format(amount, ".2f")


def _summary(
    starting: Decimal,
    cash: Decimal,
    nav: Decimal,
    commission: Decimal,
    positions: dict,
    fills: list,
) -> dict:
    return {
        "ending_cash_usd": _money(cash),
        "ending_bid_mark_nav_usd": _money(nav),
        "simulated_commission_usd": _money(commission),
        "pnl_after_simulated_commission_usd": _money(nav - starting),
        "open_positions": positions,
        "fills": fills,
    }


def _run_sleeve(
    replay: Replay,
    budget: Decimal,
    formation: Quote,
    quotes: list[Quote],
    start_at: datetime,
    end_at: datetime,
    share_step: int,
) -> dict:
    # Size from the quote already known at formation, including the configured fee.
    budget_cents = int(budget * 100)
    ask_cents = int(Decimal(formation.ask_usd) * 100)
    quantity = min(
        1_000_000, budget_cents * 10_000 // (ask_cents * (10_000 + replay.commission_bps))
    )
    quantity -= quantity % share_step
    if quantity == 0:
        return {
            **_summary(budget, budget, budget, Decimal(0), {}, []),
            "entry_status": "below_minimum_allocation",
            "simulated_dividend_cash_usd": "0.00",
            "cash_dividend_events": [],
            "stock_split_events": [],
            "split_marks_in_prior_units": [],
        }

    # Nautilus order expiry is exclusive. The final fixture quote stays eligible.
    expires_at = end_at + timedelta(microseconds=1)
    order = Order(formation.symbol, formation.symbol, "BUY", quantity, start_at, expires_at)
    result = run_replay(
        msgspec.structs.replace(
            replay,
            starting_cash_usd=_money(budget),
            quotes=tuple(quotes),
            orders=(order,),
            cash_dividends=tuple(
                action
                for action in replay.cash_dividends
                if action.symbol == formation.symbol and action.available_at <= end_at
            ),
            stock_splits=tuple(
                action
                for action in replay.stock_splits
                if action.symbol == formation.symbol and action.effective_at <= end_at
            ),
        ),
        through=end_at,
    )
    return {
        **_summary(
            budget,
            Decimal(result["ending_cash_usd"]),
            Decimal(result["ending_bid_mark_nav_usd"]),
            Decimal(result["simulated_commission_usd"]),
            result["open_positions"],
            result["fills"],
        ),
        "entry_status": result["intents"][order.intent_id],
        "simulated_dividend_cash_usd": result.get("simulated_dividend_cash_usd", "0.00"),
        "cash_dividend_events": result.get("cash_dividend_events", []),
        "stock_split_events": result.get("stock_split_events", []),
        "split_marks_in_prior_units": result.get("split_marks_in_prior_units", []),
    }


def _equal_weight(
    replay: Replay,
    formation: dict[str, Quote],
    by_symbol: dict[str, list[Quote]],
    start_at: datetime,
    end_at: datetime,
    share_steps: dict[str, int],
) -> dict:
    starting = Decimal(replay.starting_cash_usd)
    # Each sleeve receives its cash once. The private cash account prevents price
    # gaps or rejected fills on one symbol from spending another symbol's allocation.
    even_cents, leftover_cents = divmod(int(starting * 100), len(formation))
    allocations = {}
    sleeves = {}
    for index, symbol in enumerate(sorted(formation)):
        budget = Decimal(even_cents + (index < leftover_cents)) / 100
        allocations[symbol] = _money(budget)
        sleeves[symbol] = _run_sleeve(
            replay,
            budget,
            formation[symbol],
            by_symbol[symbol],
            start_at,
            end_at,
            share_steps[symbol],
        )

    def sum_field(field: str) -> Decimal:
        return sum((Decimal(row[field]) for row in sleeves.values()), Decimal(0))

    positions = {
        symbol: quantity
        for row in sleeves.values()
        for symbol, quantity in row["open_positions"].items()
    }
    fills = sorted(
        (fill for row in sleeves.values() for fill in row["fills"]),
        key=lambda fill: (int(fill["filled_at_ns"]), fill["symbol"]),
    )
    return {
        "allocations_usd": allocations,
        "entry_statuses": {symbol: row["entry_status"] for symbol, row in sleeves.items()},
        "simulated_dividend_cash_usd": _money(sum_field("simulated_dividend_cash_usd")),
        "cash_dividend_events": [
            event for row in sleeves.values() for event in row["cash_dividend_events"]
        ],
        "stock_split_events": sorted(
            (event for row in sleeves.values() for event in row["stock_split_events"]),
            key=lambda event: (int(event["effective_at_ns"]), event["action_id"]),
        ),
        "split_marks_in_prior_units": sorted(
            symbol for row in sleeves.values() for symbol in row["split_marks_in_prior_units"]
        ),
        **_summary(
            starting,
            sum_field("ending_cash_usd"),
            sum_field("ending_bid_mark_nav_usd"),
            sum_field("simulated_commission_usd"),
            positions,
            fills,
        ),
    }


def run_benchmarks(
    replay: Replay,
    *,
    start_at: datetime,
    buy_and_hold_symbol: str | None,
    through: datetime | None = None,
    include_equal_weight: bool = True,
    trend: TrendConfig | None = None,
) -> dict:
    """Compare cash, single-symbol buy-and-hold and a one-time equal allocation."""
    if replay.source_mode != "synthetic_fixture":
        raise ValueError("Benchmarks require a synthetic quote fixture")
    if start_at.utcoffset() is None:
        raise ValueError("Benchmark start must include a timezone offset")
    start_at = start_at.astimezone(UTC)
    last_quote_at = max(quote.available_at for quote in replay.quotes)
    if through is not None and (through.utcoffset() is None or through < last_quote_at):
        raise ValueError("Benchmark horizon must include a timezone offset and cover all quotes")
    end_at = (last_quote_at if through is None else through).astimezone(UTC)
    if start_at >= end_at:
        raise ValueError("Benchmark start must precede the end of the evaluation")
    _nanoseconds(start_at)
    _nanoseconds(end_at + timedelta(microseconds=1))
    split_units = _SplitUnits(replay.stock_splits, end_at)

    formation = {}
    by_symbol = defaultdict(list)
    for quote in replay.quotes:
        by_symbol[quote.symbol].append(quote)
    for quote in executable_quotes(replay):
        if quote.available_at > start_at or not split_units.quote_is_current(quote, start_at):
            continue
        latest = formation.get(quote.symbol)
        if latest is None or (quote.observed_at, quote.available_at) > (
            latest.observed_at,
            latest.available_at,
        ):
            formation[quote.symbol] = quote

    if buy_and_hold_symbol is not None and buy_and_hold_symbol not in formation:
        raise ValueError("Buy-and-hold benchmark symbol must have a quote known at benchmark start")
    if include_equal_weight and not formation:
        raise ValueError("Equal-weight benchmark requires a quote known at benchmark start")
    active_symbols = set(formation) if include_equal_weight else set()
    if buy_and_hold_symbol is not None:
        active_symbols.add(buy_and_hold_symbol)
    share_steps = {
        symbol: split_units.factor_at(symbol, start_at).numerator
        for symbol in sorted(active_symbols)
    }
    starting = Decimal(replay.starting_cash_usd)
    benchmarks = {
        "cash": {
            **_summary(starting, starting, starting, Decimal(0), {}, []),
            "simulated_dividend_cash_usd": "0.00",
            "cash_dividend_events": [],
            "stock_split_events": [],
            "split_marks_in_prior_units": [],
        }
    }
    if buy_and_hold_symbol is not None:
        hold = _run_sleeve(
            replay,
            starting,
            formation[buy_and_hold_symbol],
            by_symbol[buy_and_hold_symbol],
            start_at,
            end_at,
            share_steps[buy_and_hold_symbol],
        )
        benchmarks["buy_and_hold"] = {"symbol": buy_and_hold_symbol, **hold}
    if include_equal_weight:
        benchmarks["equal_weight_once"] = _equal_weight(
            replay, formation, by_symbol, start_at, end_at, share_steps
        )
    if trend is not None:
        active_symbols.add(trend.symbol)
        benchmarks["trend_momentum"] = run_trend(
            replay, config=trend, start_at=start_at, end_at=end_at
        )

    return {
        "benchmark_schema_version": BENCHMARK_VERSION,
        "source_mode": replay.source_mode,
        "alpha_measured": False,
        "external_costs_usd": None,
        "normalized_input_sha256": replay_sha256(replay),
        "starting_cash_usd": _money(starting),
        "start_at": start_at.isoformat(),
        "end_at": end_at.isoformat(),
        "commission_bps": replay.commission_bps,
        "assumptions": {
            "venue_access": "synthetic_fixture",
            "base_currency": "USD",
            "cash_interest_bps": 0,
            "position_mark": "final_known_bid",
            "entry": "whole_share_ioc_on_next_newer_quote",
            "equal_weight": "one_time_equal_starting_cash_no_rebalancing",
            "corporate_actions_included": any(
                action.symbol in active_symbols and action.available_at <= end_at
                for action in replay.cash_dividends
            )
            or any(
                action.symbol in active_symbols and action.effective_at <= end_at
                for action in replay.stock_splits
            ),
            "raw_share_step_at_formation": share_steps,
            **(
                {"execution_calendar_id": replay.execution_calendar.calendar_id}
                if replay.execution_calendar is not msgspec.UNSET
                else {}
            ),
        },
        "benchmarks": benchmarks,
    }
