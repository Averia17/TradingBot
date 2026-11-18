import json
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.replay import (
    Replay,
    load_replay,
    replay_prefix,
    run_replay,
    synthetic_probe,
)


def order(intent_id, symbol, side, quantity, ready_at, expires_at):
    return dict(
        intent_id=intent_id,
        symbol=symbol,
        side=side,
        quantity=quantity,
        ready_at=f"2026-10-02T13:30:{ready_at:02}Z",
        expires_at=f"2026-10-02T13:30:{expires_at:02}Z",
    )


def quote(symbol, seconds, bid, ask, *, available=None, size=1000):
    available = seconds if available is None else available
    return dict(
        symbol=symbol,
        observed_at=f"2026-10-02T13:30:{seconds:02}Z",
        available_at=f"2026-10-02T13:30:{available:02}Z",
        bid_usd=bid,
        ask_usd=ask,
        bid_size=size,
        ask_size=size,
    )


def replay_input(**overrides):
    fields = dict(
        schema_version="0.1",
        source_mode="synthetic_fixture",
        starting_cash_usd="1000.00",
        commission_bps=100,
        quotes=[
            quote("TSLA", 13, "110.00", "111.00"),
            quote("TSLA", 10, "79.00", "80.00"),
            quote("TSLA", 9, "89.00", "90.00", available=11),
            quote("TSLA", 12, "99.00", "100.00"),
        ],
        orders=[
            order("enter", "TSLA", "BUY", 1, 10, 20),
            order("exit", "TSLA", "SELL", 1, 12, 20),
        ],
    )
    return json.dumps(fields | overrides).encode()


def execution_window(record_id, start, end, status, *, available=None, symbol="TSLA"):
    return dict(
        record_id=record_id,
        symbol=symbol,
        starts_at=f"2026-10-02T13:30:{start:02}Z",
        ends_before=f"2026-10-02T13:30:{end:02}Z",
        available_at=f"2026-10-02T13:30:{start if available is None else available:02}Z",
        status=status,
    )


def test_native_replay_waits_for_new_quote_after_halt_and_honors_expiry():
    fixture = (
        Path(__file__).resolve().parents[3] / "contracts" / "valid-evaluation-replay-calendar.json"
    )
    replay = load_replay(fixture.read_bytes())
    result = run_replay(replay)
    assert result["intents"] == {
        "enter": "filled",
        "halt-expired": "expired",
        "close-expired": "expired",
    }
    assert [(fill["price_usd"], fill["execution_window_id"]) for fill in result["fills"]] == [
        ("105.00", "reopen")
    ]
    assert result["ending_bid_mark_nav_usd"] == "997.95"
    assert result["execution_calendar"]["blocked_quotes"] == {
        "halted": 1,
        "outside_observation_window": 1,
        "closed": 1,
    }
    assert result["execution_calendar"]["held_bid_marks"]["TSLA"]["execution_window_id"] == "reopen"
    benchmark = run_benchmarks(
        replay,
        start_at=replay.quotes[0].available_at,
        buy_and_hold_symbol="TSLA",
    )
    assert benchmark["benchmarks"]["buy_and_hold"]["fills"][0]["execution_window_id"] == "reopen"
    assert benchmark["benchmarks"]["buy_and_hold"]["ending_bid_mark_nav_usd"] == "981.55"


def test_unknown_availability_blocks_quotes_and_keeps_open_expiry_distinct():
    replay = load_replay(
        replay_input(
            quotes=[
                quote("TSLA", 14, "99.00", "100.00"),
                quote("NVDA", 17, "104.00", "105.00"),
            ],
            orders=[
                order("expired", "TSLA", "BUY", 1, 10, 15),
                order("unexecuted", "NVDA", "BUY", 1, 10, 20),
            ],
            execution_calendar={
                "calendar_id": "fixture-uncertain",
                "windows": [execution_window("announced-late", 11, 21, "open", available=16)],
            },
        )
    )
    result = run_replay(replay)
    assert result["fills"] == []
    assert result["ending_bid_mark_nav_usd"] == "1000.00"
    assert result["intents"] == {"expired": "expired", "unexecuted": "no_executable_quote"}
    assert result["execution_calendar"]["blocked_quotes"] == {"unknown": 2}
    prefix = replay_prefix(replay, replay.quotes[0].available_at)
    assert prefix.execution_calendar.windows == ()
    assert run_replay(prefix)["execution_calendar"]["blocked_quotes"] == {"unknown": 1}


def test_window_notice_can_follow_observation_if_known_at_quote_arrival():
    replay = load_replay(
        replay_input(
            quotes=[
                quote("TSLA", 10, "99.00", "100.00", available=13)
                | {"available_at": "2026-10-02T15:30:13+02:00"}
            ],
            orders=[order("enter", "TSLA", "BUY", 1, 9, 20)],
            execution_calendar={
                "calendar_id": "fixture-offsets",
                "windows": [
                    execution_window("known-on-arrival", 10, 18, "open", available=12)
                    | {"starts_at": "2026-10-02T09:30:10-04:00"}
                ],
            },
        )
    )
    result = run_replay(replay)
    assert result["intents"] == {"enter": "filled"}
    assert result["fills"][0]["execution_window_id"] == "known-on-arrival"
    assert result["execution_calendar"]["policy"] == (
        "observation_and_arrival_in_same_window_known_at_arrival"
    )


