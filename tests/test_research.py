from datetime import UTC, datetime
from uuid import uuid4

import pytest

from tradingbot.contracts import IntentProposals, PortfolioSnapshot
from tradingbot.research import DecisionError, collect_reports, create_decisions


def book():
    return PortfolioSnapshot(
        snapshot_id=uuid4(), as_of=datetime(2026, 10, 4, tzinfo=UTC), cash_usd="1000"
    )


def held_proposal(symbol="TSLA", **changes):
    return (
        dict(
            symbol=symbol,
            action="HOLD",
            notional_usd="0",
            confidence=None,
            expected_return_pct=None,
            horizon=dict(min_hours=24, max_hours=720),
            thesis=["No authenticated execution quotes; research only"],
            invalidation_conditions=["New issuer event"],
            evidence_ids=[f"{symbol}:market_report"],
            max_execution_cost_bps=10,
        )
        | changes
    )


class FixtureGraph:
    def __init__(self):
        self.books = []

    def propagate(self, symbol, trade_date, *, portfolio):
        self.books.append(portfolio)
        return {
            "market_report": f"SYNTHETIC {symbol} evidence",
            "final_trade_decision": "Hold",
            "final_rating": "Hold",
        }, "Hold"


class FixtureModel:
    def __init__(self, result):
        self.result = result
        self.prompts = []

    def with_structured_output(self, schema):
        assert schema is IntentProposals
        return self

    def invoke(self, prompt):
        self.prompts.append(prompt)
        return self.result


def test_each_upstream_run_sees_same_real_portfolio_context():
    graph = FixtureGraph()
    reports = collect_reports(graph, ["TSLA", "NVDA"], "2026-10-04", book())
    assert set(reports) == {"TSLA", "NVDA"}
    assert all(p.cash == 1000 for p in graph.books)
    assert len(graph.books) == 2


def test_typed_batch_has_trusted_ids_and_is_explicitly_unexecutable():
    model = FixtureModel(
        IntentProposals.model_validate({"intents": [held_proposal(), held_proposal("NVDA")]})
    )
    portfolio = book()
    reports = collect_reports(FixtureGraph(), ["TSLA", "NVDA"], "2026-10-04", portfolio)
    result = create_decisions(model, reports, portfolio)
    assert result.portfolio_snapshot_id == portfolio.snapshot_id
    assert result.execution_mode == "research"
    assert [p.symbol for p in result.intents] == ["TSLA", "NVDA"]
    assert result.run_id not in {i.decision_id for i in result.intents}
    assert len(model.prompts) == 1  # One portfolio arbitration, not one cash allocation per ticker.


@pytest.mark.parametrize("result", [None, "BUY TSLA $200", {"intents": [held_proposal()]}])
def test_structured_output_failure_has_no_free_text_execution_fallback(result):
    reports = collect_reports(FixtureGraph(), ["TSLA"], "2026-10-04", book())
    with pytest.raises(DecisionError, match="typed"):
        create_decisions(FixtureModel(result), reports, book())


def test_fabricated_evidence_is_rejected():
    reports = collect_reports(FixtureGraph(), ["TSLA"], "2026-10-04", book())
    model = FixtureModel(
        IntentProposals.model_validate({"intents": [held_proposal(evidence_ids=["unseen-filing"])]})
    )
    with pytest.raises(DecisionError, match="evidence"):
        create_decisions(model, reports, book())


def test_backdated_snapshot_cannot_be_labelled_today_analysis():
    with pytest.raises(ValueError, match="date"):
        collect_reports(FixtureGraph(), ["TSLA"], "2026-10-05", book())


def test_unrecognized_upstream_rating_is_not_silently_treated_as_hold():
    class BrokenGraph(FixtureGraph):
        def propagate(self, *args, **kwargs):
            return {"final_rating": "ERROR", "final_trade_decision": "BUY"}, "BUY"

    with pytest.raises(DecisionError, match="rating"):
        collect_reports(BrokenGraph(), ["TSLA"], "2026-10-04", book())
