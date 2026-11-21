import json
from contextlib import nullcontext
from pathlib import Path
from types import SimpleNamespace

import pytest

from tradingbot import perp_research
from tradingbot.adapters.tradingagents import TradingAgentsAdapter
from tradingbot.demo import SyntheticGraph
from tradingbot.perp_research import PerpProposal, PerpScreen, assess_proposal
from tradingbot.research import DecisionError

ROOT = Path(__file__).resolve().parents[3]
SHORT = "FIXTURE.TSLA:SHORT:1200:2"
LONG = "FIXTURE.NVDA:LONG:800:1"


@pytest.fixture
def screen():
    def market(symbol, side, option_id, cost, leverage):
        return {
            "market_id": f"FIXTURE.{symbol}",
            "security_id": f"fixture-{symbol.lower()}",
            "symbol": symbol,
            "observation_id": f"fixture-{symbol.lower()}-observation",
            "observed_at": "2026-10-05T14:34:50Z",
            "available_at": "2026-10-05T14:34:51Z",
            "mark_usd": "300",
            "index_usd": "300",
            "mark_stress_bps": 600,
            "status": "eligible",
            "options": [{
                "option_id": option_id,
                "side": side,
                "leverage": leverage,
                "notional_usd": "1200" if side == "SHORT" else "800",
                "initial_margin_usd": "600" if side == "SHORT" else "800",
                "scenario_loss_usd": "91.08",
                "scenario_headroom_usd": "431.88",
                "projected_funding_cost_bps": -8 if side == "SHORT" else 6,
                "estimated_total_cost_bps": cost,
            }],
        }

    return PerpScreen.model_validate({
        "schema_version": "0.1",
        "source_mode": "observed_market",
        "study_sha256": "a" * 64,
        "as_of": "2026-10-05T14:35:00Z",
        "horizon_hours": 24,
        "available_collateral_usd": "1000",
        "min_net_edge_bps": 25,
        "markets": [market("TSLA", "SHORT", SHORT, 16, 2), market("NVDA", "LONG", LONG, 26, 1)],
    })


def short_fixture():
    raw = (ROOT / "services/research/examples/perp-research-forecast.json").read_bytes()
    return PerpProposal.model_validate_json(raw)


def test_screen_uses_the_published_wire_schema():
    published = json.loads((ROOT / "contracts/perp-research-screen.schema.json").read_bytes())
    assert published == PerpScreen.model_json_schema(mode="serialization")


def test_observed_research_derives_universe_and_accepts_typed_long(
    screen, tmp_path, monkeypatch,
):
    study = tmp_path / "study.json"
    study.write_text("fixture", encoding="utf-8")
    monkeypatch.setattr(perp_research, "screen_study", lambda binary, raw: screen)
    monkeypatch.setattr(perp_research, "capture_usage", lambda *args: nullcontext())
    monkeypatch.setattr(perp_research, "usage_model_identities", lambda config: {})

    symbols = []

    class RecordingGraph(SyntheticGraph):
        def propagate(self, symbol, *args, **kwargs):
            symbols.append(symbol)
            return super().propagate(symbol, *args, **kwargs)

    class Model:
        def with_structured_output(self, schema):
            assert schema is PerpProposal
            return self

        def invoke(self, prompt):
            payload = json.loads(prompt[1]["content"])
            assert len(payload["markets"]) == 2 and "NVDA:market_report" in payload["equity_reports"]
            return short_fixture().model_copy(update={
                "option_id": LONG,
                "evidence_ids": (screen.markets[1].evidence_id, "NVDA:market_report"),
            })

    runtime = SimpleNamespace(
        adapter=TradingAgentsAdapter(RecordingGraph()), config={}, decision_model=Model,
    )
    monkeypatch.setattr(perp_research, "create_runtime", lambda *args, **kwargs: runtime)
    result = perp_research.run_perp_research(SimpleNamespace(
        study=study, screen_binary=None, fixture_forecast=None, config=None,
        usage_output=None, max_calls=64,
    ))
    assert symbols == ["NVDA", "TSLA"]
    assert result["action"] == "LONG"
    assert result["selected"]["symbol"] == "NVDA"
    assert result["selected"]["leverage"] == 1
    assert result["estimated_net_return_bps"] == 114


def test_short_requires_edge_after_directional_costs(screen):
    reports = {"TSLA": {"market_report": "Synthetic evidence"}}
    proposal = short_fixture()
    result = assess_proposal(screen, reports, proposal)
    assert result["action"] == "SHORT" and result["selected"]["leverage"] == 2
    assert result["estimated_net_return_bps"] == 124
    weak = assess_proposal(screen, reports, proposal.model_copy(update={"gross_return_bps": 40}))
    assert weak["action"] == "NO_TRADE" and weak["selected"] is None
    assert weak["estimated_net_return_bps"] == 24


@pytest.mark.parametrize("change", [
    {"option_id": "FIXTURE.TSLA:SHORT:1200:5"},
    {"evidence_ids": ("market:FIXTURE.TSLA:fixture-tsla-observation", "NVDA:market_report")},
    {"review_in_hours": 25},
    {"invalidation_conditions": ()},
    {"option_id": None},
])
def test_invalid_model_output_cannot_become_a_position(screen, change):
    reports = {
        "TSLA": {"market_report": "Synthetic TSLA evidence"},
        "NVDA": {"market_report": "Synthetic NVDA evidence"},
    }
    with pytest.raises(DecisionError):
        assess_proposal(screen, reports, short_fixture().model_copy(update=change))


def test_abstention_and_empty_screen_remain_flat(screen, tmp_path, monkeypatch):
    proposal = short_fixture().model_copy(update={
        "option_id": None, "gross_return_bps": None, "review_in_hours": None, "evidence_ids": (),
    })
    assert assess_proposal(screen, {}, proposal)["reason"] == "abstained"

    empty = screen.model_copy(update={"markets": tuple(
        market.model_copy(update={"options": (), "status": "stale_observation"})
        for market in screen.markets
    )})
    monkeypatch.setattr(perp_research, "screen_study", lambda *args: empty)
    study = tmp_path / "study.json"
    study.write_bytes(b"fixture")
    result = perp_research.run_perp_research(SimpleNamespace(
        study=study, screen_binary=None, fixture_forecast=None, max_calls=64, usage_output=None,
    ))
    assert result["action"] == "NO_TRADE" and result["reason"] == "no_eligible_options"
    assert result["llm_calls"] == 0
