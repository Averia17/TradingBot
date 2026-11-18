"""Scheduled equal-weight baseline using confirmed native cash and fills."""

from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from fractions import Fraction
from hashlib import sha256
from itertools import pairwise
from typing import Annotated

import msgspec
from nautilus_trader.core.data import Data
from nautilus_trader.model.currencies import USD
from nautilus_trader.model.data import CustomData, DataType
from nautilus_trader.model.identifiers import ClientId, Venue

from tradingbot_evaluation.replay import (
    _FRACTIONAL_SECONDS,
    EPOCH,
    Order,
    Replay,
    Timestamp,
    _nanoseconds,
    _ReplayStrategy,
    _run_replay,
    _scaled_usd,
    _whole_shares,
)


def parse_rebalance_at(value: str) -> datetime:
    fraction = _FRACTIONAL_SECONDS.search(value)
    if fraction and fraction[1][6:].strip("0"):
        raise ValueError("Equal-weight cutoffs require microsecond precision")
    return msgspec.convert(value, type=Timestamp)


class EqualWeightSchedule(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    rebalance_at: Annotated[tuple[Timestamp, ...], msgspec.Meta(min_length=1, max_length=64)]
    execution_phase_seconds: Annotated[int, msgspec.Meta(ge=1, le=3600)]
    max_mark_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]

    def __post_init__(self):
        if not 1 <= len(self.rebalance_at) <= 64:
            raise ValueError("Equal-weight schedule requires 1 to 64 rebalance cutoffs")
        if any(not isinstance(at, datetime) or at.utcoffset() is None for at in self.rebalance_at):
            raise ValueError("Equal-weight cutoffs require a timezone offset")
        normalized = tuple(at.astimezone(UTC) for at in self.rebalance_at)
        if normalized != tuple(sorted(set(normalized))):
            raise ValueError("Equal-weight cutoffs must be strictly increasing")
        msgspec.structs.force_setattr(self, "rebalance_at", normalized)
        if (
            type(self.execution_phase_seconds) is not int
            or not 1 <= self.execution_phase_seconds <= 3600
        ):
            raise ValueError("Equal-weight execution phase must be between 1 and 3600 seconds")
        if (
            type(self.max_mark_age_seconds) is not int
            or not 1 <= self.max_mark_age_seconds <= 604800
        ):
            raise ValueError("Equal-weight mark age must be between 1 and 604800 seconds")
        for at in normalized:
            _nanoseconds(at)
            _nanoseconds(at + timedelta(seconds=2 * self.execution_phase_seconds))

    def validate_window(self, start_at: datetime, end_at: datetime) -> None:
        cycles = _cycles(self, start_at)
        if cycles[-1].funding >= end_at:
            raise ValueError(
                "Equal-weight schedule must leave a funding phase inside the evaluation"
            )


@dataclass(frozen=True, slots=True)
class _Cycle:
    cutoff: datetime
    funding: datetime
    expires: datetime


class _PhaseBoundary(Data):
    def __init__(self, index: int, phase: str, at: datetime):
        self.index, self.phase, self.at = index, phase, at
        self.timestamp_ns = _nanoseconds(at)

    @property
    def ts_event(self):
        return self.timestamp_ns

    @property
    def ts_init(self):
        return self.timestamp_ns


_PHASE_TYPE = DataType(_PhaseBoundary)
_PHASE_CLIENT = ClientId("BENCHMARK-BOUNDARIES")


