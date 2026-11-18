"""Chronological synthetic quote replay through Nautilus' native market simulator."""

import json
import re
from bisect import bisect_right
from collections import defaultdict, deque
from collections.abc import Callable, Sequence
from datetime import UTC, datetime
from decimal import Decimal
from enum import IntEnum
from fractions import Fraction
from hashlib import sha256
from operator import attrgetter, itemgetter
from typing import Annotated, Literal

import msgspec
from nautilus_trader.backtest.engine import BacktestEngine
from nautilus_trader.backtest.modules import SimulationModule
from nautilus_trader.config import BacktestEngineConfig, LoggingConfig, SimulationModuleConfig
from nautilus_trader.core.data import Data
from nautilus_trader.model.currencies import USD
from nautilus_trader.model.data import CustomData, DataType, QuoteTick
from nautilus_trader.model.enums import AccountType, OmsType, OrderSide, TimeInForce
from nautilus_trader.model.identifiers import ClientId, InstrumentId, Symbol, Venue
from nautilus_trader.model.instruments import Equity
from nautilus_trader.model.objects import PRICE_MAX, QUANTITY_MAX, Money, Price, Quantity
from nautilus_trader.trading.strategy import Strategy

REPLAY_VERSION = "0.1"
MAX_INPUT_BYTES = 8 * 1024 * 1024
MAX_PREFIX_EVENTS = 2048
MAX_PREFIX_SYMBOLS = 8
EPOCH = datetime(1970, 1, 1, tzinfo=UTC)
MoneyText = Annotated[str, msgspec.Meta(pattern=r"^(0|[1-9][0-9]{0,9})(\.[0-9]{1,2})?$")]
Ticker = Annotated[str, msgspec.Meta(pattern=r"^[A-Z][A-Z0-9.]{0,11}$")]
IntentID = Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")]
Timestamp = Annotated[datetime, msgspec.Meta(tz=True)]
Shares = Annotated[int, msgspec.Meta(ge=1, le=1_000_000)]
_FRACTIONAL_SECONDS = re.compile(r"\.([0-9]+)")


