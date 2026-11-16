"""Chronological synthetic quote replay through Nautilus' native market simulator."""

import json
from collections import defaultdict, deque
from datetime import UTC, datetime
from decimal import Decimal
from hashlib import sha256
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
from nautilus_trader.model.objects import Money, Price, Quantity
from nautilus_trader.trading.strategy import Strategy

REPLAY_VERSION = "0.1"
MAX_INPUT_BYTES = 8 * 1024 * 1024
EPOCH = datetime(1970, 1, 1, tzinfo=UTC)
MoneyText = Annotated[str, msgspec.Meta(pattern=r"^(0|[1-9][0-9]{0,9})(\.[0-9]{1,2})?$")]
Ticker = Annotated[str, msgspec.Meta(pattern=r"^[A-Z][A-Z0-9.]{0,11}$")]
IntentID = Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")]
Timestamp = Annotated[datetime, msgspec.Meta(tz=True)]
Shares = Annotated[int, msgspec.Meta(ge=1, le=1_000_000)]


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


class Replay(msgspec.Struct, frozen=True, forbid_unknown_fields=True, omit_defaults=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture"]
    starting_cash_usd: MoneyText
    commission_bps: Annotated[int, msgspec.Meta(ge=0, le=1000)]
    quotes: Annotated[tuple[Quote, ...], msgspec.Meta(min_length=1)]
    orders: tuple[Order, ...]
    cash_dividends: tuple[CashDividend, ...] = ()


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


def load_replay(data: bytes) -> Replay:
    if len(data) > MAX_INPUT_BYTES:
        raise ValueError("Replay fixture exceeds the input limit")
    replay = msgspec.convert(json.loads(data, object_pairs_hook=_unique_fields), type=Replay)
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
    _validate_dividends(replay.cash_dividends, {symbol for symbol, _ in seen_quotes})
    return replay


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


class _DividendBoundary(Data):
    def __init__(self, timestamp_ns: int):
        self.timestamp_ns = timestamp_ns

    @property
    def ts_event(self):
        return self.timestamp_ns

    @property
    def ts_init(self):
        return self.timestamp_ns


class _DividendModule(SimulationModule):
    def __init__(
        self, dividends: tuple[CashDividend, ...], horizon_ns: int, instruments: dict[str, Equity]
    ):
        super().__init__(SimulationModuleConfig())
        self.instruments = instruments
        events = []
        for dividend in dividends:
            entitlement_ns = _nanoseconds(dividend.entitlement_at)
            paid_ns = _nanoseconds(dividend.paid_at)
            if entitlement_ns <= horizon_ns:
                events.append((entitlement_ns, False, dividend))
            if paid_ns <= horizon_ns:
                events.append((paid_ns, True, dividend))
        self.timeline = deque(
            sorted(events, key=lambda item: (item[0], item[1], item[2].action_id))
        )
        self.receipts = {}
        self.cash_received = Decimal(0)

    def boundaries(self) -> list[CustomData]:
        data_type = DataType(_DividendBoundary)
        return [
            CustomData(data_type, _DividendBoundary(timestamp_ns))
            for timestamp_ns in sorted({timestamp for timestamp, _, _ in self.timeline})
        ]

    def process(self, timestamp_ns: int) -> None:
        while self.timeline and self.timeline[0][0] <= timestamp_ns:
            boundary_ns, pay, dividend = self.timeline.popleft()
            if boundary_ns != timestamp_ns:
                raise RuntimeError("Nautilus passed a cash dividend boundary without settling it")
            if pay:
                receipt = self.receipts[dividend.action_id]
                amount = Decimal(receipt["entitled_shares"]) * Decimal(dividend.cash_per_share_usd)
                if amount:
                    self.exchange.adjust_account(Money.from_decimal(amount, USD))
                self.cash_received += amount
                receipt.update(status="paid", cash_credited_usd=_usd(amount))
            else:
                instrument = self.instruments[dividend.symbol]
                shares = sum(
                    (
                        position.quantity.as_decimal()
                        for position in self.exchange.cache.positions_open(
                            instrument_id=instrument.id
                        )
                    ),
                    Decimal(0),
                )
                self.receipts[dividend.action_id] = {
                    "action_id": dividend.action_id,
                    "symbol": dividend.symbol,
                    "entitlement_at_ns": str(boundary_ns),
                    "paid_at_ns": str(_nanoseconds(dividend.paid_at)),
                    "entitled_shares": str(shares),
                    "cash_credited_usd": "0.00",
                    "status": "pending_payment",
                }

    def log_diagnostics(self, logger) -> None:
        pass


class _ReplayStrategy(Strategy):
    def __init__(self, instruments: dict[str, Equity], orders: tuple[Order, ...]):
        super().__init__()
        self.instruments = instruments
        self.pending = defaultdict(deque)
        for order in sorted(orders, key=lambda item: (item.ready_at, item.intent_id)):
            self.pending[order.symbol].append(order)
        self.submitted = {}
        self.statuses = {order.intent_id: "no_executable_quote" for order in orders}
        self.fills = []

    def on_start(self):
        for instrument in self.instruments.values():
            self.subscribe_quote_ticks(instrument.id)

    def on_quote_tick(self, tick):
        queue = self.pending[tick.instrument_id.symbol.value]
        while queue:
            order = queue[0]
            if _nanoseconds(order.expires_at) <= tick.ts_init:
                self.statuses[order.intent_id] = "expired"
                queue.popleft()
                continue
            if _nanoseconds(order.ready_at) >= min(tick.ts_event, tick.ts_init):
                break
            queue.popleft()
            market_order = self.order_factory.market(
                instrument_id=tick.instrument_id,
                order_side=OrderSide.BUY if order.side == "BUY" else OrderSide.SELL,
                quantity=Quantity.from_int(order.quantity),
                time_in_force=TimeInForce.IOC,
                reduce_only=order.side == "SELL",
            )
            self.submitted[market_order.client_order_id] = (order.intent_id, tick.ts_init)
            self.submit_order(market_order)

    def on_order_filled(self, event):
        intent_id, quote_available_ns = self.submitted[event.client_order_id]
        self.fills.append(
            {
                "intent_id": intent_id,
                "symbol": event.instrument_id.symbol.value,
                "quantity": str(event.last_qty.as_decimal()),
                "price_usd": _usd(event.last_px.as_decimal()),
                "commission_usd": _usd(event.commission.as_decimal()),
                "quote_available_at_ns": str(quote_available_ns),
                "filled_at_ns": str(event.ts_event),
            }
        )


def run_replay(replay: Replay, *, through: datetime | None = None) -> dict:
    venue = Venue("PAPER")
    starting_cash = Decimal(replay.starting_cash_usd)
    fee_rate = Decimal(replay.commission_bps) / 10_000
    quotes = sorted(replay.quotes, key=lambda item: (item.available_at, item.symbol))
    horizon = quotes[-1].available_at if through is None else through
    if horizon.utcoffset() is None or horizon < quotes[-1].available_at:
        raise ValueError("Replay horizon must be timezone-aware and cover the supplied quotes")
    horizon_ns = _nanoseconds(horizon)
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
        for code in sorted({quote.symbol for quote in quotes})
    }
    _validate_dividends(replay.cash_dividends, set(instruments))
    dividend_module = (
        _DividendModule(replay.cash_dividends, horizon_ns, instruments)
        if replay.cash_dividends
        else None
    )
    strategy = _ReplayStrategy(instruments, replay.orders)
    engine = BacktestEngine(config=BacktestEngineConfig(logging=LoggingConfig(bypass_logging=True)))
    try:
        engine.add_venue(
            venue=venue,
            oms_type=OmsType.NETTING,
            account_type=AccountType.CASH,
            base_currency=USD,
            starting_balances=[Money.from_str(f"{_usd(starting_cash)} USD")],
            liquidity_consumption=True,
            modules=[dividend_module] if dividend_module is not None else None,
        )
        for instrument in instruments.values():
            engine.add_instrument(instrument)
        if dividend_module is not None and (boundaries := dividend_module.boundaries()):
            # Stable sort by availability gives corporate boundaries priority over
            # equal-time quotes. No artificial quote or trade advances the clock.
            engine.add_data(boundaries, client_id=ClientId("CORPORATE-ACTIONS"))
        engine.add_data(
            [
                QuoteTick(
                    instrument_id=instruments[quote.symbol].id,
                    bid_price=Price.from_str(_usd(Decimal(quote.bid_usd))),
                    ask_price=Price.from_str(_usd(Decimal(quote.ask_usd))),
                    bid_size=Quantity.from_int(quote.bid_size),
                    ask_size=Quantity.from_int(quote.ask_size),
                    ts_event=_nanoseconds(quote.observed_at),
                    ts_init=_nanoseconds(quote.available_at),
                )
                for quote in quotes
            ]
        )
        engine.add_strategy(strategy)
        engine.run()
        if dividend_module is not None and dividend_module.timeline:
            raise RuntimeError("Nautilus did not settle all cash dividend boundaries")
        for outcome in engine.cache.orders():
            intent_id, _ = strategy.submitted[outcome.client_order_id]
            strategy.statuses[intent_id] = outcome.status_string().lower()

        cash = engine.portfolio.account(venue).balance_total(USD).as_decimal()
        positions = {
            position.instrument_id.symbol.value: position.quantity.as_decimal()
            for position in engine.cache.positions_open()
        }
        marks = {
            quote.symbol: Decimal(quote.bid_usd)
            for quote in sorted(quotes, key=lambda item: item.observed_at)
        }
        nav = cash + sum(quantity * marks[symbol] for symbol, quantity in positions.items())
        commissions = sum((Decimal(fill["commission_usd"]) for fill in strategy.fills), Decimal(0))
        result = {
            "schema_version": REPLAY_VERSION,
            "source_mode": replay.source_mode,
            "alpha_measured": False,
            "intent_pipeline_connected": False,
            "normalized_input_sha256": sha256(msgspec.json.encode(replay)).hexdigest(),
            "starting_cash_usd": _usd(starting_cash),
            "ending_cash_usd": _usd(cash),
            "open_positions": {symbol: str(quantity) for symbol, quantity in positions.items()},
            "ending_bid_mark_nav_usd": _usd(nav),
            "cash_benchmark_nav_usd": _usd(starting_cash),
            "simulated_commission_usd": _usd(commissions),
            "external_costs_usd": None,
            "fills": strategy.fills,
            "intents": strategy.statuses,
        }
        if through is not None:
            result["evaluated_through_ns"] = str(horizon_ns)
        if dividend_module is not None:
            result["simulated_dividend_cash_usd"] = _usd(dividend_module.cash_received)
            result["cash_dividend_events"] = list(dividend_module.receipts.values())
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