class _ScheduledStrategy(_ReplayStrategy):
    def __init__(self, instruments, orders, units, *, cycles, universe, config, commission_bps):
        super().__init__(instruments, orders, units)
        self.cycles = cycles
        self.universe = universe
        self.mark_age_ns = config.max_mark_age_seconds * 1_000_000_000
        self.commission_bps = commission_bps
        self.marks = {}
        self.targets = {}
        self.sleeve_cents = 0
        self.buy_budgets = {}
        self.rebalances = []

    def on_start(self):
        super().on_start()
        self.subscribe_data(_PHASE_TYPE, _PHASE_CLIENT)

    def on_quote_tick(self, tick):
        symbol = tick.instrument_id.symbol.value
        prior = self.marks.get(symbol)
        if prior is None or tick.ts_event >= prior.ts_event:
            self.marks[symbol] = tick
        super().on_quote_tick(tick)

    def on_data(self, boundary):
        cycle = self.cycles[boundary.index]
        self.expire_orders(boundary.at)
        if boundary.phase == "formation":
            self._target(boundary.index, cycle, place_sales=False)
            self._buy(boundary.index, cycle)
        elif boundary.phase == "target":
            self._target(boundary.index, cycle, place_sales=True)
        else:
            self._buy(boundary.index, cycle)

    def expire_orders(self, at):
        for queue in self.pending.values():
            while queue and queue[0].expires_at <= at:
                order = queue.popleft()
                self.statuses[order.intent_id] = "expired"

    def execution_quantity(self, order, tick, factor):
        if order.side == "SELL":
            return order.quantity
        ask = int(_scaled_usd(tick.ask_price.as_decimal(), 1 / factor) * 100)
        budget = min(self.buy_budgets[order.intent_id], self._free_cash())
        quantity = min(order.quantity, self._affordable(budget, ask))
        return quantity - quantity % factor.numerator

    def _free_cash(self):
        account = self.portfolio.account(Venue("PAPER"))
        return int(account.balance_free(USD).as_decimal() * 100)

    def _affordable(self, budget, ask):
        return max(0, budget) * 10_000 // (ask * (10_000 + self.commission_bps))

    def _account(self, cutoff):
        positions = {
            item.instrument_id.symbol.value: _whole_shares(Fraction(item.quantity.as_decimal()))
            for item in self.cache.positions_open()
        }
        return self._free_cash(), positions, self._prices_at(cutoff)

    def _prices_at(self, cutoff):
        phase_ns = _nanoseconds(cutoff)
        prices = {}
        for symbol in self.universe:
            tick = self.marks.get(symbol)
            if (
                tick is None
                or tick.ts_init > phase_ns
                or phase_ns - tick.ts_event > self.mark_age_ns
            ):
                continue
            observed = EPOCH + timedelta(microseconds=tick.ts_event // 1000)
            factor = self.units.factor_at(symbol, cutoff)
            if not self.units.observation_is_current(symbol, observed, cutoff):
                continue
            prices[symbol] = (
                int(_scaled_usd(tick.bid_price.as_decimal(), 1 / factor) * 100),
                int(_scaled_usd(tick.ask_price.as_decimal(), 1 / factor) * 100),
                factor,
            )
        return prices

    def _target(self, index, cycle, *, place_sales):
        cash, holdings, prices = self._account(cycle.cutoff)
        self.targets = {}
        record = {
            "cutoff": cycle.cutoff.isoformat(),
            "buy_sizing_at": cycle.funding.isoformat(),
            "expires_at": cycle.expires.isoformat(),
            "symbols_without_fresh_mark": sorted(set(self.universe) - set(prices)),
            "status": "sized",
            "target_quantity": {},
        }
        self.rebalances.append(record)
        if set(holdings) - set(prices):
            record["status"] = "unvalued_holding"
            return

        nav = cash + sum(
            _whole_shares(quantity * prices[symbol][2]) * prices[symbol][0]
            for symbol, quantity in holdings.items()
        )
        self.sleeve_cents = nav // len(self.universe)
        for symbol, (bid, _, factor) in prices.items():
            target_raw = self.sleeve_cents // bid
            target_raw -= target_raw % factor.numerator
            target = _whole_shares(target_raw / factor)
            self.targets[symbol] = target
            record["target_quantity"][symbol] = str(target_raw)
            if place_sales:
                reduction = max(0, holdings.get(symbol, 0) - target)
                quantity = _whole_shares(reduction * factor)
                self._queue(index, symbol, "SELL", quantity, cycle.cutoff, cycle.funding, factor)

    def _buy(self, index, cycle):
        cash, holdings, prices = self._account(cycle.funding)
        record = self.rebalances[index]
        record["free_cash_before_buy_usd"] = format(Decimal(cash) / 100, ".2f")
        plan = {}
        for symbol, target in self.targets.items():
            if symbol not in prices or target <= holdings.get(symbol, 0):
                continue
            bid, ask, factor = prices[symbol]
            raw = (target - holdings.get(symbol, 0)) * factor
            quantity = min(int(raw), 1_000_000)
            quantity -= quantity % factor.numerator
            held_value = _whole_shares(holdings.get(symbol, 0) * factor) * bid
            available = self.sleeve_cents - held_value
            if quantity > 0 and available > 0:
                gross = quantity * ask
                fee = (gross * self.commission_bps + 9_999) // 10_000
                plan[symbol] = (quantity, ask, factor, min(available, gross + fee))

        total = sum(item[3] for item in plan.values())
        for symbol, (target, ask, factor, cost) in plan.items():
            # Allocate only cash actually visible after the sell interval.
            # Integer division preserves any unallocated fractional cent as cash.
            budget = min(cost, max(0, cash) * cost // total)
            affordable = self._affordable(budget, ask)
            intent = self._queue(
                index, symbol, "BUY", min(target, affordable), cycle.funding, cycle.expires, factor
            )
            if intent is not None:
                self.buy_budgets[intent] = budget

    def _queue(self, index, symbol, side, quantity, ready, expires, factor):
        quantity = min(quantity, 1_000_000)
        quantity -= quantity % factor.numerator
        if quantity <= 0:
            return
        intent = f"ew:{index}:{side.lower()}:{symbol}"
        self.pending[symbol].append(Order(intent, symbol, side, quantity, ready, expires))
        self.statuses[intent] = "no_executable_quote"
        return intent


def _cycles(config: EqualWeightSchedule, start_at: datetime) -> tuple[_Cycle, ...]:
    cutoffs = (start_at, *config.rebalance_at)
    phase = timedelta(seconds=config.execution_phase_seconds)
    if any(right - left < 2 * phase for left, right in pairwise(cutoffs)):
        raise ValueError("Equal-weight execution windows overlap or precede benchmark start")
    return tuple(
        _Cycle(
            cutoff,
            cutoff if index == 0 else cutoff + phase,
            cutoff + 2 * phase,
        )
        for index, cutoff in enumerate(cutoffs)
    )


def run_scheduled_equal_weight(
    replay: Replay,
    *,
    config: EqualWeightSchedule,
    start_at: datetime,
    end_at: datetime,
    universe: tuple[str, ...],
) -> dict:
    if not universe:
        raise ValueError("Scheduled equal weight requires an initial executable universe")
    cycles = _cycles(config, start_at)
    boundaries = []
    for index, cycle in enumerate(cycles):
        if index == 0:
            phases = (("formation", cycle.cutoff),)
        else:
            phases = (("target", cycle.cutoff), ("funding", cycle.funding))
        boundaries.extend(
            CustomData(_PHASE_TYPE, _PhaseBoundary(index, phase, at))
            for phase, at in phases
            if at <= end_at
        )

    strategy = None

    def strategy_factory(instruments, orders, units):
        nonlocal strategy
        strategy = _ScheduledStrategy(
            instruments,
            orders,
            units,
            cycles=cycles,
            universe=universe,
            config=config,
            commission_bps=replay.commission_bps,
        )
        return strategy

    result = _run_replay(
        msgspec.structs.replace(replay, orders=()),
        through=end_at,
        strategy_factory=strategy_factory,
        strategy_data=(boundaries, _PHASE_CLIENT),
    )
    reached = sum(cycle.cutoff <= end_at for cycle in cycles)
    if strategy is None or len(strategy.rebalances) != reached:
        raise RuntimeError("Native engine missed an equal-weight cutoff within the horizon")
    strategy.expire_orders(end_at)
    parameters = {
        "start_at": start_at.isoformat(),
        "rebalance_at": [at.isoformat() for at in config.rebalance_at],
        "execution_phase_seconds": config.execution_phase_seconds,
        "max_mark_age_seconds": config.max_mark_age_seconds,
    }
    return {
        "parameters": parameters,
        "schedule_sha256": sha256(
            msgspec.json.encode(parameters, order="deterministic")
        ).hexdigest(),
        "initial_universe": list(universe),
        "rebalances": strategy.rebalances,
        "ending_cash_usd": result["ending_cash_usd"],
        "ending_bid_mark_nav_usd": result["ending_bid_mark_nav_usd"],
        "pnl_after_simulated_commission_usd": format(
            Decimal(result["ending_bid_mark_nav_usd"]) - Decimal(replay.starting_cash_usd), ".2f"
        ),
        "simulated_commission_usd": result["simulated_commission_usd"],
        "simulated_dividend_cash_usd": result.get("simulated_dividend_cash_usd", "0.00"),
        "stock_split_events": result.get("stock_split_events", []),
        "cash_dividend_events": result.get("cash_dividend_events", []),
        "split_marks_in_prior_units": result.get("split_marks_in_prior_units", []),
        "open_positions": result["open_positions"],
        "ending_symbols_without_fresh_mark": sorted(
            set(result["open_positions"]) - set(strategy._prices_at(end_at))
        ),
        "intents": result["intents"],
        "fills": result["fills"],
    }
