import json
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.replay import Replay, load_replay, run_replay, synthetic_probe


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
