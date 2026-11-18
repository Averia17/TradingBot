import hashlib
import json
from datetime import datetime
from pathlib import Path

import pytest

from tradingbot.ingest import build_archive
from tradingbot.snapshots import load_snapshot


def pin(root, name, data):
    raw = json.dumps(data, sort_keys=True).encode()
    (root / name).write_bytes(raw)
    return {"ref": name, "sha256": hashlib.sha256(raw).hexdigest()}


def capture(root, name, at, records):
    return {
        "kind": "normalized_market",
        "source": pin(root, name, {"schema_version": "0.1", "records": records}),
        "captured_at": at,
        "provider": "fixture-exchange",
    }


def row(kind, record_id, event, publication, payload, *, revision=1, symbol="OLD"):
    return {
        "kind": kind,
        "record_id": record_id,
        "symbol": symbol,
        "revision": revision,
        "event_time": event,
        "available_at": publication,
        "payload": payload,
    }


def manifest(*captures):
    return {"schema_version": "0.1", "source_mode": "synthetic_fixture", "captures": captures}


def fixture_captures(root):
    news_at = "2026-11-17T15:00:00Z"
    listed = row(
        "listing",
        "list-old-17",
        "2026-11-17T14:00:00Z",
        "2026-11-17T14:01:00Z",
        {"security_id": "equity-17", "status": "listed", "effective_at": "2026-11-17T14:30:00Z"},
    )
    news = row("news", "n1", news_at, news_at, {"headline": "First report"})
    bar_at = "2026-11-17T16:00:00Z"
    bar = row(
        "bar",
        "b1",
        bar_at,
        bar_at,
        {
            "security_id": "equity-17",
            "price_basis": "raw",
            "currency": "USD",
            "open": "100.00000001",
            "high": "102.00",
            "low": "99.00",
            "close": "101.00000009",
            "volume": 1234,
            "bid": "101.00",
            "ask": "101.01",
        },
    )
    future = "2026-11-19T14:30:00Z"
    announced = "2026-11-17T16:30:00Z"
    delisted = row(
        "listing",
        "delist-old-17",
        announced,
        announced,
        {"security_id": "equity-17", "status": "delisted", "effective_at": future},
    )
    split = row(
        "action",
        "split-old-17",
        announced,
        announced,
        {
            "security_id": "equity-17",
            "action": "split",
            "effective_at": "2026-11-18T14:30:00Z",
            "new_shares": 2,
            "old_shares": 1,
        },
    )
    corrected = row(
        "news",
        "n1",
        news_at,
        "2026-11-17T17:00:00Z",
        {"headline": "Corrected report"},
        revision=2,
    )
    reused = row(
        "listing",
        "list-old-23",
        "2026-11-18T15:00:00Z",
        "2026-11-18T15:00:00Z",
        {"security_id": "equity-23", "status": "listed", "effective_at": "2026-11-20T14:30:00Z"},
    )
    return (
        capture(root, "market-initial.json", "2026-11-17T16:05:00Z", [listed, news, bar]),
        capture(root, "market-next.json", "2026-11-17T18:00:00Z", [delisted, split, corrected]),
        capture(root, "market-reused.json", "2026-11-18T15:05:00Z", [reused]),
    )


def test_market_import_preserves_raw_amount_and_bitemporal_news(tmp_path):
    initial, next_capture, reused = fixture_captures(tmp_path)
    archive = build_archive(tmp_path, manifest(next_capture, initial, reused))
    assert archive == build_archive(tmp_path, manifest(initial, reused, next_capture))
    ref = pin(tmp_path, "archive.json", archive)

    cutoff = datetime.fromisoformat("2026-11-17T17:30:00Z")
    source = load_snapshot(tmp_path, ref, cutoff)
    local = load_snapshot(tmp_path, ref, cutoff, visibility="locally_ingested")
    assert next(record for record in source.records if record.kind == "news").revision == 2
    news = next(record for record in local.records if record.kind == "news")
    assert news.revision == 1
    assert news.payload["headline"] == "First report"
    assert news.payload["source_sha256"] == initial["source"]["sha256"]
    assert news.ingested_at == datetime.fromisoformat(initial["captured_at"])

    after = load_snapshot(
        tmp_path,
        ref,
        datetime.fromisoformat("2026-11-20T15:00:00Z"),
        visibility="locally_ingested",
    )
    by_kind = {record.kind: record for record in after.records}
    assert by_kind["bar"].payload["close"] == "101.00000009"
    assert by_kind["bar"].payload["price_basis"] == "raw"
    assert by_kind["action"].payload["new_shares"] == 2
    assert by_kind["news"].payload["headline"] == "Corrected report"


