from datetime import UTC, datetime

import pytest
from test_replay import order, quote, replay_input

from tradingbot_evaluation.replay import load_replay, run_replay


def at(seconds):
    return f"2026-10-02T13:30:{seconds:02}Z"


def dividend(**changes):
    return {
        "action_id": "TSLA-dividend",
        "symbol": "TSLA",
        "available_at": at(8),
        "entitlement_at": at(13),
        "paid_at": at(15),
        "cash_per_share_usd": "1.37",
    } | changes


def test_native_cash_credit_uses_shares_held_at_entitlement_after_position_is_sold():
    fixture = load_replay(
        replay_input(
            quotes=[
                quote("TSLA", 12, "99.00", "100.00"),
                quote("TSLA", 14, "110.00", "111.00"),
                quote("TSLA", 16, "109.00", "110.00"),
            ],
            orders=[
                order("enter", "TSLA", "BUY", 2, 10, 20),
                order("exit", "TSLA", "SELL", 2, 13, 20),
            ],
            cash_dividends=[dividend()],
        )
    )
    result = run_replay(fixture)
    assert [(fill["price_usd"], fill["commission_usd"]) for fill in result["fills"]] == [
        ("100.00", "2.00"),
        ("110.00", "2.20"),
    ]
    assert result["open_positions"] == {}
    assert result["simulated_commission_usd"] == "4.20"
    assert result["ending_cash_usd"] == "1018.54"
    assert result["simulated_dividend_cash_usd"] == "2.74"
    (event,) = result["cash_dividend_events"]
    assert event["action_id"] == "TSLA-dividend"
    assert event["entitled_shares"] == "2"
    assert event["status"] == "paid"


def test_boundary_precedes_equal_time_fill_and_native_cash_is_available_at_payment():
    result = run_replay(
        load_replay(
            replay_input(
                starting_cash_usd="100.00",
                commission_bps=0,
                quotes=[
                    quote("TSLA", 12, "89.00", "90.00"),
                    quote("TSLA", 13, "9.00", "10.00"),
                    quote("NVDA", 15, "0.90", "1.00"),
                ],
                orders=[
                    order("before_record", "TSLA", "BUY", 1, 10, 20),
                    order("on_record", "TSLA", "BUY", 1, 12, 20),
                    order("funded_by_dividend", "NVDA", "BUY", 1, 14, 20),
                ],
                cash_dividends=[dividend()],
            )
        )
    )
    assert {fill["intent_id"] for fill in result["fills"]} == {
        "before_record",
        "on_record",
        "funded_by_dividend",
    }
    assert result["ending_cash_usd"] == "0.37"
    assert result["open_positions"] == {"TSLA": "2", "NVDA": "1"}
    assert result["cash_dividend_events"][0]["entitled_shares"] == "1"


def test_pending_dividend_has_no_cash_until_the_explicit_later_horizon():
    fixture = load_replay(
        replay_input(
            commission_bps=0,
            quotes=[quote("TSLA", 12, "99.00", "100.00"), quote("TSLA", 14, "99.00", "100.00")],
            orders=[order("enter", "TSLA", "BUY", 1, 10, 20)],
            cash_dividends=[
                dividend(),
                dividend(
                    action_id="future", available_at=at(17), entitlement_at=at(18), paid_at=at(19)
                ),
            ],
        )
    )
    pending = run_replay(fixture)
    assert pending["ending_cash_usd"] == "900.00"
    assert pending["simulated_dividend_cash_usd"] == "0.00"
    assert [event["status"] for event in pending["cash_dividend_events"]] == ["pending_payment"]
    paid = run_replay(fixture, through=datetime(2026, 10, 2, 13, 30, 16, tzinfo=UTC))
    assert paid["ending_cash_usd"] == "901.37"
    assert paid["simulated_dividend_cash_usd"] == "1.37"
    assert [event["action_id"] for event in paid["cash_dividend_events"]] == ["TSLA-dividend"]


@pytest.mark.parametrize(
    "events",
    [
        [dividend(available_at=at(14))],
        [dividend(paid_at=at(13))],
        [dividend(symbol="UNKNOWN")],
        [dividend(cash_per_share_usd="0.00")],
        [dividend(cash_per_share_usd="0.013")],
        [dividend(), dividend()],
    ],
)
def test_invalid_or_ambiguous_cash_dividend_records_are_rejected(events):
    with pytest.raises(ValueError):
        load_replay(replay_input(cash_dividends=events))