class Quote(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    symbol: Ticker
    observed_at: Timestamp
    available_at: Timestamp
    bid_usd: MoneyText
    ask_usd: MoneyText
    bid_size: Shares
    ask_size: Shares


class Order(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    intent_id: IntentID
    symbol: Ticker
    side: Literal["BUY", "SELL"]
    quantity: Shares
    ready_at: Timestamp
    expires_at: Timestamp


class CashDividend(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    action_id: IntentID
    symbol: Ticker
    available_at: Timestamp
    entitlement_at: Timestamp
    paid_at: Timestamp
    cash_per_share_usd: MoneyText


class StockSplit(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    action_id: IntentID
    symbol: Ticker
    available_at: Timestamp
    effective_at: Timestamp
    new_shares: Shares
    old_shares: Shares


class ExecutionWindow(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    record_id: IntentID
    symbol: Ticker
    starts_at: Timestamp
    ends_before: Timestamp
    available_at: Timestamp
    status: Literal["open", "closed", "halted"]


class ExecutionCalendar(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    calendar_id: IntentID
    windows: Annotated[tuple[ExecutionWindow, ...], msgspec.Meta(max_length=MAX_PREFIX_EVENTS)]


class Replay(msgspec.Struct, frozen=True, forbid_unknown_fields=True, omit_defaults=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture"]
    starting_cash_usd: MoneyText
    commission_bps: Annotated[int, msgspec.Meta(ge=0, le=1000)]
    quotes: Annotated[tuple[Quote, ...], msgspec.Meta(min_length=1)]
    orders: tuple[Order, ...]
    cash_dividends: tuple[CashDividend, ...] = ()
    stock_splits: tuple[StockSplit, ...] = ()
    execution_calendar: ExecutionCalendar | msgspec.UnsetType = msgspec.UNSET


def replay_event_count(replay: Replay) -> int:
    calendar = replay.execution_calendar
    return (
        len(replay.quotes)
        + len(replay.orders)
        + len(replay.cash_dividends)
        + len(replay.stock_splits)
        + (len(calendar.windows) if calendar is not msgspec.UNSET else 0)
    )


def replay_prefix(replay: Replay, through: datetime) -> Replay:
    quotes = tuple(quote for quote in replay.quotes if quote.available_at <= through)
    symbols = {quote.symbol for quote in quotes}
    calendar = replay.execution_calendar
    if calendar is not msgspec.UNSET:
        calendar = msgspec.structs.replace(
            calendar,
            windows=tuple(
                row
                for row in calendar.windows
                if row.symbol in symbols and row.available_at <= through
            ),
        )
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
        execution_calendar=calendar,
    )


def _nanoseconds(at: datetime) -> int:
    elapsed = at - EPOCH
    ns = ((elapsed.days * 86400 + elapsed.seconds) * 1_000_000 + elapsed.microseconds) * 1000
    if not 0 <= ns < 2**63:
        raise ValueError("Replay timestamp falls outside the native engine time range")
    return ns


def _usd(amount: Decimal) -> str:
    return format(amount, ".2f")


def _unique_fields(pairs):
    fields = dict(pairs)
    if len(fields) != len(pairs):
        raise ValueError("Duplicate replay object field")
    return fields


def replay_sha256(replay: Replay) -> str:
    return sha256(msgspec.json.encode(replay)).hexdigest()


def load_replay(data: bytes) -> Replay:
    if len(data) > MAX_INPUT_BYTES:
        raise ValueError("Replay fixture exceeds the input limit")
    document = json.loads(data, object_pairs_hook=_unique_fields)
    replay = msgspec.convert(document, type=Replay)
    if Decimal(replay.starting_cash_usd) <= 0:
        raise ValueError("Starting cash must be positive")
    seen_quotes = set()
    for quote in replay.quotes:
        _nanoseconds(quote.observed_at)
        _nanoseconds(quote.available_at)
        key = (quote.symbol, quote.available_at)
        if key in seen_quotes:
            raise ValueError("Ambiguous quote availability for one symbol")
        seen_quotes.add(key)
        if quote.available_at < quote.observed_at:
            raise ValueError("Quote cannot be available before observation")
        if not 0 < Decimal(quote.bid_usd) <= Decimal(quote.ask_usd):
            raise ValueError("Quote must have positive, uncrossed prices")

    seen_orders = set()
    for order in replay.orders:
        _nanoseconds(order.ready_at)
        _nanoseconds(order.expires_at)
        if order.intent_id in seen_orders:
            raise ValueError("Duplicate replay intent ID")
        if order.ready_at >= order.expires_at:
            raise ValueError("Order must expire after its earliest admission time")
        seen_orders.add(order.intent_id)
    symbols = {symbol for symbol, _ in seen_quotes}
    _validate_dividends(replay.cash_dividends, symbols)
    _validate_splits(replay, symbols)
    if replay.execution_calendar is not msgspec.UNSET:
        _validate_calendar_precision(document)
        _index_calendar(replay.execution_calendar, symbols)
    return replay


def _validate_calendar_precision(document: dict) -> None:
    fields_by_row = (
        (document["execution_calendar"]["windows"], ("starts_at", "ends_before", "available_at")),
        (document["quotes"], ("observed_at", "available_at")),
        (document["orders"], ("ready_at", "expires_at")),
        (document.get("stock_splits", ()), ("available_at", "effective_at")),
        (document.get("cash_dividends", ()), ("available_at", "entitlement_at", "paid_at")),
    )
    for rows, fields in fields_by_row:
        for row in rows:
            for field in fields:
                fraction = _FRACTIONAL_SECONDS.search(row[field])
                if fraction and fraction[1][6:].strip("0"):
                    raise ValueError("Execution calendar replay requires microsecond precision")


def _index_calendar(
    calendar: ExecutionCalendar, symbols: set[str] | None = None
) -> dict[str, list[ExecutionWindow]]:
    seen = set()
    by_symbol = defaultdict(list)
    for window in sorted(calendar.windows, key=attrgetter("starts_at")):
        _nanoseconds(window.available_at)
        _nanoseconds(window.starts_at)
        _nanoseconds(window.ends_before)
        if window.record_id in seen:
            raise ValueError("Execution calendar contains a duplicate record ID")
        if symbols is not None and window.symbol not in symbols:
            raise ValueError("Execution window needs a quoted symbol")
        if window.starts_at >= window.ends_before:
            raise ValueError("Execution window must start before it ends")
        previous = by_symbol[window.symbol]
        if previous and window.starts_at < previous[-1].ends_before:
            raise ValueError("Execution windows for one symbol cannot overlap")
        seen.add(window.record_id)
        previous.append(window)
    return by_symbol


def _calendar_quotes(
    quotes: Sequence[Quote], calendar: ExecutionCalendar | msgspec.UnsetType
) -> tuple[Sequence[Quote], dict[tuple[str, int], str], dict[str, int]]:
    if calendar is msgspec.UNSET:
        return quotes, {}, {}
    windows = _index_calendar(calendar)
    eligible = []
    known_open = {}
    blocked = defaultdict(int)
    for quote in quotes:
        by_symbol = windows.get(quote.symbol, ())
        at = quote.available_at
        index = bisect_right(by_symbol, at, key=attrgetter("starts_at")) - 1
        window = by_symbol[index] if index >= 0 else None
        if window is None or at >= window.ends_before or at < window.available_at:
            blocked["unknown"] += 1
        elif window.status != "open":
            blocked[window.status] += 1
        elif not window.starts_at <= quote.observed_at <= at:
            blocked["outside_observation_window"] += 1
        else:
            eligible.append(quote)
            known_open[quote.symbol, _nanoseconds(at)] = window.record_id
    return eligible, known_open, dict(blocked)


def executable_quotes(replay: Replay) -> tuple[Quote, ...]:
    """Read only quotes tied to a known execution window when a calendar is pinned."""
    eligible, _, _ = _calendar_quotes(replay.quotes, replay.execution_calendar)
    return tuple(eligible)


def _validate_dividends(dividends: tuple[CashDividend, ...], symbols: set[str]) -> None:
    seen = set()
    for dividend in dividends:
        available = _nanoseconds(dividend.available_at)
        entitlement = _nanoseconds(dividend.entitlement_at)
        paid = _nanoseconds(dividend.paid_at)
        if dividend.action_id in seen or dividend.symbol not in symbols:
            raise ValueError("Cash dividend requires a unique action ID and a quoted symbol")
        cash = Decimal(dividend.cash_per_share_usd)
        if not available <= entitlement < paid or cash <= 0:
            raise ValueError(
                "Cash dividend requires known entitlement before payment and positive cash"
            )
        if cash != Money.from_decimal(cash, USD).as_decimal():
            raise ValueError("Cash dividend must be representable in exact USD cents")
        seen.add(dividend.action_id)


def _validate_splits(replay: Replay, symbols: set[str]) -> None:
    seen_ids = {action.action_id for action in replay.cash_dividends}
    seen_times = set()
    for action in replay.stock_splits:
        available = _nanoseconds(action.available_at)
        effective = _nanoseconds(action.effective_at)
        key = (action.symbol, effective)
        if action.action_id in seen_ids or key in seen_times or action.symbol not in symbols:
            raise ValueError("Stock split requires a unique action ID, time and quoted symbol")
        if (
            available > effective
            or not 1 <= action.new_shares <= 1_000_000
            or not 1 <= action.old_shares <= 1_000_000
            or action.new_shares == action.old_shares
        ):
            raise ValueError("Stock split requires a known effective time and a valid share ratio")
        seen_ids.add(action.action_id)
        seen_times.add(key)


class _SplitUnits:
    """Use first-era share units in Nautilus, exchanging exact raw units at the boundary."""

    def __init__(self, splits: tuple[StockSplit, ...], horizon: datetime):
        self._epochs = defaultdict(list)
        for action in sorted(splits, key=lambda item: item.effective_at):
            if action.effective_at > horizon:
                break
            epochs = self._epochs[action.symbol]
            previous = epochs[-1][1] if epochs else Fraction(1)
            factor = previous * Fraction(action.new_shares, action.old_shares)
            if max(factor.numerator, factor.denominator) > QUANTITY_MAX:
                raise ValueError("Cumulative stock split ratio exceeds the native quantity limit")
            epochs.append((action.effective_at, factor))

    def _epoch(self, symbol: str, at: datetime) -> int:
        return bisect_right(self._epochs.get(symbol, ()), at, key=itemgetter(0))

    def factor_at(self, symbol: str, at: datetime) -> Fraction:
        epoch = self._epoch(symbol, at)
        return self._epochs[symbol][epoch - 1][1] if epoch else Fraction(1)

    def quote_is_current(self, quote: Quote, at: datetime) -> bool:
        return self.observation_is_current(quote.symbol, quote.observed_at, at)

    def observation_is_current(self, symbol: str, observed_at: datetime, at: datetime) -> bool:
        return self._epoch(symbol, observed_at) == self._epoch(symbol, at)


def _whole_shares(value: Fraction) -> int:
    if value.denominator != 1:
        raise ValueError("Stock split cannot be represented in whole shares on the native grid")
    return value.numerator


def _native_quantity(raw_quantity: int, factor: Fraction) -> Quantity:
    quantity = _whole_shares(raw_quantity / factor)
    if quantity > QUANTITY_MAX:
        raise ValueError("Stock split quantity exceeds the native quantity limit")
    return Quantity.from_int(quantity)


def _scaled_usd(value: Decimal, factor: Fraction) -> Decimal:
    cents = Fraction(value) * 100 * factor
    if cents.denominator != 1 or cents > int(PRICE_MAX) * 100:
        raise ValueError("Stock split price must fit in exact native USD cents")
    return Decimal(cents.numerator) / 100


class _CorporateActionBoundary(Data):
    def __init__(self, timestamp_ns: int):
        self.timestamp_ns = timestamp_ns

    @property
    def ts_event(self):
        return self.timestamp_ns

    @property
    def ts_init(self):
        return self.timestamp_ns


class _ActionStage(IntEnum):
    SPLIT = 0
    ENTITLEMENT = 1
    PAYMENT = 2


class _CorporateActions(SimulationModule):
    def __init__(
        self,
        replay: Replay,
        horizon_ns: int,
        strategy: "_ReplayStrategy",
    ):
        super().__init__(SimulationModuleConfig())
        self.strategy = strategy
        events = []
        for action in replay.stock_splits:
            split_ns = _nanoseconds(action.effective_at)
            if split_ns <= horizon_ns:
                events.append((split_ns, _ActionStage.SPLIT, action))
        for dividend in replay.cash_dividends:
            entitlement_ns = _nanoseconds(dividend.entitlement_at)
            paid_ns = _nanoseconds(dividend.paid_at)
            if entitlement_ns <= horizon_ns:
                events.append((entitlement_ns, _ActionStage.ENTITLEMENT, dividend))
            if paid_ns <= horizon_ns:
                events.append((paid_ns, _ActionStage.PAYMENT, dividend))
        self.timeline = deque(
            sorted(events, key=lambda item: (item[0], item[1], item[2].action_id))
        )
        self.receipts = {}
        self.split_events = []
        self.cash_received = Decimal(0)

    def boundaries(self) -> list[CustomData]:
        data_type = DataType(_CorporateActionBoundary)
        return [
            CustomData(data_type, _CorporateActionBoundary(timestamp_ns))
            for timestamp_ns in sorted({timestamp for timestamp, _, _ in self.timeline})
        ]

    def process(self, timestamp_ns: int) -> None:
        while self.timeline and self.timeline[0][0] <= timestamp_ns:
            boundary_ns, stage, action = self.timeline.popleft()
            if boundary_ns != timestamp_ns:
                raise RuntimeError(
                    "Nautilus passed a corporate action boundary without settling it"
                )
            if stage == _ActionStage.SPLIT:
                self._record_split(action, boundary_ns)
            elif stage == _ActionStage.PAYMENT:
                receipt = self.receipts[action.action_id]
                amount = Decimal(receipt["entitled_shares"]) * Decimal(action.cash_per_share_usd)
                if amount:
                    self.exchange.adjust_account(Money.from_decimal(amount, USD))
                self.cash_received += amount
                receipt.update(status="paid", cash_credited_usd=_usd(amount))
            else:
                shares = _whole_shares(
                    self._held_units(action.symbol)
                    * self.strategy.units.factor_at(action.symbol, action.entitlement_at)
                )
                self.receipts[action.action_id] = {
                    "action_id": action.action_id,
                    "symbol": action.symbol,
                    "entitlement_at_ns": str(boundary_ns),
                    "paid_at_ns": str(_nanoseconds(action.paid_at)),
                    "entitled_shares": str(shares),
                    "cash_credited_usd": "0.00",
                    "status": "pending_payment",
                }

    def _held_units(self, symbol: str) -> Fraction:
        instrument = self.strategy.instruments[symbol]
        return sum(
            (
                Fraction(position.quantity.as_decimal())
                for position in self.exchange.cache.positions_open(instrument_id=instrument.id)
            ),
            Fraction(0),
        )

    def _record_split(self, action: StockSplit, boundary_ns: int) -> None:
        held = self._held_units(action.symbol)
        after = held * self.strategy.units.factor_at(action.symbol, action.effective_at)
        before = after * Fraction(action.old_shares, action.new_shares)
        self.split_events.append(
            {
                "action_id": action.action_id,
                "symbol": action.symbol,
                "effective_at_ns": str(boundary_ns),
                "new_shares": action.new_shares,
                "old_shares": action.old_shares,
                "shares_before": str(_whole_shares(before)),
                "shares_after": str(_whole_shares(after)),
            }
        )
        self.strategy.cancel_prior_units(action)

    def log_diagnostics(self, logger) -> None:
        pass


class _ReplayStrategy(Strategy):
    def __init__(
        self, instruments: dict[str, Equity], orders: tuple[Order, ...], units: _SplitUnits
    ):
        super().__init__()
        self.instruments = instruments
        self.units = units
        self.pending = defaultdict(deque)
        for order in sorted(orders, key=lambda item: (item.ready_at, item.intent_id)):
            self.pending[order.symbol].append(order)
        self.submitted = {}
        self.statuses = {order.intent_id: "no_executable_quote" for order in orders}
        self.fills = []

    def on_start(self):
        for instrument in self.instruments.values():
            self.subscribe_quote_ticks(instrument.id)

    def cancel_prior_units(self, action: StockSplit) -> None:
        queue = self.pending[action.symbol]
        while queue and queue[0].ready_at < action.effective_at:
            order = queue.popleft()
            self.statuses[order.intent_id] = (
                "expired" if order.expires_at <= action.effective_at else "cancelled_stock_split"
            )

    def on_quote_tick(self, tick):
        symbol = tick.instrument_id.symbol.value
        queue = self.pending[symbol]
        while queue:
            order = queue[0]
            if _nanoseconds(order.expires_at) <= tick.ts_init:
                self.statuses[order.intent_id] = "expired"
                queue.popleft()
                continue
            if _nanoseconds(order.ready_at) >= min(tick.ts_event, tick.ts_init):
                break
            queue.popleft()
            factor = self.units.factor_at(symbol, order.ready_at)
            quantity = self.execution_quantity(order, tick, factor)
            if quantity == 0:
                self.statuses[order.intent_id] = "unaffordable_at_execution"
                continue
            market_order = self.order_factory.market(
                instrument_id=tick.instrument_id,
                order_side=OrderSide.BUY if order.side == "BUY" else OrderSide.SELL,
                quantity=_native_quantity(quantity, factor),
                time_in_force=TimeInForce.IOC,
                reduce_only=order.side == "SELL",
            )
            self.submitted[market_order.client_order_id] = (order.intent_id, tick.ts_init, factor)
            self.submit_order(market_order)

    def execution_quantity(self, order: Order, tick, factor: Fraction) -> int:
        return order.quantity

    def on_order_filled(self, event):
        intent_id, quote_available_ns, factor = self.submitted[event.client_order_id]
        self.fills.append(
            {
                "intent_id": intent_id,
                "symbol": event.instrument_id.symbol.value,
                "quantity": str(_whole_shares(Fraction(event.last_qty.as_decimal()) * factor)),
                "price_usd": _usd(_scaled_usd(event.last_px.as_decimal(), 1 / factor)),
                "commission_usd": _usd(event.commission.as_decimal()),
                "quote_available_at_ns": str(quote_available_ns),
                "filled_at_ns": str(event.ts_event),
            }
        )


def run_replay(replay: Replay, *, through: datetime | None = None) -> dict:
    return _run_replay(replay, through=through)


def _run_replay(
    replay: Replay,
    *,
    through: datetime | None = None,
    strategy_factory: Callable[
        [dict[str, Equity], tuple[Order, ...], _SplitUnits], _ReplayStrategy
    ] = _ReplayStrategy,
    strategy_data: tuple[list[CustomData], ClientId] | None = None,
) -> dict:
    venue = Venue("PAPER")
    starting_cash = Decimal(replay.starting_cash_usd)
    fee_rate = Decimal(replay.commission_bps) / 10_000
    quotes = sorted(replay.quotes, key=lambda item: (item.available_at, item.symbol))
    horizon = quotes[-1].available_at if through is None else through
    if horizon.utcoffset() is None or horizon < quotes[-1].available_at:
        raise ValueError("Replay horizon must be timezone-aware and cover the supplied quotes")
    horizon_ns = _nanoseconds(horizon)
    symbols = {quote.symbol for quote in quotes}
    _validate_dividends(replay.cash_dividends, symbols)
    _validate_splits(replay, symbols)
    units = _SplitUnits(replay.stock_splits, horizon)
    instruments = {
        code: Equity(
            instrument_id=InstrumentId(Symbol(code), venue),
            raw_symbol=Symbol(code),
            currency=USD,
            price_precision=2,
            price_increment=Price.from_str("0.01"),
            lot_size=Quantity.from_int(1),
            taker_fee=fee_rate,
            ts_event=0,
            ts_init=0,
        )
        for code in sorted(symbols)
    }
    strategy = strategy_factory(instruments, replay.orders, units)
    actions = _CorporateActions(replay, horizon_ns, strategy)
    boundaries = actions.boundaries()
    calendar = replay.execution_calendar
    session_quotes, window_ids, blocked_quotes = _calendar_quotes(quotes, calendar)
    eligible_quotes = [
        quote for quote in session_quotes if units.quote_is_current(quote, quote.available_at)
    ]
    ticks = []
    for quote in eligible_quotes:
        factor = units.factor_at(quote.symbol, quote.observed_at)
        ticks.append(
            QuoteTick(
                instrument_id=instruments[quote.symbol].id,
                bid_price=Price.from_str(_usd(_scaled_usd(Decimal(quote.bid_usd), factor))),
                ask_price=Price.from_str(_usd(_scaled_usd(Decimal(quote.ask_usd), factor))),
                bid_size=_native_quantity(quote.bid_size, factor),
                ask_size=_native_quantity(quote.ask_size, factor),
                ts_event=_nanoseconds(quote.observed_at),
                ts_init=_nanoseconds(quote.available_at),
            )
        )
    engine = BacktestEngine(config=BacktestEngineConfig(logging=LoggingConfig(bypass_logging=True)))
    try:
        engine.add_venue(
            venue=venue,
            oms_type=OmsType.NETTING,
            account_type=AccountType.CASH,
            base_currency=USD,
            starting_balances=[Money.from_str(f"{_usd(starting_cash)} USD")],
            liquidity_consumption=True,
            modules=[actions] if boundaries else None,
        )
        for instrument in instruments.values():
            engine.add_instrument(instrument)
        if boundaries:
            # Stable sort by availability gives corporate boundaries priority over
            # equal-time quotes. No artificial quote or trade advances the clock.
            engine.add_data(boundaries, client_id=ClientId("CORPORATE-ACTIONS"))
        if ticks:
            engine.add_data(ticks)
        if strategy_data is not None:
            data, client_id = strategy_data
            # A phase may use quotes available exactly at its cutoff. Their
            # ticks precede equal-time strategy data in the stable sort.
            engine.add_data(data, client_id=client_id)
        engine.add_strategy(strategy)
        if ticks or boundaries or strategy_data is not None:
            engine.run()
        if actions.timeline:
            raise RuntimeError("Nautilus did not settle all corporate action boundaries")
        booked_units = defaultdict(int)
        for fill in strategy.fills:
            booked_units[fill["intent_id"]] += int(fill["quantity"])
        for outcome in engine.cache.orders():
            intent_id, _, factor = strategy.submitted[outcome.client_order_id]
            status = outcome.status_string().lower()
            if status == "filled":
                # An IOC gap can leave a filled order-cache status without an
                # account fill callback. Report only what was actually booked.
                booked = booked_units[intent_id]
                requested = _whole_shares(Fraction(outcome.quantity.as_decimal()) * factor)
                if booked < requested:
                    status = "partially_filled" if booked else "no_accounted_fill"
            strategy.statuses[intent_id] = status

        cash = (
            engine.portfolio.account(venue).balance_total(USD).as_decimal()
            if ticks or boundaries or strategy_data is not None
            else starting_cash
        )
        positions = {
            position.instrument_id.symbol.value: position.quantity.as_decimal()
            for position in engine.cache.positions_open()
        }
        marks = {
            quote.symbol: quote
            for quote in sorted(eligible_quotes, key=lambda item: item.observed_at)
        }
        nav = cash
        for symbol, quantity in positions.items():
            mark = marks[symbol]
            nav += quantity * _scaled_usd(
                Decimal(mark.bid_usd), units.factor_at(symbol, mark.observed_at)
            )
        raw_positions = {
            symbol: str(_whole_shares(Fraction(quantity) * units.factor_at(symbol, horizon)))
            for symbol, quantity in positions.items()
        }
        commissions = sum((Decimal(fill["commission_usd"]) for fill in strategy.fills), Decimal(0))
        result = {
            "schema_version": REPLAY_VERSION,
            "source_mode": replay.source_mode,
            "alpha_measured": False,
            "intent_pipeline_connected": False,
            "normalized_input_sha256": replay_sha256(replay),
            "starting_cash_usd": _usd(starting_cash),
            "ending_cash_usd": _usd(cash),
            "open_positions": raw_positions,
            "ending_bid_mark_nav_usd": _usd(nav),
            "cash_benchmark_nav_usd": _usd(starting_cash),
            "simulated_commission_usd": _usd(commissions),
            "external_costs_usd": None,
            "fills": strategy.fills,
            "intents": strategy.statuses,
        }
        if through is not None:
            result["evaluated_through_ns"] = str(horizon_ns)
            result["evaluated_through"] = horizon.astimezone(UTC).isoformat().replace("+00:00", "Z")
        if replay.cash_dividends:
            result["simulated_dividend_cash_usd"] = _usd(actions.cash_received)
            result["cash_dividend_events"] = list(actions.receipts.values())
        if replay.stock_splits:
            result["stock_split_events"] = actions.split_events
            result["split_marks_in_prior_units"] = sorted(
                symbol for symbol in positions if not units.quote_is_current(marks[symbol], horizon)
            )
        if calendar is not msgspec.UNSET:
            for order in replay.orders:
                if (
                    order.expires_at <= horizon
                    and strategy.statuses[order.intent_id] == "no_executable_quote"
                ):
                    strategy.statuses[order.intent_id] = "expired"
            for fill in strategy.fills:
                fill["execution_window_id"] = window_ids[
                    fill["symbol"], int(fill["quote_available_at_ns"])
                ]
            result["execution_calendar"] = {
                "calendar_id": calendar.calendar_id,
                "policy": "observation_and_arrival_in_same_window_known_at_arrival",
                "eligible_quotes": len(session_quotes),
                "blocked_quotes": blocked_quotes,
                "held_bid_marks": {
                    symbol: {
                        "quote_observed_at_ns": str(_nanoseconds(marks[symbol].observed_at)),
                        "quote_available_at_ns": str(_nanoseconds(marks[symbol].available_at)),
                        "execution_window_id": window_ids[
                            symbol, _nanoseconds(marks[symbol].available_at)
                        ],
                    }
                    for symbol in sorted(positions)
                },
            }
        return result
    finally:
        engine.dispose()


def synthetic_probe() -> dict:
    ready = datetime(2026, 10, 2, 13, 29, tzinfo=UTC)
    observed = datetime(2026, 10, 2, 13, 30, tzinfo=UTC)
    expiry = datetime(2026, 10, 2, 13, 31, tzinfo=UTC)
    quantities = {"TSLA": 1, "NVDA": 2}
    fixture = Replay(
        schema_version=REPLAY_VERSION,
        source_mode="synthetic_fixture",
        starting_cash_usd="1000.00",
        commission_bps=0,
        quotes=tuple(
            Quote(symbol, observed, observed, "99.99", "100.00", 1000, 1000)
            for symbol in quantities
        ),
        orders=tuple(
            Order(symbol, symbol, "BUY", quantity, ready, expiry)
            for symbol, quantity in quantities.items()
        ),
    )
    return run_replay(fixture)