def test_universe_distinguishes_missing_and_delisted_with_reused_ticker(tmp_path):
    from tradingbot.snapshots import load_universe

    records = build_archive(tmp_path, manifest(*fixture_captures(tmp_path)))
    ref = pin(tmp_path, "archive.json", records)
    ids = ("equity-99", "equity-23", "equity-17")

    def universe(at):
        return load_universe(
            tmp_path, ref, datetime.fromisoformat(at), "fixture-exchange", security_ids=ids
        )

    early = universe("2026-11-17T16:04:59Z")
    assert [(item.security_id, item.status) for item in early.entries] == [
        ("equity-17", "missing"),
        ("equity-23", "missing"),
        ("equity-99", "missing"),
    ]
    before = universe("2026-11-19T14:29:59Z")
    assert before.entries[0].status == "listed"
    assert before.entries[1].status == "missing"
    at_delisting = universe("2026-11-19T14:30:00Z")
    assert at_delisting.entries[0].status == "delisted"
    assert at_delisting.entries[0].evidence.symbol == "OLD"
    after = universe("2026-11-20T14:30:00Z")
    assert [
        (item.security_id, item.status, item.evidence.symbol) for item in after.entries[:2]
    ] == [
        ("equity-17", "delisted", "OLD"),
        ("equity-23", "listed", "OLD"),
    ]
    assert after.entries[2].evidence is None


def test_late_relisting_cannot_change_prior_universe_or_identity(tmp_path):
    from tradingbot.snapshots import load_universe

    relist_time = "2026-11-20T14:30:00Z"
    received = "2026-11-20T14:40:00Z"
    listing = row(
        "listing",
        "relist-old-17",
        relist_time,
        relist_time,
        {"security_id": "equity-17", "status": "listed", "effective_at": relist_time},
        symbol="NEW",
    )
    late = capture(tmp_path, "relisting.json", received, [listing])
    records = build_archive(tmp_path, manifest(*fixture_captures(tmp_path), late))
    ref = pin(tmp_path, "archive.json", records)

    def universe(at):
        projection = load_universe(
            tmp_path,
            ref,
            datetime.fromisoformat(at),
            "fixture-exchange",
            security_ids=["equity-17"],
        )
        return [(item.status, item.evidence.symbol) for item in projection.entries]

    assert universe("2026-11-20T14:39:59Z") == [("delisted", "OLD")]
    assert universe(received) == [("listed", "NEW")]


def test_market_ingest_rejects_future_publish_adjusted_bars_and_duplicate_revisions(tmp_path):
    initial, next_capture, _ = fixture_captures(tmp_path)
    with pytest.raises(ValueError, match="captured before publication"):
        build_archive(tmp_path, manifest(next_capture | {"captured_at": "2026-11-17T16:59:00Z"}))
    with pytest.raises(ValueError, match="duplicate market record revision"):
        build_archive(tmp_path, manifest(initial, initial))
    raw_path = tmp_path / initial["source"]["ref"]
    data = json.loads(raw_path.read_bytes())
    data["records"][-1]["payload"]["price_basis"] = "adjusted"
    capture = initial | {"source": pin(tmp_path, "adjusted.json", data)}
    with pytest.raises(ValueError, match="price_basis"):
        build_archive(tmp_path, manifest(capture))


@pytest.mark.parametrize(
    ("change", "message"),
    [
        ({"event_time": "2026-11-17T16:00:00.000000001Z"}, "fractional digits"),
        ({"payload": {"close": "999.00"}}, "OHLC"),
        ({"payload": {"ask": "100.00"}}, "bid/ask"),
        ({"payload": {"close": 101}}, "valid string"),
    ],
)
def test_market_does_not_truncate_time_or_coerce_inconsistent_quotes(tmp_path, change, message):
    initial, _, _ = fixture_captures(tmp_path)
    data = json.loads((tmp_path / initial["source"]["ref"]).read_bytes())
    observation = data["records"][-1]
    if "payload" in change:
        observation["payload"].update(change["payload"])
    else:
        observation.update(change)
    malformed = initial | {"source": pin(tmp_path, "malformed.json", data)}
    with pytest.raises(ValueError, match=message):
        build_archive(tmp_path, manifest(malformed))


def test_published_market_fixture_and_input_schema_match(tmp_path):
    from tradingbot.ingest import MarketResponse

    research = Path(__file__).resolve().parents[1]
    published = research.parent.parent / "contracts/pit-market-capture.schema.json"
    assert json.loads(published.read_text(encoding="utf-8")) == MarketResponse.model_json_schema()
    root = research / "examples/pit-import"
    archive = build_archive(
        root, json.loads((root / "market-manifest.json").read_text(encoding="utf-8"))
    )
    ref = pin(tmp_path, "archive.json", archive)
    snapshot = load_snapshot(tmp_path, ref, datetime.fromisoformat("2026-11-20T15:00:00Z"))
    assert {item.kind for item in snapshot.records} == {"action", "bar", "listing", "news"}