@pytest.mark.parametrize(
    "windows,message",
    [
        ([execution_window("equal", 12, 12, "open")], "start before"),
        (
            [execution_window("one", 8, 15, "open"), execution_window("two", 14, 16, "halted")],
            "overlap",
        ),
        (
            [execution_window("repeat", 8, 10, "open"), execution_window("repeat", 10, 12, "open")],
            "duplicate",
        ),
        (
            [execution_window("unbound", 8, 12, "open", symbol="NVDA")],
            "quoted symbol",
        ),
        (
            [
                execution_window("ambiguous", 8, 12, "open")
                | {"ends_before": "2026-10-02T13:30:12.000000001Z"}
            ],
            "microsecond",
        ),
    ],
)
def test_invalid_execution_calendar_fails_closed(windows, message):
    with pytest.raises(ValueError, match=message):
        load_replay(
            replay_input(execution_calendar={"calendar_id": "fixture-invalid", "windows": windows})
        )


def test_native_smoke_matches_research_probe_outcomes():
    result = synthetic_probe()
    assert result["source_mode"] == "synthetic_fixture"
    assert result["alpha_measured"] is False
    assert result["ending_cash_usd"] == "700.00"
    assert result["open_positions"] == {"NVDA": "2", "TSLA": "1"}
    assert len(result["fills"]) == 2


def test_native_replay_fills_only_strictly_after_intent_and_charges_commission():
    result = run_replay(load_replay(replay_input()))
    assert [
        (fill["intent_id"], fill["price_usd"], fill["commission_usd"]) for fill in result["fills"]
    ] == [
        ("enter", "100.00", "1.00"),
        ("exit", "110.00", "1.10"),
    ]
    assert result["ending_cash_usd"] == "1007.90"
    assert result["open_positions"] == {}
    assert result["simulated_commission_usd"] == "2.10"
    assert result["cash_benchmark_nav_usd"] == "1000.00"
    assert result["external_costs_usd"] is None


def test_expired_order_and_unfunded_orders_cannot_fill():
    data = replay_input(
        starting_cash_usd="90.00",
        orders=[
            order("expired", "TSLA", "BUY", 1, 10, 12),
            order("unfunded", "TSLA", "BUY", 1, 10, 20),
            order("short", "TSLA", "SELL", 1, 10, 20),
        ],
    )
    result = run_replay(load_replay(data))
    assert result["fills"] == []
    assert result["ending_cash_usd"] == "90.00"
    assert result["intents"]["expired"] == "expired"


def test_price_gap_cannot_report_filled_when_no_execution_was_booked():
    result = run_replay(load_replay(replay_input(orders=[order("gap", "TSLA", "BUY", 9, 12, 20)])))
    assert result["fills"] == []
    assert result["ending_cash_usd"] == "1000.00"
    assert result["intents"]["gap"] == "no_accounted_fill"


def test_two_orders_share_one_displayed_unit_of_quote_liquidity():
    result = run_replay(
        load_replay(
            replay_input(
                quotes=[quote("TSLA", 12, "99.00", "100.00", size=1)],
                orders=[
                    order("first", "TSLA", "BUY", 1, 10, 20),
                    order("second", "TSLA", "BUY", 1, 10, 20),
                ],
            )
        )
    )
    assert result["open_positions"] == {"TSLA": "1"}
    assert len(result["fills"]) == 1


def test_same_time_orders_use_one_cash_account():
    replay = load_replay(
        replay_input(
            starting_cash_usd="100.00",
            quotes=[quote("TSLA", 12, "74.00", "75.00"), quote("NVDA", 12, "74.00", "75.00")],
            orders=[
                order("first", "TSLA", "BUY", 1, 10, 20),
                order("second", "NVDA", "BUY", 1, 10, 20),
            ],
        ),
    )
    result = run_replay(replay)
    assert len(result["fills"]) == 1
    assert result["ending_cash_usd"] == "24.25"


def test_published_replay_schema_matches_decoder():
    contract = Path(__file__).resolve().parents[3] / "contracts" / "evaluation-replay.schema.json"
    schema = json.loads(contract.read_text(encoding="utf-8"))
    assert schema.pop("$schema") == "https://json-schema.org/draft/2020-12/schema"
    assert schema == json.loads(msgspec.json.encode(msgspec.json.schema(Replay)))


def test_duplicate_json_fields_fail_closed():
    duplicate = replay_input().replace(
        b'"schema_version":', b'"schema_version": "0.1", "schema_version":'
    )
    with pytest.raises(ValueError, match="Duplicate replay object field"):
        load_replay(duplicate)


@pytest.mark.parametrize(
    "fields",
    [
        {"starting_cash_usd": "1.001"},
        {"starting_cash_usd": 1000},
        {"source_mode": "live"},
        {"quotes": [quote("TSLA", 12, "2.00", "1.00")]},
        {"quotes": [quote("TSLA", 12, "1.00", "2.00", available=11)]},
        {"quotes": [quote("TSLA", 12, "1.00", "2.00")] * 2},
        {"orders": [order("same", "TSLA", "BUY", 1, 10, 20)] * 2},
        {"unexpected": True},
    ],
)
def test_invalid_or_ambiguous_replay_inputs_fail_closed(fields):
    with pytest.raises(ValueError):
        load_replay(replay_input(**fields))
