import json

import pytest
from test_benchmarks import fixture, quote, timestamp

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.equal_weight import EqualWeightSchedule
from tradingbot_evaluation.replay import load_replay


def scheduled_fixture(*, sell_size=1000, sale_delayed=False, buy_delayed=False, buy_ask="5.00"):
    quotes = [
        quote("TSLA", 0, "10.00", "10.00"),
        quote("NVDA", 0, "10.00", "10.00"),
        quote("TSLA", 2, "10.00", "10.00"),
        quote("NVDA", 2, "10.00", "10.00"),
        quote("TSLA", 5, "20.00", "20.00"),
        quote("NVDA", 5, "5.00", "5.00"),
        quote("TSLA", 5 if sale_delayed else 6, "20.00", "20.00", available=6, size=sell_size),
        quote("NVDA", 6, "5.00", "5.00"),
        quote("TSLA", 8, "20.00", "20.00"),
        quote("NVDA", 7 if buy_delayed else 8, "5.00", buy_ask, available=8),
        quote("AMZN", 8, "10.00", "10.00"),
        quote("TSLA", 11, "20.00", "20.00"),
        quote("NVDA", 11, "5.00", "5.00"),
    ]
    return fixture(quotes=quotes, cash="1000.00", commission=100)


def scheduled(**fixture_args):
    return run_benchmarks(
        load_replay(scheduled_fixture(**fixture_args)),
        start_at=timestamp(0),
        buy_and_hold_symbol="TSLA",
        equal_weight_schedule=EqualWeightSchedule((timestamp(5),), 2, 10),
    )


def test_scheduled_equal_weight_funds_buys_with_confirmed_sell_cash_only():
    report = scheduled()
    result = report["benchmarks"]["equal_weight_scheduled"]
    assert report["alpha_measured"] is False and report["source_mode"] == "synthetic_fixture"
    assert report["benchmarks"]["equal_weight_once"]["open_positions"] == {
        "NVDA": "49",
        "TSLA": "49",
    }
    assert result["initial_universe"] == ["NVDA", "TSLA"]
    sales = [fill for fill in result["fills"] if ":sell:" in fill["intent_id"]]
    assert len(sales) == 1 and sales[0]["symbol"] == "TSLA"
    extra_buys = [fill for fill in result["fills"] if fill["intent_id"].startswith("ew:1:buy:")]
    assert len(extra_buys) == 1 and extra_buys[0]["symbol"] == "NVDA"
    assert int(extra_buys[0]["filled_at_ns"]) > int(sales[0]["filled_at_ns"])
    assert result["ending_cash_usd"] == "17.75"
    assert result["simulated_commission_usd"] == "17.25"
    assert result["open_positions"] == {"NVDA": "122", "TSLA": "30"}

    partial = scheduled(sell_size=1)["benchmarks"]["equal_weight_scheduled"]
    partial_sale = next(fill for fill in partial["fills"] if ":sell:" in fill["intent_id"])
    extra_buy = next(fill for fill in partial["fills"] if fill["intent_id"].startswith("ew:1:buy:"))
    assert partial_sale["quantity"] == "1"
    assert extra_buy["quantity"] == "5"
    assert partial["ending_cash_usd"] == "4.75"


def test_scheduled_equal_weight_excludes_old_quotes_and_unfilled_sale_proceeds():
    result = scheduled(sale_delayed=True, buy_delayed=True)["benchmarks"]["equal_weight_scheduled"]
    assert len(result["fills"]) == 2
    assert result["intents"]["ew:1:sell:TSLA"] == "expired"
    assert result["intents"]["ew:1:buy:NVDA"] == "expired"
    assert result["rebalances"][1]["free_cash_before_buy_usd"] == "10.20"
    assert result["ending_cash_usd"] == "10.20"


def test_buy_price_gap_cannot_spend_another_sleeve_allocation():
    result = scheduled(buy_ask="380.00")["benchmarks"]["equal_weight_scheduled"]
    assert result["intents"]["ew:1:buy:NVDA"] == "unaffordable_at_execution"
    assert result["ending_cash_usd"] == "386.40"


def test_benchmark_cli_reproduces_the_opt_in_schedule(tmp_path, capsys):
    tape = tmp_path / "tape.json"
    tape.write_bytes(scheduled_fixture())
    assert (
        main(
            [
                "benchmarks",
                str(tape),
                "--start-at",
                timestamp(0).isoformat(),
                "--buy-and-hold-symbol",
                "TSLA",
                "--rebalance-at",
                timestamp(5).isoformat(),
                "--rebalance-phase-seconds",
                "2",
                "--rebalance-max-mark-age-seconds",
                "10",
            ]
        )
        == 0
    )
    result = json.loads(capsys.readouterr().out)
    assert result == scheduled()


def test_schedule_keeps_position_and_cash_in_exact_native_split_units():
    document = json.loads(scheduled_fixture())
    for row in document["quotes"]:
        if row["symbol"] == "TSLA" and row["observed_at"] > timestamp(4).isoformat():
            row.update(bid_usd="10.00", ask_usd="10.00")
    document["stock_splits"] = [
        {
            "action_id": "split",
            "symbol": "TSLA",
            "available_at": timestamp(3).isoformat(),
            "effective_at": timestamp(4).isoformat(),
            "old_shares": 1,
            "new_shares": 2,
        }
    ]
    report = run_benchmarks(
        load_replay(json.dumps(document).encode()),
        start_at=timestamp(0),
        buy_and_hold_symbol="TSLA",
        equal_weight_schedule=EqualWeightSchedule((timestamp(5),), 2, 10),
    )["benchmarks"]["equal_weight_scheduled"]
    assert report["open_positions"] == {"NVDA": "122", "TSLA": "60"}
    sale = next(fill for fill in report["fills"] if ":sell:" in fill["intent_id"])
    assert (sale["quantity"], sale["price_usd"], sale["commission_usd"]) == ("38", "10.00", "3.80")
    assert report["ending_cash_usd"] == "17.75"


def test_stale_held_price_defers_rebalance_and_reports_missing_mark():
    document = json.loads(scheduled_fixture())
    document["quotes"] = [
        row
        for row in document["quotes"]
        if not (row["symbol"] == "NVDA" and row["observed_at"] > timestamp(2).isoformat())
    ]
    report = run_benchmarks(
        load_replay(json.dumps(document).encode()),
        start_at=timestamp(0),
        buy_and_hold_symbol="TSLA",
        equal_weight_schedule=EqualWeightSchedule((timestamp(5),), 2, 1),
    )["benchmarks"]["equal_weight_scheduled"]
    assert report["rebalances"][1]["status"] == "unvalued_holding"
    assert report["ending_symbols_without_fresh_mark"] == ["NVDA"]
    assert len(report["fills"]) == 2


def test_schedule_requires_ordered_aware_nonoverlapping_cutoffs():
    with pytest.raises(ValueError, match="increasing"):
        EqualWeightSchedule((timestamp(10), timestamp(5)), 2, 10)
    with pytest.raises(ValueError, match="timezone"):
        EqualWeightSchedule((timestamp(10).replace(tzinfo=None),), 2, 10)
    replay = load_replay(fixture())
    with pytest.raises(ValueError, match="overlap"):
        run_benchmarks(
            replay,
            start_at=timestamp(0),
            buy_and_hold_symbol="TSLA",
            equal_weight_schedule=EqualWeightSchedule((timestamp(1),), 2, 10),
        )
