import hashlib
import json
from datetime import UTC, datetime, timedelta

import pytest

from tradingbot.snapshots import ArchiveRecord


def record(kind, symbol, at):
    return ArchiveRecord(
        kind=kind,
        provider="fixture",
        record_id=f"{symbol}-{kind}",
        symbol=symbol,
        revision=1,
        event_time=at,
        available_at=at,
        ingested_at=at,
        payload={"source_url": "https://issuer.example/record"},
    )


def issuer_archive(path, records):
    raw = json.dumps({
        "schema_version": "0.1",
        "source_mode": "local_archive",
        "records": [item.model_dump(mode="json") for item in records],
    }).encode()
    path.write_bytes(raw)
    return path, hashlib.sha256(raw).hexdigest()


def test_prepare_merges_pinned_issuer_and_market_observations(tmp_path, monkeypatch):
    from tradingbot import prepare_research

    known = datetime.now(UTC) - timedelta(minutes=1)
    filings = issuer_archive(tmp_path / "filings.json", [
        record("filing", symbol, known) for symbol in ("NVDA", "TSLA")
    ])
    news = issuer_archive(tmp_path / "news.json", [
        record(kind, symbol, at)
        for kind, symbol, at in (
            ("news", "NVDA", known), ("news", "TSLA", known),
            ("news", "MSFT", known), ("filing", "NVDA", known + timedelta(days=1)),
        )
    ])
    captured = []

    def capture(symbol):
        captured.append(symbol)
        return (record("bar", symbol, datetime.now(UTC)),)

    monkeypatch.setattr(prepare_research, "capture_market", capture)
    book, archive = prepare_research.prepare(["NVDA", "TSLA"], [filings, news], "1000")
    assert captured == ["NVDA", "TSLA"]
    assert book.cash_usd == 1000 and not book.positions
    assert archive["source_mode"] == "local_archive"
    assert len(archive["records"]) == 6
    assert {item["symbol"] for item in archive["records"]} == {"NVDA", "TSLA"}
    for item in archive["records"]:
        assert datetime.fromisoformat(item["ingested_at"]) <= book.as_of


def test_prepare_rejects_wrong_pin_or_missing_issuer_before_network(tmp_path, monkeypatch):
    from tradingbot import prepare_research

    known = datetime.now(UTC) - timedelta(minutes=1)
    path, digest = issuer_archive(tmp_path / "source.json", [record("filing", "NVDA", known)])
    monkeypatch.setattr(
        prepare_research, "capture_market", lambda symbol: pytest.fail("unexpected market request")
    )
    with pytest.raises(ValueError, match="SHA-256 mismatch"):
        prepare_research.prepare(["NVDA"], [(path, "0" * 64)], "1000")
    with pytest.raises(ValueError, match="NVDA.*news"):
        prepare_research.prepare(["NVDA"], [(path, digest)], "1000")
