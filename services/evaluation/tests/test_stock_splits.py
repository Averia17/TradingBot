import json
from datetime import UTC, datetime

import pytest

from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.replay import load_replay, run_replay


def at(minute):
    return f"2026-10-02T13:{minute:02d}:00Z"


def quote(minute, bid, ask, available=None):
    return {
        "symbol": "TSLA",
        "observed_at": at(minute),
        "available_at": at(available or minute),
        "bid_usd": bid,
        "ask_usd": ask,
        "bid_size": 100,
        "ask_size": 100,
    }


def order(intent, side, qty, ready, expires=40):
    return {
        "intent_id": intent,
        "symbol": "TSLA",
        "side": side,
        "quantity": qty,
        "ready_at": at(ready),
        "expires_at": at(expires),
    }


def fixture():
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "starting_cash_usd": "1000.00",
        "commission_bps": 100,
        "quotes": [quote(30, "99.00", "100.00"), quote(35, "50.00", "51.00")],
        "orders": [order("buy", "BUY", 3, 29), order("sell", "SELL", 6, 34)],
        "stock_splits": [
            {
                "action_id": "split-2-for-1",
                "symbol": "TSLA",
                "available_at": at(31),
                "effective_at": at(32),
                "new_shares": 2,
                "old_shares": 1,
            }
        ],
    }


def replay(document, minute=None):
    decoded = load_replay(json.dumps(document).encode())
    return run_replay(
        decoded, through=datetime(2026, 10, 2, 13, minute, tzinfo=UTC) if minute else None
    )


def test_stock_split_changes_raw_shares_without_trades_or_cash_and_preserves_prices():
    result = replay(fixture())
    assert result["ending_cash_usd"] == "994.00"
    assert result["ending_bid_mark_nav_usd"] == "994.00"
    assert result["open_positions"] == {}
    assert result["simulated_commission_usd"] == "6.00"
    assert [(fill["quantity"], fill["price_usd"]) for fill in result["fills"]] == [
        ("3", "100.00"),
        ("6", "50.00"),
    ]
    action = result["stock_split_events"][0]
    assert action["action_id"] == "split-2-for-1"
    assert (action["shares_before"], action["shares_after"]) == ("3", "6")


def test_future_split_and_late_presplit_quote_do_not_change_quote_or_order_units():
    data = fixture()
    data["quotes"] = [
        quote(30, "99.00", "100.00"),
        quote(31, "999.00", "1000.00", 34),
        quote(35, "50.00", "51.00"),
    ]
    data["orders"] = [order("buy", "BUY", 3, 29), order("old", "SELL", 1, 31)]
    result = replay(data)
    assert result["open_positions"] == {"TSLA": "6"}
    assert result["ending_cash_usd"] == "697.00"
    assert result["ending_bid_mark_nav_usd"] == "997.00"
    assert result["intents"]["old"] == "cancelled_stock_split"
    assert len(result["fills"]) == 1
    data["quotes"] = data["quotes"][:1]
    before = replay(data, 31)
    assert before["open_positions"] == {"TSLA": "3"}
    assert before.get("stock_split_events", []) == []
    after = replay(data, 32)
    assert after["open_positions"] == {"TSLA": "6"}
    assert before["ending_bid_mark_nav_usd"] == after["ending_bid_mark_nav_usd"] == "994.00"
    assert after["split_marks_in_prior_units"] == ["TSLA"]


def test_same_time_dividend_uses_postsplit_raw_entitlement():
    data = fixture()
    data["cash_dividends"] = [
        {
            "action_id": "dividend",
            "symbol": "TSLA",
            "available_at": at(31),
            "entitlement_at": at(32),
            "paid_at": at(34),
            "cash_per_share_usd": "1.25",
        }
    ]
    result = replay(data)
    assert result["cash_dividend_events"][0]["entitled_shares"] == "6"
    assert result["simulated_dividend_cash_usd"] == "7.50"
    assert result["ending_cash_usd"] == "1001.50"


