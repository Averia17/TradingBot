import hashlib
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from uuid import UUID

from tradingbot.selection import SelectionRequest, plan_research

NOW = datetime(2026, 11, 18, 10, tzinfo=UTC)


def entry(kind, symbol, record_id, at, payload, seen=None):
    return {
        "kind": kind,
        "symbol": symbol,
        "provider": "selection-fixture",
        "record_id": record_id,
        "revision": 1,
        "event_time": at.isoformat(),
        "available_at": at.isoformat(),
        "ingested_at": (seen or at).isoformat(),
        "payload": payload,
    }


def bars(symbol, closes, *, delay=0, wide=False):
    mark_time = NOW - timedelta(seconds=30 + delay)
    return [
        entry(
            "bar",
            symbol,
            f"bar-{index}",
            mark_time - timedelta(hours=len(closes) - index - 1),
            {
                "close": str(close),
                "volume": 5000,
                "currency": "USD",
                "price_basis": "raw",
                "bid": "99",
                "ask": "105" if wide else "100",
            },
        )
        for index, close in enumerate(closes)
    ]


def request(tmp_path, records, *, remaining=2, calls=30):
    data = json.dumps(
        {"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": records}
    ).encode()
    (tmp_path / "market.json").write_bytes(data)
    return {
        "schema_version": "0.1",
        "job_id": str(UUID(int=1)),
        "policy_version": "selection-v1",
        "portfolio": {
            "snapshot_id": str(UUID(int=2)),
            "as_of": NOW.isoformat(),
            "cash_usd": "1000",
            "positions": [
                {
                    "symbol": "TSLA",
                    "instrument_id": "fixture:TSLA",
                    "quantity": "1",
                    "mark_price_usd": "100",
                }
            ],
        },
        "data_manifest": {"ref": "market.json", "sha256": hashlib.sha256(data).hexdigest()},
        "candidates": [
            {"symbol": "NVDA", "allowed": True},
            {"symbol": "TSLA", "allowed": True},
            {"symbol": "ABC", "allowed": False},
            {"symbol": "XYZ", "allowed": True},
            {"symbol": "DEF", "allowed": True},
        ],
        "top_k": 3,
        "remaining_analysis_slots": remaining,
        "calls_per_symbol": 10,
        "max_llm_calls": calls,
        "roc_periods": 2,
        "max_bar_age_seconds": 90,
        "max_bar_gap_seconds": 3600,
        "max_catalyst_age_seconds": 1200,
        "min_bar_turnover_usd": "10000",
        "max_spread_bps": 150,
    }


def test_prioritize_held_fresh_catalyst_filter_and_apply_remaining_budget(tmp_path):
    records = (
        bars("NVDA", [70, 80, 110])
        + bars("TSLA", [100, 100, 98])
        + bars("ABC", [10, 12, 80])
        + bars("XYZ", [100, 120, 130], delay=500)
        + bars("DEF", [90, 110, 125], wide=True)
        + [entry("filing", "TSLA", "earnings", NOW - timedelta(minutes=2), {})]
    )
    config = request(tmp_path, records, remaining=2, calls=11)
    result = plan_research(tmp_path, config)
    assert result["job"]["requested_symbols"] == ["TSLA"]
    assert result["job"]["max_llm_calls"] == 11
    assert result["job"]["data_manifest"] == config["data_manifest"]
    assert result["job"]["policy_version"] == config["policy_version"]
    statuses = {row["symbol"]: row["status"] for row in result["candidates"]}
    assert statuses == {
        "TSLA": "selected",
        "NVDA": "capacity_exhausted",
        "ABC": "not_allowed",
        "XYZ": "stale_bar",
        "DEF": "spread_above_limit",
    }


def test_late_news_and_future_bars_cannot_change_workload(tmp_path):
    records = bars("NVDA", [70, 80, 110]) + bars("TSLA", [100, 100, 98])
    records += [
        entry(
            "news", "TSLA", "late", NOW - timedelta(seconds=10), {}, seen=NOW + timedelta(seconds=1)
        ),
        entry(
            "bar",
            "TSLA",
            "future",
            NOW + timedelta(seconds=1),
            {
                "close": "999",
                "volume": 5000,
                "currency": "USD",
                "price_basis": "raw",
                "bid": "99",
                "ask": "100",
            },
        ),
    ]
    result = plan_research(tmp_path, request(tmp_path, records, remaining=1))
    assert result["job"]["requested_symbols"] == ["NVDA"]
    assert result["candidates"][0]["momentum_bps"] == "5714.285714"
    assert plan_research(tmp_path, request(tmp_path, records, remaining=0))["job"] is None


def test_sparse_or_split_crossing_history_is_excluded(tmp_path):
    records = bars("NVDA", [70, 80, 110]) + bars("TSLA", [100, 100, 98])
    records += [
        entry(
            "action",
            "NVDA",
            "split",
            NOW - timedelta(hours=1),
            {
                "action": "split",
                "effective_at": (NOW - timedelta(minutes=15)).isoformat(),
            },
        )
    ]
    config = request(tmp_path, records)
    result = plan_research(tmp_path, config)
    assert result["job"]["requested_symbols"] == ["TSLA"]
    assert {row["symbol"]: row["status"] for row in result["candidates"]}[
        "NVDA"
    ] == "corporate_action_in_window"
    config["max_bar_gap_seconds"] = 3599
    result = plan_research(tmp_path, config)
    assert result["job"] is None
    assert {row["symbol"]: row["status"] for row in result["candidates"]}[
        "TSLA"
    ] == "bar_gap_above_limit"


def test_published_selection_schema_and_example():
    research = Path(__file__).resolve().parents[1]
    schema = research.parent.parent / "contracts/research-selection.schema.json"
    assert json.loads(schema.read_text(encoding="utf-8")) == SelectionRequest.model_json_schema(
        mode="serialization"
    )
    example = research / "examples/selection"
    plan = plan_research(
        example, json.loads((example / "request.json").read_text(encoding="utf-8"))
    )
    assert plan["job"]["requested_symbols"] == ["TSLA"]
    assert plan["job"]["max_llm_calls"] == 17
