"""Single-symbol long-or-cash momentum using the native quote replay."""

import re
from collections import deque
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta
from decimal import Decimal
from fractions import Fraction
from functools import partial
from typing import Annotated

import msgspec
from nautilus_trader.model.currencies import USD

from tradingbot_evaluation.replay import (
    EPOCH,
    NativeNav,
    Order,
    Replay,
    _nanoseconds,
    _ReplayStrategy,
    _run_replay,
    _scaled_usd,
    _whole_shares,
    replay_nav_series,
)

MAX_LOOKBACK_QUOTES = 252
MAX_GAP_SECONDS = 86_400 * 30
TrendLookbackQuotes = Annotated[int, msgspec.Meta(ge=1, le=MAX_LOOKBACK_QUOTES)]
TrendMaxGapSeconds = Annotated[int, msgspec.Meta(ge=1, le=MAX_GAP_SECONDS)]


@dataclass(frozen=True, slots=True)
class TrendConfig:
    symbol: str
    lookback_quotes: TrendLookbackQuotes
    max_gap_seconds: TrendMaxGapSeconds

    def __post_init__(self):
        if not isinstance(self.symbol, str) or not re.fullmatch(
            r"[A-Z][A-Z0-9.]{0,11}", self.symbol
        ):
            raise ValueError("Invalid trend symbol")
        if (
            type(self.lookback_quotes) is not int
            or not 1 <= self.lookback_quotes <= MAX_LOOKBACK_QUOTES
        ):
            raise ValueError(f"Trend lookback must be between 1 and {MAX_LOOKBACK_QUOTES} quotes")
        if (
            type(self.max_gap_seconds) is not int
            or not 1 <= self.max_gap_seconds <= MAX_GAP_SECONDS
        ):
            raise ValueError(f"Trend max gap must be between 1 and {MAX_GAP_SECONDS} seconds")


