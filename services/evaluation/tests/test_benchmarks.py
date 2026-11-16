import json
from datetime import UTC, datetime

import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.replay import load_replay


def timestamp(second):
    return datetime(2026, 10, 2, 13, 30, second, tzinfo=UTC)


def quote(symbol, second, bid, ask, *, available=None, size=1000):
    return {
        "symbol": symbol,
        "observed_at": timestamp(second).isoformat(),
        "available_at": timestamp(second if available is None else available).isoformat(),
        "bid_usd": bid,
        "ask_usd": ask,
        "bid_size": size,
        "ask_size": size,
    }


def fixture(*, quotes=None, cash="1000.01", commission=100):
    return json.dumps(
        {
            "schema_version": "0.1",
            "source_mode": "synthetic_fixture",
            "starting_cash_usd": cash,
            "commission_bps": commission,
            "quotes": quotes
            if quotes is not None
            else [
                quote("TSLA", 0, "99.00", "100.00"),
                quote("NVDA", 0, "49.00", "50.00"),
                quote("TSLA", 2, "99.00", "100.00"),
                quote("NVDA", 2, "49.00", "50.00"),
                quote("TSLA", 4, "120.00", "121.00"),
                quote("NVDA", 4, "55.00", "56.00"),
            ],
            "orders": [],
        }
    ).encode()


def test_same_funding_commission_and_native_bid_marks():
    report = run_benchmarks(
        load_replay(fixture()), start_at=timestamp(0), buy_and_hold_symbol="TSLA"
    )
    assert report["source_mode"] == "synthetic_fixture"
    assert report["alpha_measured"] is False
    assert report["external_costs_usd"] is None
    assert report["starting_cash_usd"] == "1000.01"
    assert report["commission_bps"] == 100

    cash = report["benchmarks"]["cash"]
    assert cash["ending_cash_usd"] == cash["ending_bid_mark_nav_usd"] == "1000.01"
    assert cash["simulated_commission_usd"] == "0.00"
    assert cash["fills"] == []

    hold = report["benchmarks"]["buy_and_hold"]
    assert hold["ending_cash_usd"] == "91.01"
    assert hold["open_positions"] == {"TSLA": "9"}
    assert hold["simulated_commission_usd"] == "9.00"
    assert hold["ending_bid_mark_nav_usd"] == "1171.01"
    assert hold["pnl_after_simulated_commission_usd"] == "171.00"

    equal = report["benchmarks"]["equal_weight_once"]
    assert equal["allocations_usd"] == {"NVDA": "500.01", "TSLA": "500.00"}
    assert equal["ending_cash_usd"] == "141.51"
    assert equal["open_positions"] == {"NVDA": "9", "TSLA": "4"}
    assert equal["simulated_commission_usd"] == "8.50"
    assert equal["ending_bid_mark_nav_usd"] == "1116.51"


def test_formation_excludes_unavailable_records_and_next_quote_must_be_newer():
    replay = load_replay(
        fixture(
            cash="100.00",
            commission=0,
            quotes=[
                quote("TSLA", 1, "99.00", "100.00"),
                quote("TSLA", 0, "9.00", "10.00", available=2),
                quote("TSLA", 2, "9.00", "10.00", available=3),
                quote("TSLA", 4, "99.00", "100.00"),
                quote("AMZN", 3, "49.00", "50.00"),
            ],
        )
    )
    report = run_benchmarks(replay, start_at=timestamp(2), buy_and_hold_symbol="TSLA")
    equal = report["benchmarks"]["equal_weight_once"]
    assert equal["allocations_usd"] == {"TSLA": "100.00"}
    assert len(equal["fills"]) == 1
    assert equal["fills"][0]["price_usd"] == "100.00"
    assert equal["fills"][0]["quantity"] == "1"
    with pytest.raises(ValueError, match="known at benchmark start"):
        run_benchmarks(replay, start_at=timestamp(2), buy_and_hold_symbol="AMZN")


def test_price_gap_cannot_spend_another_equal_weight_allocation():
    replay = load_replay(
        fixture(
            cash="100.00",
            commission=100,
            quotes=[
                quote("TSLA", 0, "48.00", "49.00"),
                quote("NVDA", 0, "48.00", "49.00"),
                quote("TSLA", 2, "49.00", "50.00"),
                quote("NVDA", 3, "48.00", "49.00"),
            ],
        )
    )
    equal = run_benchmarks(replay, start_at=timestamp(0), buy_and_hold_symbol="TSLA")["benchmarks"][
        "equal_weight_once"
    ]
    assert equal["open_positions"] == {"NVDA": "1"}
    assert equal["ending_cash_usd"] == "50.51"
    assert equal["ending_bid_mark_nav_usd"] == "98.51"


def test_tiny_allocations_remain_cash_without_fabricating_fills():
    replay = load_replay(fixture(cash="0.01"))
    equal = run_benchmarks(replay, start_at=timestamp(0), buy_and_hold_symbol="TSLA")["benchmarks"][
        "equal_weight_once"
    ]
    assert equal["allocations_usd"] == {"NVDA": "0.01", "TSLA": "0.00"}
    assert equal["ending_bid_mark_nav_usd"] == "0.01"
    assert not equal["fills"]


def test_dividends_share_horizon_and_follow_each_baseline_holdings():
    def dividend(symbol, amount, name, paid, *, available=1, entitled=3):
        return {
            "action_id": name,
            "symbol": symbol,
            "cash_per_share_usd": amount,
            "available_at": timestamp(available).isoformat(),
            "entitlement_at": timestamp(entitled).isoformat(),
            "paid_at": timestamp(paid).isoformat(),
        }

    data = json.loads(fixture())
    # The common end is after both payments while TSLA's own last quote precedes them.
    data["quotes"].append(quote("NVDA", 6, "55.00", "56.00"))
    data["cash_dividends"] = [
        dividend("TSLA", "1.23", "tsla-paid", 5),
        dividend("NVDA", "0.50", "nvda-paid", 5),
        dividend("TSLA", "9.00", "tsla-pending", 8),
        dividend("TSLA", "9.00", "tsla-unavailable", 10, available=8, entitled=9),
    ]
    report = run_benchmarks(
        load_replay(json.dumps(data).encode()), start_at=timestamp(0), buy_and_hold_symbol="TSLA"
    )
    assert report["assumptions"]["corporate_actions_included"] is True
    assert report["benchmarks"]["cash"]["ending_bid_mark_nav_usd"] == "1000.01"
    hold = report["benchmarks"]["buy_and_hold"]
    assert hold["simulated_dividend_cash_usd"] == "11.07"
    assert hold["ending_bid_mark_nav_usd"] == "1182.08"
    equal = report["benchmarks"]["equal_weight_once"]
    assert equal["simulated_dividend_cash_usd"] == "9.42"
    assert equal["ending_bid_mark_nav_usd"] == "1125.93"
    assert "tsla-unavailable" not in json.dumps(report)


def test_cli_uses_local_replay(tmp_path, capsys):
    path = tmp_path / "replay.json"
    path.write_bytes(fixture())
    assert (
        main(
            [
                "benchmarks",
                str(path),
                "--start-at",
                timestamp(0).isoformat(),
                "--buy-and-hold-symbol",
                "TSLA",
            ]
        )
        == 0
    )
    result = json.loads(capsys.readouterr().out)
    assert result["benchmarks"]["cash"]["ending_cash_usd"] == "1000.01"
