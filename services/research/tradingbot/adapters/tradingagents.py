"""The only boundary that knows TradingAgents imports, config and result fields.

Compatibility is capability-based, so a new release is not rejected merely for
having a new version. The candidate must pass our real integration tests before
the updater promotes its source commit.
"""

import inspect
import os
import re
from collections.abc import Mapping
from dataclasses import dataclass
from importlib.metadata import version
from pathlib import Path
from typing import Any

from tradingbot.contracts import PortfolioSnapshot

ADAPTER_CONTRACT = "tradingagents-research-v1"
MODEL_FIELDS = frozenset(
    {
        "llm_provider",
        "quick_think_llm",
        "deep_think_llm",
        "quick_think_provider",
        "deep_think_provider",
        "backend_url",
        "quick_think_backend_url",
        "deep_think_backend_url",
    }
)
RESEARCH_POLICY = {
    "memory_log_max_entries": None,
    "checkpoint_enabled": False,
    "max_debate_rounds": 1,
    "max_risk_discuss_rounds": 1,
    "max_tool_rounds": 3,
    "max_recur_limit": 100,
    "llm_max_retries": 0,
    "max_tokens": 1500,
    "news_article_limit": 10,
}
REPORT_FIELDS = (
    "market_report",
    "fundamentals_report",
    "news_report",
    "sentiment_report",
    "trader_investment_plan",
    "final_trade_decision",
)


class UpstreamCompatibilityError(RuntimeError):
    """The upstream interface changed; update this adapter before running research."""


def _require_call(target, label: str, *args, **kwargs) -> None:
    try:
        inspect.signature(target).bind(*args, **kwargs)
    except (TypeError, ValueError) as exc:
        raise UpstreamCompatibilityError(
            f"TradingAgents {label} does not support adapter {ADAPTER_CONTRACT}"
        ) from exc


def check_compatibility() -> dict[str, Any]:
    """Check actual installed exports without constructing clients or calling APIs."""
    try:
        from tradingagents.default_config import build_default_config
        from tradingagents.graph.trading_graph import TradingAgentsGraph
        from tradingagents.llm_clients import create_tier_client
        from tradingagents.portfolio import PortfolioContext, Position
    except (ImportError, AttributeError) as exc:
        raise UpstreamCompatibilityError("TradingAgents required exports are unavailable") from exc
    _require_call(
        TradingAgentsGraph,
        "graph constructor/config/callbacks",
        config={},
        callbacks=[],
        selected_analysts=(),
    )
    _require_call(
        getattr(TradingAgentsGraph, "propagate", None),
        "propagate/portfolio",
        None,
        "TSLA",
        "2026-10-04",
        portfolio=None,
    )
    _require_call(create_tier_client, "tier client/config/callbacks", {}, "deep", callbacks=[])
    _require_call(build_default_config, "default config")
    for model, required in (
        (PortfolioContext, {"cash", "currency", "positions"}),
        (Position, {"ticker", "quantity", "average_price"}),
    ):
        if not required.issubset(getattr(model, "model_fields", {})):
            raise UpstreamCompatibilityError(f"TradingAgents {model.__name__} fields changed")
    config = build_default_config()
    required_config = set(RESEARCH_POLICY) | {
        "llm_provider",
        "quick_think_llm",
        "deep_think_llm",
        "results_dir",
        "data_cache_dir",
        "memory_log_path",
    }
    if not isinstance(config, dict) or not required_config.issubset(config):
        raise UpstreamCompatibilityError("TradingAgents required configuration fields changed")
    return {
        "compatible": True,
        "adapter_contract": ADAPTER_CONTRACT,
        "version": version("tradingagents"),
    }


def research_config(runtime: Path, overrides: dict | None = None) -> dict:
    check_compatibility()
    from tradingagents.default_config import build_default_config

    if overrides is not None and (not isinstance(overrides, dict) or set(overrides) - MODEL_FIELDS):
        raise ValueError("config may override only model/provider/endpoint fields")
    config = build_default_config()
    config.update(overrides or {})
    config.update(RESEARCH_POLICY)
    config.update(
        results_dir=str(runtime / "reports"),
        data_cache_dir=str(runtime / "cache"),
        memory_log_path=str(runtime / "memory.md"),
    )
    return config


