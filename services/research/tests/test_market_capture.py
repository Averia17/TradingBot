from copy import deepcopy
from datetime import UTC, datetime
from decimal import Decimal
from types import SimpleNamespace

import pandas as pd
import pytest
from yfinance.scrapers.history import HistoryMetadata

from tradingbot import market_capture

NOW = datetime(2026, 11, 18, 20, 50, 6, tzinfo=UTC)


class FixedClock(datetime):
    @classmethod
    def now(cls, tz=None):
        return NOW.astimezone(tz)


@pytest.fixture
def source(monkeypatch):
    dates = pd.bdate_range(end="2026-11-18", periods=230, tz="America/New_York")
    closes = [100 + i / 10 for i in range(len(dates))]
    history = pd.DataFrame(
        {
            "Open": closes,
            "High": [p + 2 for p in closes],
            "Low": [p - 2 for p in closes],
            "Close": closes,
            "Volume": [100_000] * len(dates),
        },
        index=dates,
    )
    metadata = {
        "symbol": "NVDA",
        "currency": "USD",
        "exchangeTimezoneName": "America/New_York",
        "regularMarketPrice": 150.81,
        "regularMarketTime": int(NOW.timestamp()) - 3,
        "currentTradingPeriod": {
            "pre": {
                "start": int(NOW.replace(hour=9, minute=0).timestamp()),
                "end": int(NOW.replace(hour=14, minute=30).timestamp()),
            },
            "regular": {
                "start": int(NOW.replace(hour=14, minute=30).timestamp()),
                "end": int(NOW.replace(hour=21, minute=0).timestamp()),
            },
            "post": {
                "start": int(NOW.replace(hour=21, minute=0).timestamp()),
                "end": int(NOW.replace(day=19, hour=1, minute=0).timestamp()),
            },
        },
    }
    calls = []

    class Ticker:
        def history(self, **kwargs):
            calls.append(kwargs)
            return history.copy()

        def get_history_metadata(self):
            return deepcopy(metadata)

    def ticker(symbol):
        assert symbol == "NVDA"
        return Ticker()

    monkeypatch.setattr(market_capture, "datetime", FixedClock)
    monkeypatch.setattr(market_capture.yf, "Ticker", ticker)
    return history, metadata, calls


def test_capture_uses_completed_history_and_exact_source_timestamps(source):
    history, _, calls = source
    history.loc[history.index[-1], ["Open", "High", "Low", "Close"]] = [9999, 10001, 9998, 10000]
    history.loc[pd.Timestamp("2026-11-19", tz="America/New_York")] = [
        20000,
        20002,
        19998,
        20000,
        100000,
    ]
    price, technical = market_capture.capture_market("NVDA")
    assert price.record_id == "market-price" and price.payload["price_usd"] == "150.81"
    assert (
        price.event_time == NOW.replace(second=3) and price.available_at == price.ingested_at == NOW
    )
    assert price.payload["session"] == "regular"
    assert price.payload["quote_type"] == "regular_market_last_trade"
    assert technical.record_id == "completed-daily-technical"
    assert technical.payload["session_date"] == "2026-11-17"
    assert technical.event_time.date().isoformat() == "2026-11-17"
    assert technical.payload["row_count"] == 229
    assert technical.payload["close"] == "122.80"
    assert technical.payload["volume"] == 100000
    indicators = technical.payload["indicators"]
    assert set(indicators) == {"sma20", "sma50", "sma200", "rsi14", "macd", "macd_signal", "atr14"}
    assert Decimal(indicators["sma20"]) == Decimal("121.85")
    assert Decimal(indicators["sma200"]) == Decimal("112.85")
    assert all(isinstance(value, str) for value in indicators.values())
    assert calls == [
        {
            "period": "2y",
            "interval": "1d",
            "auto_adjust": False,
            "actions": False,
            "prepost": False,
            "timeout": 15,
        }
    ]
    assert all(
        record.provider == "yahoo-finance" and record.kind == "bar" for record in (price, technical)
    )
    assert all(
        record.payload["source_url"] == "https://finance.yahoo.com/quote/NVDA/history/"
        for record in (price, technical)
    )


def test_capture_omits_indicators_without_their_full_warmup(source):
    history, _, _ = source
    history.drop(history.index[:-6], inplace=True)
    _, technical = market_capture.capture_market("NVDA")
    assert technical.payload["row_count"] == 5
    assert technical.payload["indicators"] == {}


def test_capture_accepts_yfinance_formatted_timestamps_and_unknown_session(source):
    _, metadata, _ = source
    metadata["regularMarketTime"] = pd.Timestamp(metadata["regularMarketTime"], unit="s", tz="UTC")
    for period in metadata["currentTradingPeriod"].values():
        for bound in ("start", "end"):
            period[bound] = pd.Timestamp(period[bound], unit="s", tz="UTC")
    price, technical = market_capture.capture_market("NVDA")
    assert price.event_time == NOW.replace(second=3)
    assert price.payload["session"] == "regular"
    assert technical.payload["session_date"] == "2026-11-17"
    metadata["currentTradingPeriod"] = None
    price, technical = market_capture.capture_market("NVDA")
    assert price.payload["session"] == "unknown"
    assert technical.payload["session_date"] == "2026-11-17"


def test_capture_accepts_pinned_yfinance_metadata_proxy_without_fetching_lazy_keys(
    source, monkeypatch
):
    history, metadata, _ = source
    proxy = HistoryMetadata(SimpleNamespace(_history_metadata=metadata))

    class Ticker:
        def history(self, **kwargs):
            return history.copy()

        def get_history_metadata(self):
            return proxy

    monkeypatch.setattr(market_capture.yf, "Ticker", lambda symbol: Ticker())
    price, _ = market_capture.capture_market("NVDA")
    assert price.payload["price_usd"] == "150.81"


def test_capture_keeps_same_day_only_after_its_actual_regular_session_end(source, monkeypatch):
    _, _, _ = source

    class AfterClose(FixedClock):
        @classmethod
        def now(cls, tz=None):
            return NOW.replace(hour=22).astimezone(tz)

    monkeypatch.setattr(market_capture, "datetime", AfterClose)
    price, technical = market_capture.capture_market("NVDA")
    assert price.payload["session"] == "post"
    assert technical.payload["row_count"] == 230
    assert technical.payload["session_date"] == "2026-11-18"


def test_capture_refuses_invalid_completed_ohlcv(source):
    history, _, _ = source
    history.loc[history.index[-2], "High"] = 0
    with pytest.raises(ValueError, match="OHLCV"):
        market_capture.capture_market("NVDA")


@pytest.mark.parametrize("change", ["future_price", "invalid_price", "currency", "symbol", "empty"])
def test_capture_refuses_unusable_price_or_history(source, change):
    history, metadata, _ = source
    if change == "future_price":
        metadata["regularMarketTime"] = int(NOW.timestamp()) + 1
    elif change == "invalid_price":
        metadata["regularMarketPrice"] = float("nan")
    elif change == "currency":
        metadata["currency"] = "EUR"
    elif change == "symbol":
        metadata["symbol"] = "TSLA"
    else:
        history.drop(history.index, inplace=True)
    with pytest.raises(ValueError):
        market_capture.capture_market("NVDA")
