"""Deterministic fixture plumbing demo, explicitly not AI market analysis."""

from datetime import UTC, datetime
from uuid import uuid4

from tradingbot.contracts import IntentProposals, PortfolioSnapshot
from tradingbot.research import collect_reports, create_decisions


class SyntheticGraph:
    def propagate(self, symbol, trade_date, *, portfolio):
        return {
            "market_report": f"SYNTHETIC FIXTURE: {symbol}; no real market observations.",
            "final_trade_decision": "Hold: fixture tests plumbing, not strategy returns.",
            "final_rating": "Hold",
        }, "Hold"


class SyntheticDecisionModel:
    def __init__(self, symbols):
        self.symbols = symbols

    def with_structured_output(self, schema):
        return self

    def invoke(self, prompt):
        return IntentProposals.model_validate(
            {
                "intents": [
                    dict(
                        symbol=symbol,
                        action="HOLD",
                        notional_usd="0",
                        confidence=None,
                        expected_return_pct=None,
                        horizon=dict(min_hours=24, max_hours=720),
                        thesis=["SYNTHETIC FIXTURE; no market thesis or alpha estimate"],
                        invalidation_conditions=["Replace fixture with actual upstream reports"],
                        evidence_ids=[f"{symbol}:market_report"],
                        max_execution_cost_bps=10,
                    )
                    for symbol in self.symbols
                ]
            }
        )


def demo_batch():
    book = PortfolioSnapshot(snapshot_id=uuid4(), as_of=datetime.now(UTC), cash_usd="1000")
    symbols = ["TSLA", "NVDA"]
    reports = collect_reports(SyntheticGraph(), symbols, book.as_of.date().isoformat(), book)
    batch = create_decisions(SyntheticDecisionModel(symbols), reports, book)
    return batch, reports, book