class _TrendStrategy(_ReplayStrategy):
    def __init__(self, instruments, orders, units, *, config, start_at, commission_bps):
        super().__init__(instruments, orders, units)
        self.history = deque(maxlen=config.lookback_quotes + 1)
        self.last_observed_ns = None
        self.max_gap_ns = config.max_gap_seconds * 1_000_000_000
        self.order_lifetime = timedelta(seconds=config.max_gap_seconds)
        self.start_ns = _nanoseconds(start_at)
        self.commission_bps = commission_bps
        self.warmup_quotes = 0
        self.stale_or_reordered_quotes = 0
        self.observation_gap_resets = 0
        self.signals = []

    def on_quote_tick(self, tick):
        observed = tick.ts_event
        previous = self.last_observed_ns
        if tick.ts_init - observed > self.max_gap_ns or (
            previous is not None and observed <= previous
        ):
            self.stale_or_reordered_quotes += 1
            return

        symbol = tick.instrument_id.symbol.value
        queue = self.pending[symbol]
        if previous is not None and observed - previous > self.max_gap_ns:
            self.history.clear()
            self.observation_gap_resets += 1
            for order in queue:
                self.statuses[order.intent_id] = (
                    "expired"
                    if _nanoseconds(order.expires_at) <= tick.ts_init
                    else "cancelled_observation_gap"
                )
            queue.clear()
        self.last_observed_ns = observed
        # The engine stores exact first-era prices, so a split alone does not
        # make a cross-era price comparison look like a gain or loss.
        midpoint_twice = tick.bid_price.as_decimal() + tick.ask_price.as_decimal()
        self.history.append(midpoint_twice)
        before = len(self.submitted)
        super().on_quote_tick(tick)
        if len(self.history) < self.history.maxlen:
            self.warmup_quotes += 1
            return
        if tick.ts_init < self.start_ns or queue or len(self.submitted) != before:
            return
        self._size_signal(tick, symbol, midpoint_twice > self.history[0])

    def _size_signal(self, tick, symbol: str, want_long: bool) -> None:
        ready = EPOCH + timedelta(microseconds=tick.ts_init // 1000)
        factor = self.units.factor_at(symbol, ready)
        native_held = sum(
            (
                Fraction(position.quantity.as_decimal())
                for position in self.cache.positions_open(instrument_id=tick.instrument_id)
            ),
            Fraction(0),
        )
        held = _whole_shares(native_held * factor)
        if (held > 0) == want_long:
            return
        if want_long:
            cash = self.portfolio.account(tick.instrument_id.venue).balance_free(USD).as_decimal()
            ask = _scaled_usd(tick.ask_price.as_decimal(), 1 / factor)
            quantity = min(
                1_000_000,
                int(cash * 100) * 10_000 // (int(ask * 100) * (10_000 + self.commission_bps)),
            )
        else:
            quantity = min(held, 1_000_000)
        quantity -= quantity % factor.numerator
        if quantity <= 0:
            return
        side = "BUY" if want_long else "SELL"
        intent_id = f"trend:{symbol}:{tick.ts_init}"
        self.pending[symbol].append(
            Order(intent_id, symbol, side, quantity, ready, ready + self.order_lifetime)
        )
        self.statuses[intent_id] = "no_executable_quote"
        self.signals.append(
            {
                "intent_id": intent_id,
                "side": side,
                "quantity": str(quantity),
                "observed_at_ns": str(tick.ts_event),
                "ready_at_ns": str(tick.ts_init),
            }
        )

    def expire_unexecuted(self, horizon: datetime) -> None:
        for orders in self.pending.values():
            for order in orders:
                if order.expires_at <= horizon:
                    self.statuses[order.intent_id] = "expired"


def _trend_replay(replay: Replay, symbol: str) -> Replay:
    return msgspec.structs.replace(
        replay,
        quotes=tuple(quote for quote in replay.quotes if quote.symbol == symbol),
        orders=(),
        cash_dividends=tuple(action for action in replay.cash_dividends if action.symbol == symbol),
        stock_splits=tuple(action for action in replay.stock_splits if action.symbol == symbol),
    )


def _trend_strategy(replay: Replay, config: TrendConfig, start_at: datetime):
    return partial(
        _TrendStrategy, config=config, start_at=start_at, commission_bps=replay.commission_bps,
    )


def trend_nav(
    replay: Replay, cutoffs: tuple[datetime, ...], *, config: TrendConfig, start_at: datetime,
) -> dict[datetime, NativeNav]:
    return replay_nav_series(
        _trend_replay(replay, config.symbol), cutoffs,
        strategy_factory=_trend_strategy(replay, config, start_at),
    )


def run_trend(replay: Replay, *, config: TrendConfig, start_at: datetime, end_at: datetime) -> dict:
    fixture = _trend_replay(replay, config.symbol)
    if not fixture.quotes:
        cash = format(Decimal(replay.starting_cash_usd), ".2f")
        return {
            "parameters": asdict(config),
            "ending_cash_usd": cash,
            "ending_bid_mark_nav_usd": cash,
            "pnl_after_simulated_commission_usd": "0.00",
            "simulated_commission_usd": "0.00",
            "simulated_dividend_cash_usd": "0.00",
            "stock_split_events": [],
            "cash_dividend_events": [],
            "split_marks_in_prior_units": [],
            "open_positions": {},
            "signals": [],
            "fills": [],
            "intents": {},
            "warmup_quotes": 0,
            "stale_or_reordered_quotes": 0,
            "observation_gap_resets": 0,
            "mark_is_stale": True,
        }

    make_strategy = _trend_strategy(replay, config, start_at)

    def strategy_factory(instruments, orders, units):
        nonlocal strategy
        strategy = make_strategy(instruments, orders, units)
        return strategy

    strategy = None
    result = _run_replay(
        fixture,
        through=end_at,
        strategy_factory=strategy_factory,
    )
    assert strategy is not None
    strategy.expire_unexecuted(end_at)
    nav = Decimal(result["ending_bid_mark_nav_usd"])
    return {
        "parameters": asdict(config),
        "ending_cash_usd": result["ending_cash_usd"],
        "ending_bid_mark_nav_usd": result["ending_bid_mark_nav_usd"],
        "pnl_after_simulated_commission_usd": format(
            nav - Decimal(replay.starting_cash_usd), ".2f"
        ),
        "simulated_commission_usd": result["simulated_commission_usd"],
        "simulated_dividend_cash_usd": result.get("simulated_dividend_cash_usd", "0.00"),
        "stock_split_events": result.get("stock_split_events", []),
        "cash_dividend_events": result.get("cash_dividend_events", []),
        "split_marks_in_prior_units": result.get("split_marks_in_prior_units", []),
        "open_positions": result["open_positions"],
        "signals": strategy.signals,
        "fills": result["fills"],
        "intents": result["intents"],
        "warmup_quotes": strategy.warmup_quotes,
        "stale_or_reordered_quotes": strategy.stale_or_reordered_quotes,
        "observation_gap_resets": strategy.observation_gap_resets,
        "mark_is_stale": strategy.last_observed_ns is None
        or _nanoseconds(end_at) - strategy.last_observed_ns > strategy.max_gap_ns,
    }
