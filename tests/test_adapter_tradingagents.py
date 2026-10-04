import ast
from datetime import UTC, datetime
from pathlib import Path
from uuid import uuid4

import pytest

from tradingbot.adapters.tradingagents import (
    TradingAgentsAdapter,
    UpstreamCompatibilityError,
    check_compatibility,
    create_runtime,
    research_config,
)
from tradingbot.contracts import PortfolioSnapshot


def test_installed_upstream_contract_is_checked_without_clients_or_network():
    result = check_compatibility()
    assert result["compatible"] is True
    assert result["adapter_contract"] == "tradingagents-research-v1"
    assert result["version"]


def test_adapter_projects_only_free_cash_and_real_quantities_to_upstream():
    portfolio = PortfolioSnapshot(
        snapshot_id=uuid4(),
        as_of=datetime(2026, 10, 4, tzinfo=UTC),
        cash_usd="1000",
        reserved_cash_usd="150",
        positions=(
            dict(symbol="TSLA", instrument_id="test-tsla", quantity="1.25", mark_price_usd="100"),
        ),
    )
    context = TradingAgentsAdapter.portfolio_context(portfolio)
    assert context.cash == 850
    assert context.currency == "USD"
    assert context.position_in("TSLA").quantity == 1.25
    assert context.position_in("TSLA").average_price is None


def test_changed_graph_interface_is_rejected_before_research():
    class ChangedGraph:
        def propagate(self, symbol, trade_date):
            raise AssertionError("must fail before upstream research")

    with pytest.raises(UpstreamCompatibilityError, match="propagate.*portfolio"):
        TradingAgentsAdapter(ChangedGraph())


def test_graph_without_propagate_is_an_explicit_compatibility_error():
    with pytest.raises(UpstreamCompatibilityError, match="propagate"):
        TradingAgentsAdapter(object())


def test_upstream_with_a_new_version_can_pass_the_same_capability_contract(monkeypatch):
    from tradingbot.adapters import tradingagents

    monkeypatch.setattr(tradingagents, "version", lambda _: "9.0.0")
    assert check_compatibility()["version"] == "9.0.0"


def test_missing_portfolio_capability_in_installed_upstream_is_detected(monkeypatch):
    from tradingagents.graph.trading_graph import TradingAgentsGraph

    monkeypatch.setattr(TradingAgentsGraph, "propagate", lambda self, symbol, date: None)
    with pytest.raises(UpstreamCompatibilityError, match="propagate/portfolio"):
        check_compatibility()


@pytest.mark.parametrize("result", [None, {}, ("Buy", "Buy"), ({},), ({}, "Hold", "extra")])
def test_changed_result_shape_is_a_compatibility_error(result):
    class ChangedGraph:
        def propagate(self, symbol, trade_date, *, portfolio):
            return result

    portfolio = PortfolioSnapshot(
        snapshot_id=uuid4(), as_of=datetime(2026, 10, 4, tzinfo=UTC), cash_usd="1000"
    )
    with pytest.raises(UpstreamCompatibilityError, match="result"):
        TradingAgentsAdapter(ChangedGraph()).collect_reports(["TSLA"], "2026-10-04", portfolio)


def test_upstream_configuration_keeps_resource_and_storage_policy_in_our_adapter(tmp_path):
    config = research_config(tmp_path, {"quick_think_llm": "custom-model"})
    assert config["quick_think_llm"] == "custom-model"
    assert config["max_tool_rounds"] == 3
    assert config["llm_max_retries"] == 0
    assert config["checkpoint_enabled"] is False
    assert config["data_cache_dir"] == str(tmp_path / "cache")
    with pytest.raises(ValueError, match="only model/provider/endpoint"):
        research_config(tmp_path, {"checkpoint_enabled": True})


def test_real_upstream_graph_runs_through_our_adapter_with_offline_models(tmp_path, monkeypatch):
    from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel
    from langchain_core.messages import AIMessage
    from pydantic import Field
    from tradingagents import llm_clients
    from tradingagents.graph import trading_graph

    class OfflineModel(FakeMessagesListChatModel):
        observed_prompts: list[str] = Field(default_factory=list)

        def bind_tools(self, tools, **kwargs):
            return self

        def _generate(self, messages, stop=None, run_manager=None, **kwargs):
            self.observed_prompts.extend(str(message.content) for message in messages)
            return super()._generate(messages, stop=stop, run_manager=run_manager, **kwargs)

    model = OfflineModel(responses=[AIMessage(content="Synthetic report.\nRating: Hold")])

    class OfflineClient:
        def get_llm(self):
            return model

    def offline_client(config, tier, **kwargs):
        return OfflineClient()

    monkeypatch.setattr(trading_graph, "create_tier_client", offline_client)
    monkeypatch.setattr(llm_clients, "create_tier_client", offline_client)
    runtime = create_runtime(tmp_path, callbacks=[])
    monkeypatch.setattr(
        runtime.adapter.graph, "resolve_instrument_context", lambda *a, **k: "Synthetic identity"
    )
    portfolio = PortfolioSnapshot(
        snapshot_id=uuid4(),
        as_of=datetime(2026, 1, 9, tzinfo=UTC),
        cash_usd="1000",
        reserved_cash_usd="150",
        positions=(
            dict(symbol="TSLA", instrument_id="test-tsla", quantity="1.25", mark_price_usd="100"),
        ),
    )
    reports = runtime.adapter.collect_reports(["TSLA"], "2026-01-09", portfolio)
    assert reports["TSLA"]["final_trade_decision"].strip()
    assert set(reports["TSLA"]) >= {"market_report", "news_report", "fundamentals_report"}
    assert runtime.decision_model() is model
    assert runtime.adapter.graph.selected_analysts == ("market", "fundamentals", "news")
    assert any("Cash available: 850.00 USD" in prompt for prompt in model.observed_prompts)
    assert any("Current position in TSLA: 1.25 units" in prompt for prompt in model.observed_prompts)


def test_upstream_imports_are_confined_to_replaceable_adapters():
    source = Path(__file__).resolve().parents[1] / "src" / "tradingbot"
    imports_outside_adapter = []
    for file in source.rglob("*.py"):
        if "adapters" in file.relative_to(source).parts:
            continue
        for node in ast.walk(ast.parse(file.read_text(encoding="utf-8"))):
            names = (
                [node.module or ""]
                if isinstance(node, ast.ImportFrom)
                else [alias.name for alias in node.names]
                if isinstance(node, ast.Import)
                else []
            )
            if any(name == "tradingagents" or name.startswith("tradingagents.") for name in names):
                imports_outside_adapter.append(str(file.relative_to(source)))
    assert imports_outside_adapter == []