def test_split_and_reverse_split_at_distinct_times_exclude_late_quote_from_original_era():
    data = fixture()
    data["stock_splits"].append(
        dict(
            action_id="reverse",
            symbol="TSLA",
            available_at=at(32),
            effective_at=at(33),
            new_shares=1,
            old_shares=2,
        )
    )
    data["quotes"] = [
        quote(30, "99.00", "100.00"),
        quote(31, "0.10", "0.11", 35),
        quote(36, "100.00", "101.00"),
    ]
    data["orders"] = [order("buy", "BUY", 3, 29), order("sell", "SELL", 3, 33)]
    result = replay(data)
    assert [event["shares_after"] for event in result["stock_split_events"]] == ["6", "3"]
    assert [(fill["quantity"], fill["price_usd"]) for fill in result["fills"]] == [
        ("3", "100.00"),
        ("3", "100.00"),
    ]
    assert result["ending_cash_usd"] == "994.00"


def test_same_time_quote_cannot_fill_stale_intent_and_partial_fill_uses_raw_units():
    data = fixture()
    data["quotes"] = [
        quote(32, "50.00", "51.00"),
        quote(35, "50.00", "51.00") | {"bid_size": 2, "ask_size": 2},
    ]
    data["orders"] = [order("old", "BUY", 1, 31), order("postsplit", "BUY", 4, 32)]
    result = replay(data)
    assert result["intents"]["old"] == "cancelled_stock_split"
    assert result["open_positions"] == {"TSLA": "2"}
    assert result["ending_cash_usd"] == "896.98"
    (fill,) = result["fills"]
    assert fill["intent_id"] == "postsplit"
    assert (fill["quantity"], fill["price_usd"], fill["commission_usd"]) == ("2", "51.00", "1.02")


def test_benchmarks_use_same_corporate_actions_and_raw_fill_evidence():
    data = fixture()
    data["quotes"].insert(0, quote(29, "99.00", "100.00"))
    report = run_benchmarks(
        load_replay(json.dumps(data).encode()),
        start_at=datetime.fromisoformat(at(29)),
        buy_and_hold_symbol="TSLA",
    )
    assert report["assumptions"]["corporate_actions_included"] is True
    assert report["benchmarks"]["cash"]["ending_bid_mark_nav_usd"] == "1000.00"
    for name in ("buy_and_hold", "equal_weight_once"):
        result = report["benchmarks"][name]
        assert result["open_positions"] == {"TSLA": "18"}
        assert result["ending_cash_usd"] == "91.00"
        assert result["ending_bid_mark_nav_usd"] == "991.00"
        assert [(fill["quantity"], fill["price_usd"]) for fill in result["fills"]] == [
            ("9", "100.00")
        ]
        assert result["stock_split_events"][0]["shares_after"] == "18"


def test_benchmark_formation_rejects_pre_split_quote_and_uses_exact_raw_share_grid():
    data = fixture()
    data["quotes"].insert(1, quote(31, "99.00", "100.00", 33))

    def compute():
        return run_benchmarks(
            load_replay(json.dumps(data).encode()),
            start_at=datetime.fromisoformat(at(34)),
            buy_and_hold_symbol="TSLA",
        )

    with pytest.raises(ValueError, match="known at benchmark start"):
        compute()
    data["quotes"].insert(2, quote(34, "50.00", "51.00"))
    result = compute()
    assert result["assumptions"]["raw_share_step_at_formation"] == {"TSLA": 2}
    hold = result["benchmarks"]["buy_and_hold"]
    assert hold["open_positions"] == {"TSLA": "18"}
    assert hold["fills"][0]["price_usd"] == "51.00"


@pytest.mark.parametrize(
    "invalid", ["late", "conflict", "not_exact", "fractional_inventory", "order_grid", "price_grid"]
)
def test_invalid_stock_split_fails_closed(invalid):
    data = fixture()
    action = data["stock_splits"][0]
    if invalid == "late":
        action["available_at"] = at(33)
    elif invalid == "conflict":
        data["stock_splits"].append(action | {"action_id": "duplicate"})
    elif invalid == "not_exact":
        action["new_shares"] = 3
    elif invalid == "fractional_inventory":
        action.update(new_shares=1, old_shares=2)
    elif invalid == "order_grid":
        data["orders"][1]["quantity"] = 1
    elif invalid == "price_grid":
        action.update(new_shares=1, old_shares=2)
        data["orders"][0]["quantity"] = 2
        data["quotes"][1]["ask_usd"] = "51.01"
    with pytest.raises(ValueError, match="(?i)stock split|whole shares"):
        replay(data)
