from datetime import UTC, datetime
from decimal import Decimal
from uuid import uuid4

import pytest
from pydantic import ValidationError

from tradingbot.contracts import IntentProposal, PortfolioSnapshot, Position, validate_proposals


def portfolio(**changes):
    return PortfolioSnapshot(
        snapshot_id=uuid4(),
        as_of=datetime(2026, 10, 4, tzinfo=UTC),
        cash_usd="1000",
        **changes,
    )


def proposal(symbol="TSLA", action="BUY", amount="200", **changes):
    return IntentProposal.model_validate(
        dict(
            symbol=symbol,
            action=action,
            notional_usd=amount,
            confidence=0.6,
            expected_return_pct=4.0,
            horizon=dict(min_hours=24, max_hours=720),
            thesis=["Test scenario; not a market forecast"],
            invalidation_conditions=["Test thesis invalidated"],
            evidence_ids=[f"{symbol}:market_report"],
            max_execution_cost_bps=10,
        )
        | changes
    )


def test_two_buys_cannot_spend_the_same_reserved_cash():
    book = portfolio(reserved_cash_usd="650")
    with pytest.raises(ValueError, match="cash"):
        validate_proposals([proposal(), proposal("NVDA")], book, ["TSLA", "NVDA"])


def test_position_limit_counts_existing_exposure_when_adding():
    held = Position(symbol="TSLA", quantity="1", mark_price_usd="200", instrument_id="paper:TSLA")
    book = portfolio(positions=[held])
    with pytest.raises(ValueError, match="position"):
        validate_proposals([proposal(action="ADD", amount="150")], book, ["TSLA"])


def test_sell_requires_an_existing_position():
    with pytest.raises(ValueError, match="held"):
        validate_proposals([proposal(action="SELL")], portfolio(), ["TSLA"])


def test_unknown_symbols_and_duplicate_decisions_are_rejected():
    with pytest.raises(ValueError, match="symbols"):
        validate_proposals([proposal("FAKE")], portfolio(), ["TSLA"])
    with pytest.raises(ValueError, match="duplicate"):
        validate_proposals([proposal(), proposal()], portfolio(), ["TSLA"])


@pytest.mark.parametrize(
    "change",
    [
        {"confidence": float("nan")},
        {"action": "YOLO"},
        {"withdraw_to": "attacker"},
        {"notional_usd": "-1"},
    ],
)
def test_nonfinite_invalid_action_unknown_field_and_negative_amount_rejected(change):
    with pytest.raises(ValidationError):
        proposal(**change)


def test_hold_does_not_create_an_order_and_can_admit_unknown_estimates():
    hold = proposal(action="HOLD", amount="0", confidence=None, expected_return_pct=None)
    validate_proposals([hold], portfolio(), ["TSLA"])
    assert hold.notional_usd == Decimal("0")


def test_non_hold_must_have_forecast_and_evidence():
    with pytest.raises(ValidationError):
        proposal(expected_return_pct=None)
    with pytest.raises(ValueError, match="evidence"):
        validate_proposals([proposal(evidence_ids=[])], portfolio(), ["TSLA"])


def test_portfolio_rejects_naive_time_and_reserved_cash_exceeding_balance():
    with pytest.raises(ValidationError):
        portfolio(reserved_cash_usd="1001")
    with pytest.raises(ValidationError):
        PortfolioSnapshot(snapshot_id=uuid4(), as_of=datetime(2026, 10, 4), cash_usd="1000")


def test_sale_proceeds_are_not_available_to_another_intent_before_fill():
    held = Position(symbol="TSLA", quantity="10", mark_price_usd="100", instrument_id="paper:TSLA")
    book = PortfolioSnapshot(
        snapshot_id=uuid4(), as_of=datetime(2026, 10, 4, tzinfo=UTC), cash_usd="0", positions=[held]
    )
    with pytest.raises(ValueError, match="cash"):
        validate_proposals(
            [proposal(action="SELL", amount="1000"), proposal("NVDA")], book, ["TSLA", "NVDA"]
        )
