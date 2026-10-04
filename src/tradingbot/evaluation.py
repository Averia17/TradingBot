"""Native-engine compatibility probe, not a historical alpha backtest."""

from datetime import UTC, datetime


def run_synthetic_probe() -> dict:
    # Lazy optional imports: the AI/contract milestone can run without this extra.
    from nautilus_trader.backtest.engine import BacktestEngine
    from nautilus_trader.config import BacktestEngineConfig, LoggingConfig
    from nautilus_trader.model.currencies import USD
    from nautilus_trader.model.data import QuoteTick
    from nautilus_trader.model.enums import AccountType, OmsType, OrderSide, OrderStatus
    from nautilus_trader.model.identifiers import InstrumentId, Symbol, Venue
    from nautilus_trader.model.instruments import Equity
    from nautilus_trader.model.objects import Money, Price, Quantity
    from nautilus_trader.trading.strategy import Strategy

    venue = Venue("PAPER")
    engine = BacktestEngine(config=BacktestEngineConfig(logging=LoggingConfig(bypass_logging=True)))
    instruments = [
        Equity(
            instrument_id=InstrumentId(Symbol(symbol), venue),
            raw_symbol=Symbol(symbol),
            currency=USD,
            price_precision=2,
            price_increment=Price.from_str("0.01"),
            lot_size=Quantity.from_int(1),
            ts_event=0,
            ts_init=0,
        )
        for symbol in ("TSLA", "NVDA")
    ]

    class SyntheticOrders(Strategy):
        """Fixed fixture orders; explicitly no AI signal or profitability claim."""

        def __init__(self):
            super().__init__()
            self.submitted = set()

        def on_start(self):
            for instrument in instruments:
                self.subscribe_quote_ticks(instrument.id)

        def on_quote_tick(self, tick):
            if tick.instrument_id in self.submitted:
                return
            self.submitted.add(tick.instrument_id)
            quantity = 1 if tick.instrument_id.symbol.value == "TSLA" else 2
            self.submit_order(
                self.order_factory.market(
                    instrument_id=tick.instrument_id,
                    order_side=OrderSide.BUY,
                    quantity=Quantity.from_int(quantity),
                )
            )

    try:
        engine.add_venue(
            venue=venue,
            oms_type=OmsType.NETTING,
            account_type=AccountType.CASH,
            base_currency=USD,
            starting_balances=[Money(1000, USD)],
        )
        base = int(datetime(2026, 10, 2, 13, 30, tzinfo=UTC).timestamp() * 1_000_000_000)
        for index, instrument in enumerate(instruments):
            engine.add_instrument(instrument)
            engine.add_data(
                [
                    QuoteTick(
                        instrument_id=instrument.id,
                        bid_price=Price.from_str("99.99"),
                        ask_price=Price.from_str("100.00"),
                        bid_size=Quantity.from_int(1000),
                        ask_size=Quantity.from_int(1000),
                        ts_event=base + step * 1_000_000_000 + index,
                        ts_init=base + step * 1_000_000_000 + index,
                    )
                    for step in range(3)
                ]
            )
        engine.add_strategy(SyntheticOrders())
        engine.run()
        return {
            "source_mode": "synthetic_fixture",
            "alpha_measured": False,
            "intent_pipeline_connected": False,
            "starting_cash_usd": "1000.00",
            "ending_cash_usd": str(engine.portfolio.account(venue).balance_total(USD).as_decimal()),
            "open_positions": {
                position.instrument_id.symbol.value: str(position.quantity.as_decimal())
                for position in engine.cache.positions_open()
            },
            "filled_orders": sum(
                order.status == OrderStatus.FILLED for order in engine.cache.orders()
            ),
        }
    finally:
        engine.dispose()
