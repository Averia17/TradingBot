import json

from test_benchmarks import fixture, quote, timestamp

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.replay import _nanoseconds, load_replay
from tradingbot_evaluation.trend import TrendConfig


def trend_report(quotes, *, max_gap=5, splits=None):
    document = json.loads(fixture(quotes=quotes, cash="1000.00"))
    if splits is not None:
        document["stock_splits"] = splits
    return run_benchmarks(
        load_replay(json.dumps(document).encode()),
        start_at=timestamp(0),
        buy_and_hold_symbol=None,
        include_equal_weight=False,
        trend=TrendConfig("TSLA", 1, max_gap),
    )["benchmarks"]["trend_momentum"]


def test_trend_fills_only_on_newer_quotes_and_accounts_for_both_sides():
    trend = trend_report(
        [
            quote("TSLA", 0, "99.00", "100.00"),
            quote("TSLA", 1, "109.00", "110.00"),
            quote("TSLA", 0, "89.00", "90.00", available=2),
            quote("TSLA", 3, "109.00", "110.00"),
            quote("TSLA", 4, "80.00", "81.00"),
            quote("TSLA", 5, "78.00", "79.00"),
        ]
    )
    assert trend["parameters"] == {"symbol": "TSLA", "lookback_quotes": 1, "max_gap_seconds": 5}
    assert trend["stale_or_reordered_quotes"] == 1
    assert [
        (fill["quantity"], fill["price_usd"], fill["commission_usd"]) for fill in trend["fills"]
    ] == [
        ("9", "110.00", "9.90"),
        ("9", "78.00", "7.02"),
    ]
    assert [fill["filled_at_ns"] for fill in trend["fills"]] == [
        str(_nanoseconds(timestamp(3))),
        str(_nanoseconds(timestamp(5))),
    ]
    assert trend["ending_bid_mark_nav_usd"] == trend["ending_cash_usd"] == "695.08"
    assert trend["simulated_commission_usd"] == "16.92"
    assert trend["open_positions"] == {}


def test_trend_resets_warmup_on_gaps_and_labels_stale_mark():
    trend = trend_report(
        [
            quote("TSLA", 0, "99.00", "100.00"),
            quote("TSLA", 20, "109.00", "110.00"),
            quote("TSLA", 21, "199.00", "200.00", available=40),
        ]
    )
    assert not trend["fills"]
    assert trend["ending_bid_mark_nav_usd"] == "1000.00"
    assert trend["observation_gap_resets"] == 1
    assert trend["warmup_quotes"] == 2
    assert trend["mark_is_stale"] is True


def test_aged_quote_never_executes_and_order_lifetime_still_expires():
    trend = trend_report(
        [
            quote("TSLA", 0, "99.00", "100.00"),
            quote("TSLA", 1, "109.00", "110.00"),
            quote("TSLA", 2, "109.00", "110.00", available=12),
        ]
    )
    assert trend["fills"] == []
    assert len(trend["signals"]) == 1
    assert trend["intents"] == {trend["signals"][0]["intent_id"]: "expired"}
    assert trend["ending_cash_usd"] == "1000.00"


def test_split_adjusted_native_midpoint_does_not_create_a_signal():
    trend = trend_report(
        [
            quote("TSLA", 0, "99.00", "100.00"),
            quote("TSLA", 1, "49.50", "50.00"),
            quote("TSLA", 2, "49.50", "50.00"),
        ],
        splits=[
            {
                "action_id": "two-for-one",
                "symbol": "TSLA",
                "available_at": timestamp(0).isoformat(),
                "effective_at": timestamp(1).isoformat(),
                "new_shares": 2,
                "old_shares": 1,
            }
        ],
    )
    assert trend["signals"] == []
    assert trend["fills"] == []
    assert len(trend["stock_split_events"]) == 1


def test_cli_freezes_trend_inputs(tmp_path, capsys):
    file = tmp_path / "fixture.json"
    file.write_bytes(fixture())
    arguments = [
        "benchmarks",
        str(file),
        "--start-at",
        timestamp(0).isoformat(),
        "--buy-and-hold-symbol",
        "TSLA",
        "--trend-symbol",
        "TSLA",
    ]
    assert main(arguments) == 2
    assert "Trend benchmark requires" in capsys.readouterr().err
    assert main([*arguments, "--trend-lookback-quotes", "2", "--trend-max-gap-seconds", "5"]) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["benchmarks"]["trend_momentum"]["parameters"]["lookback_quotes"] == 2
