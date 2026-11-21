from decimal import Decimal

from nautilus_trader.core.nautilus_pyo3 import UUID4, backtest
from nautilus_trader.core.nautilus_pyo3 import model as native
from nautilus_trader.core.nautilus_pyo3.trading import Strategy


def test_native_margin_engine_settles_funding_on_both_sides():
    venue = native.Venue("PERPPROBE")
    usd = native.Currency.from_str("USD")
    price = native.Price.from_str("100.00")
    depth = native.Quantity.from_int(50)
    start = 1_791_216_000_000_000_000
    second = 1_000_000_000
    settlement_at = start + 3 * second
    short = native.InstrumentId.from_str("TSLA-PERP.PERPPROBE")
    long = native.InstrumentId.from_str("NVDA-PERP.PERPPROBE")
    entries = {
        short: (native.OrderSide.SELL, native.Quantity.from_int(4)),
        long: (native.OrderSide.BUY, native.Quantity.from_int(2)),
    }

    class OpenOnFirstQuote(Strategy):
        def on_start(self):
            for instrument_id in entries:
                self.subscribe_quotes(instrument_id)

        def on_quote(self, quote):
            if quote.ts_event != start:
                return
            side, quantity = entries[quote.instrument_id]
            order = native.MarketOrder(
                trader_id=self.trader_id,
                strategy_id=self.strategy_id,
                instrument_id=quote.instrument_id,
                client_order_id=native.ClientOrderId(f"PROBE-{quote.instrument_id.symbol}"),
                order_side=side,
                quantity=quantity,
                init_id=UUID4(),
                ts_init=self.clock.timestamp_ns(),
                time_in_force=native.TimeInForce.GTC,
                reduce_only=False,
                quote_quantity=False,
                contingency_type=native.ContingencyType.NO_CONTINGENCY,
            )
            self.submit_order(order)

    engine = backtest.BacktestEngine(config=backtest.BacktestEngineConfig(run_analysis=False))
    try:
        engine.add_venue(
            venue=venue,
            oms_type=native.OmsType.NETTING,
            account_type=native.AccountType.MARGIN,
            starting_balances=[native.Money.from_str("1000.00 USD")],
            base_currency=usd,
            default_leverage=Decimal(2),
        )
        data = []
        for instrument_id in entries:
            engine.add_instrument(native.CryptoPerpetual(
                instrument_id=instrument_id,
                raw_symbol=instrument_id.symbol,
                base_currency=usd,
                quote_currency=usd,
                settlement_currency=usd,
                is_inverse=False,
                price_precision=2,
                size_precision=0,
                price_increment=native.Price.from_str("0.01"),
                size_increment=native.Quantity.from_int(1),
                ts_event=0,
                ts_init=0,
                margin_init=Decimal("0.5"),
                margin_maint=Decimal("0.05"),
            ))
            for stamp in (start, start + 4 * second):
                data.append(native.QuoteTick(instrument_id, price, price, depth, depth, stamp, stamp))
            data.append(native.MarkPriceUpdate(instrument_id, price, start + second, start + second))
            data.append(native.FundingRateUpdate(
                instrument_id, Decimal("0.01"), start + 2 * second, start + 2 * second,
                interval=60, next_funding_ns=settlement_at,
            ))
        engine.add_data(data)
        engine.add_strategy(OpenOnFirstQuote())
        engine.run()

        positions = {position.instrument_id: position for position in engine.cache.positions_open()}
        assert set(positions) == {short, long}
        for instrument_id, side, funding in (
            (short, native.PositionSide.SHORT, Decimal("4")),
            (long, native.PositionSide.LONG, Decimal("-2")),
        ):
            position = positions[instrument_id]
            assert position.side == side
            assert position.realized_pnl.as_decimal() == funding
            adjustment, = position.adjustments()
            assert adjustment.ts_event == settlement_at
            assert adjustment.pnl_change.as_decimal() == funding
    finally:
        engine.dispose()