def usage_model_identities(config: dict) -> dict[tuple[str, str], tuple[str, str]]:
    """Attribute callbacks only to configured native-provider model names."""
    base_provider = config["llm_provider"].lower()
    model_patterns = {
        "openai": r"(?:gpt-|chatgpt-|o[134]-?)[A-Za-z0-9_.-]{1,180}",
        "anthropic": r"claude-[A-Za-z0-9_.-]{1,180}",
        "google": r"gemini-[A-Za-z0-9_.-]{1,180}",
    }
    endpoint_environment = {
        "openai": ("OPENAI_BASE_URL",),
        "anthropic": ("ANTHROPIC_BASE_URL", "ANTHROPIC_PROFILE"),
        "google": ("GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL"),
    }
    identities = {}
    ambiguous = set()
    for tier in ("quick", "deep"):
        provider = (config.get(f"{tier}_think_provider") or base_provider).lower()
        model = config[f"{tier}_think_llm"]
        if not isinstance(model, str):
            continue
        if provider == "google":
            model = model.removeprefix("models/")
        pattern = model_patterns.get(provider)
        endpoint = config.get(f"{tier}_think_backend_url")
        if provider == base_provider:
            endpoint = endpoint or config.get("backend_url")
        if (
            pattern is None
            or not re.fullmatch(pattern, model)
            or endpoint
            or any(os.getenv(name) for name in endpoint_environment[provider])
        ):
            # An OpenAI-compatible client may expose the same callback name
            # as another native tier sharing this model name.
            ambiguous.add(model)
            continue
        integration = "google_genai" if provider == "google" else provider
        identities[integration, model] = provider, model
    return {identity: model for identity, model in identities.items() if model[1] not in ambiguous}


class TradingAgentsAdapter:
    def __init__(self, graph):
        _require_call(
            getattr(graph, "propagate", None),
            "propagate/portfolio",
            "TSLA",
            "2026-10-04",
            portfolio=None,
        )
        self.graph = graph

    @staticmethod
    def portfolio_context(portfolio: PortfolioSnapshot):
        from tradingagents.portfolio import PortfolioContext

        return PortfolioContext(
            cash=float(portfolio.available_cash_usd),
            currency="USD",
            positions=[
                dict(ticker=p.symbol, quantity=float(p.quantity)) for p in portfolio.positions
            ],
        )

    def collect_reports(
        self, symbols: list[str], trade_date: str, portfolio: PortfolioSnapshot
    ) -> dict[str, dict[str, str]]:
        if trade_date != portfolio.as_of.date().isoformat():
            raise ValueError("analysis date must match the portfolio snapshot date")
        if not symbols or len(set(symbols)) != len(symbols):
            raise ValueError("symbols must be nonempty and unique")
        context = self.portfolio_context(portfolio)
        reports = {}
        for symbol in symbols:
            result = self.graph.propagate(symbol, trade_date, portfolio=context)
            if (
                not isinstance(result, tuple)
                or len(result) != 2
                or not isinstance(result[0], Mapping)
            ):
                raise UpstreamCompatibilityError("TradingAgents propagate result shape changed")
            state, _ = result
            if state.get("final_rating") not in {
                "Buy",
                "Overweight",
                "Hold",
                "Underweight",
                "Sell",
            }:
                raise ValueError(f"{symbol}: missing or unrecognized upstream rating")
            reports[symbol] = {
                field: state[field]
                for field in REPORT_FIELDS
                if isinstance(state.get(field), str) and state[field].strip()
            }
            if not reports[symbol].get("final_trade_decision"):
                raise ValueError(f"{symbol}: missing upstream decision evidence")
        return reports


@dataclass(frozen=True)
class TradingAgentsRuntime:
    adapter: TradingAgentsAdapter
    config: dict
    callbacks: list

    def decision_model(self):
        return create_portfolio_model(self.config, callbacks=self.callbacks)


def create_portfolio_model(config: dict, *, callbacks: list):
    from tradingagents.llm_clients import create_tier_client

    return create_tier_client(config, "deep", callbacks=callbacks).get_llm()


def create_runtime(
    runtime: Path, overrides: dict | None = None, *, callbacks: list
) -> TradingAgentsRuntime:
    config = research_config(runtime, overrides)
    from tradingagents.graph.trading_graph import TradingAgentsGraph

    graph = TradingAgentsGraph(
        selected_analysts=("market", "fundamentals", "news"), config=config, callbacks=callbacks
    )
    return TradingAgentsRuntime(TradingAgentsAdapter(graph), config, callbacks)
