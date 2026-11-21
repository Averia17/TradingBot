"""Capture Yahoo prices and completed-session indicators through pinned libraries."""

from collections.abc import Mapping
from datetime import UTC, datetime
from decimal import ROUND_HALF_UP, Decimal, InvalidOperation
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import pandas as pd
import yfinance as yf
from pydantic import TypeAdapter
from stockstats import StockDataFrame

from tradingbot.contracts import Symbol
from tradingbot.snapshots import ArchiveRecord

_SYMBOL = TypeAdapter(Symbol)
_OHLCV = ("Open", "High", "Low", "Close", "Volume")
_INDICATORS = {
    "sma20": ("close_20_sma", 20),
    "sma50": ("close_50_sma", 50),
    "sma200": ("close_200_sma", 200),
    "rsi14": ("rsi_14", 15),
    "macd": ("macd", 26),
    "macd_signal": ("macds", 35),
    "atr14": ("atr_14", 15),
}


def _timestamp(value) -> datetime:
    if value is None or isinstance(value, bool):
        raise ValueError("market timestamp is unavailable")
    stamp = (
        pd.Timestamp(value, unit="s", tz="UTC")
        if isinstance(value, int | float)
        else pd.Timestamp(value)
    )
    if pd.isna(stamp) or stamp.tzinfo is None:
        raise ValueError("market timestamp requires a timezone")
    return stamp.to_pydatetime().astimezone(UTC)


def _decimal(value, places: int = 2) -> str:
    try:
        amount = Decimal(str(value))
        if not amount.is_finite():
            raise ValueError("market value is not finite")
        return format(amount.quantize(Decimal(1).scaleb(-places), rounding=ROUND_HALF_UP), "f")
    except InvalidOperation as exc:
        raise ValueError("market value is unavailable") from exc


def capture_market(symbol: str) -> tuple[ArchiveRecord, ArchiveRecord]:
    """Return the last regular trade and indicators known at one capture cutoff."""
    symbol = _SYMBOL.validate_python(symbol)
    ticker = yf.Ticker(symbol)
    history = ticker.history(
        period="2y", interval="1d", auto_adjust=False, actions=False, prepost=False, timeout=15
    )
    metadata = ticker.get_history_metadata()
    cutoff = datetime.now(UTC)
    if (
        not isinstance(metadata, Mapping)
        or metadata.get("symbol") != symbol
        or metadata.get("currency") != "USD"
    ):
        raise ValueError("Yahoo metadata does not identify the requested USD security")
    try:
        timezone = ZoneInfo(metadata["exchangeTimezoneName"])
    except (KeyError, TypeError, ZoneInfoNotFoundError) as exc:
        raise ValueError("Yahoo exchange timezone is unavailable") from exc
    observed = _timestamp(metadata.get("regularMarketTime"))
    price = _decimal(metadata.get("regularMarketPrice"))
    if observed > cutoff or Decimal(price) <= 0:
        raise ValueError("Yahoo regular trade is future-dated or nonpositive")

    periods = {}
    for name, period in (metadata.get("currentTradingPeriod") or {}).items():
        if name in {"pre", "regular", "post"}:
            start, end = _timestamp(period["start"]), _timestamp(period["end"])
            if start >= end:
                raise ValueError("Yahoo session bounds are not ordered")
            periods[name] = (start, end)
    session = "closed" if periods else "unknown"
    for name, (start, end) in periods.items():
        if start <= cutoff < end:
            session = name

    if (
        not isinstance(history, pd.DataFrame)
        or history.empty
        or not set(_OHLCV).issubset(history.columns)
    ):
        raise ValueError("Yahoo daily OHLCV is unavailable")
    if (
        not isinstance(history.index, pd.DatetimeIndex)
        or history.index.has_duplicates
        or history.index.hasnans
    ):
        raise ValueError("Yahoo daily OHLCV has ambiguous dates")
    daily = history.loc[:, list(_OHLCV)].copy().sort_index()
    daily.index = (
        daily.index.tz_localize(timezone)
        if daily.index.tz is None
        else daily.index.tz_convert(timezone)
    )
    today = cutoff.astimezone(timezone).date()
    regular = periods.get("regular")
    today_closed = (
        regular is not None
        and regular[0].astimezone(timezone).date() == today
        and cutoff >= regular[1]
    )
    daily = daily[(daily.index.date < today) | ((daily.index.date == today) & today_closed)]
    if daily.empty:
        raise ValueError("Yahoo history contains no completed daily session")
    for row in daily.itertuples(index=False, name=None):
        values = [Decimal(str(value)) for value in row]
        if (
            any(not value.is_finite() for value in values)
            or any(value <= 0 for value in values[:4])
            or values[4] < 0
            or values[4] != values[4].to_integral_value()
        ):
            raise ValueError("Yahoo completed daily OHLCV contains invalid values")
        opening, high, low, close, _ = values
        if high < max(opening, close, low) or low > min(opening, close, high):
            raise ValueError("Yahoo completed daily price range is inconsistent")

    statistics = StockDataFrame.retype(daily.copy())
    indicators = {}
    for name, (column, warmup) in _INDICATORS.items():
        if len(daily) >= warmup:
            try:
                indicators[name] = _decimal(statistics[column].iloc[-1], 4)
            except (KeyError, ValueError, ZeroDivisionError):
                continue
    latest = daily.iloc[-1]
    shared = {
        "currency": "USD",
        "source_url": f"https://finance.yahoo.com/quote/{symbol}/history/",
        "captured_at": cutoff.isoformat(),
    }
    trade = ArchiveRecord(
        kind="bar",
        provider="yahoo-finance",
        record_id="market-price",
        symbol=symbol,
        revision=1,
        event_time=observed,
        available_at=cutoff,
        ingested_at=cutoff,
        payload=shared
        | {"price_usd": price, "quote_type": "regular_market_last_trade", "session": session},
    )
    technical = ArchiveRecord(
        kind="bar",
        provider="yahoo-finance",
        record_id="completed-daily-technical",
        symbol=symbol,
        revision=1,
        event_time=daily.index[-1].to_pydatetime().astimezone(UTC),
        available_at=cutoff,
        ingested_at=cutoff,
        payload=shared
        | {
            "session_date": daily.index[-1].date().isoformat(),
            "row_count": len(daily),
            "event_time_basis": "daily_bar_label",
            "price_basis": "raw",
            "open": _decimal(latest["Open"]),
            "high": _decimal(latest["High"]),
            "low": _decimal(latest["Low"]),
            "close": _decimal(latest["Close"]),
            "volume": int(latest["Volume"]),
            "indicators": indicators,
        },
    )
    return trade, technical
