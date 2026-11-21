import hashlib
import json
from datetime import UTC, datetime
from types import SimpleNamespace
from uuid import uuid4

import pytest
from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel
from langchain_core.messages import AIMessage
from langchain_core.runnables import RunnableLambda
from pydantic import Field

from tradingbot.cli import main
from tradingbot.contracts import DecisionBatch, IntentProposals, PortfolioSnapshot
from tradingbot.llm_usage import LLMUsageReport
from tradingbot.prepared_analysis import load_prepared_reports
from tradingbot.snapshots import load_snapshot


def observation(
    kind,
    ref,
    payload,
    *,
    symbol="NVDA",
    revision=1,
    event="12:00",
    public="12:01",
    received="12:02",
):
    return {
        "kind": kind,
        "provider": "fixture",
        "record_id": ref,
        "symbol": symbol,
        "revision": revision,
        "event_time": f"2026-11-16T{event}:00Z",
        "available_at": f"2026-11-16T{public}:00Z",
        "ingested_at": f"2026-11-16T{received}:00Z",
        "payload": payload,
    }


def save_archive(path, records):
    raw = json.dumps(
        {"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": records}
    ).encode()
    path.write_bytes(raw)
    return hashlib.sha256(raw).hexdigest()


@pytest.fixture
def evidence(tmp_path):
    portfolio = PortfolioSnapshot(
        snapshot_id=uuid4(), as_of=datetime(2026, 11, 16, 14, 30, tzinfo=UTC), cash_usd="1000"
    )
    book = tmp_path / "portfolio.json"
    book.write_text(portfolio.model_dump_json(), encoding="utf-8")
    archive = tmp_path / "market.json"
    records = [
        observation("bar", "known-bar", {"close": "102.125", "currency": "USD", "volume": 5048}),
        observation("filing", "earnings", {"revenue_usd": "100.50"}),
        observation("news", "known-news", {"headline": "Available issuer announcement"}),
        observation("news", "portfolio-news", {"headline": "Other company"}, symbol="MSFT"),
        observation("news", "general-news", {"headline": "General market context"}, symbol=None),
        observation(
            "news",
            "future-event",
            {"headline": "Future issuer announcement"},
            event="14:35",
            public="14:36",
            received="14:37",
        ),
        observation(
            "filing",
            "earnings",
            {"revenue_usd": "199.99"},
            revision=2,
            event="14:00",
            public="14:25",
            received="14:31",
        ),
    ]
    digest = save_archive(archive, records)
    return portfolio, book, archive, digest, records


def test_prepared_snapshot_excludes_future_and_not_yet_ingested_data(evidence):
    portfolio, _, archive, digest, _ = evidence
    snapshot, reports = load_prepared_reports(archive, digest, portfolio, ["NVDA"])

    assert snapshot.as_of == portfolio.as_of
    assert snapshot.symbols == ("NVDA",)
    assert all(record.ingested_at <= portfolio.as_of for record in snapshot.records)
    assert set(reports) == {"NVDA"}
    grouped = {field: json.loads(report)["records"] for field, report in reports["NVDA"].items()}
    assert set(grouped) == {"market_report", "fundamentals_report", "news_report"}
    assert [record["record_id"] for record in grouped["market_report"]] == ["known-bar"]
    assert [record["revision"] for record in grouped["fundamentals_report"]] == [1]
    assert [record["record_id"] for record in grouped["news_report"]] == ["known-news"]
    assert grouped["market_report"][0]["payload"]["close"] == "102.125"
    assert grouped["fundamentals_report"][0]["payload"]["revenue_usd"] == "100.50"


def test_prepared_snapshot_requires_issuer_news_available_at_cutoff(evidence):
    portfolio, _, archive, _, records = evidence
    digest = save_archive(
        archive, [record for record in records if record["record_id"] != "known-news"]
    )
    with pytest.raises(ValueError, match="NVDA.*news"):
        load_prepared_reports(archive, digest, portfolio, ["NVDA"])


class PreparedModel(FakeMessagesListChatModel):
    inputs: list[dict] = Field(default_factory=list)

    def with_structured_output(self, schema, **kwargs):
        assert schema is IntentProposals
        return self | RunnableLambda(lambda message: schema.model_validate_json(message.content))

    def _generate(self, messages, stop=None, run_manager=None, **kwargs):
        self.inputs.append(json.loads(messages[-1].content))
        return super()._generate(messages, stop=stop, run_manager=run_manager, **kwargs)


def analyze_args(book, output):
    return [
        "analyze",
        "--portfolio",
        str(book),
        "--symbols",
        "NVDA",
        "--max-calls",
        "1",
        "--output",
        str(output),
    ]


def test_analyze_prepared_saves_pinned_evidence_and_one_strict_decision(
    evidence, tmp_path, monkeypatch
):
    from tradingagents import llm_clients
    from tradingagents.graph import trading_graph

    portfolio, book, archive, digest, _ = evidence
    output = tmp_path / "run"
    usage_path = tmp_path / "usage.json"
    model = PreparedModel(
        responses=[
            AIMessage(
                content=json.dumps(
                    {
                        "intents": [
                            {
                                "symbol": "NVDA",
                                "action": "BUY",
                                "notional_usd": "100",
                                "confidence": 0.6,
                                "expected_return_pct": 2.5,
                                "horizon": {"min_hours": 24, "max_hours": 168},
                                "thesis": ["Review available prepared evidence"],
                                "invalidation_conditions": ["Issuer retracts the announcement"],
                                "evidence_ids": [
                                    "NVDA:market_report",
                                    "NVDA:fundamentals_report",
                                    "NVDA:news_report",
                                ],
                                "max_execution_cost_bps": 30,
                            }
                        ]
                    }
                )
            )
        ]
    )
    tiers = []

    def make_model(config, tier, *, callbacks):
        tiers.append(tier)
        model.callbacks = callbacks
        return SimpleNamespace(get_llm=lambda: model)

    def unexpected_graph_client(*args, **kwargs):
        pytest.fail("prepared analysis constructed a network-enabled graph model")

    monkeypatch.setattr(llm_clients, "create_tier_client", make_model)
    monkeypatch.setattr(trading_graph, "create_tier_client", unexpected_graph_client)
    assert (
        main(
            analyze_args(book, output)
            + [
                "--data-archive",
                str(archive),
                "--data-sha256",
                digest,
                "--usage-output",
                str(usage_path),
            ]
        )
        == 0
    )

    assert tiers == ["deep"]
    assert len(model.inputs) == 1
    reports = json.loads((output / "reports.json").read_text(encoding="utf-8"))
    assert model.inputs[0]["reports"] == reports
    assert set(reports["NVDA"]) == {"market_report", "fundamentals_report", "news_report"}
    assert "known-bar" in reports["NVDA"]["market_report"]
    assert "199.99" not in reports["NVDA"]["fundamentals_report"]
    assert "future-event" not in reports["NVDA"]["news_report"]
    batch = DecisionBatch.model_validate_json((output / "intents.json").read_bytes())
    assert batch.execution_mode == "research"
    assert batch.portfolio_snapshot_id == portfolio.snapshot_id
    assert len(batch.intents) == 1
    assert batch.intents[0].action == "BUY"
    assert batch.evidence_hashes == {
        f"NVDA:{group}": hashlib.sha256(report.encode()).hexdigest()
        for group, report in reports["NVDA"].items()
    }
    assert set(batch.intents[0].evidence_ids) == set(batch.evidence_hashes)

    metadata = json.loads((output / "metadata.json").read_text(encoding="utf-8"))
    assert metadata["source_mode"] == "prepared_archive_research"
    assert metadata["archive_sha256"] == digest
    assert metadata["archive_source_mode"] == "synthetic_fixture"
    snapshot = load_snapshot(
        archive.parent,
        {"ref": archive.name, "sha256": digest},
        portfolio.as_of,
        symbols=["NVDA"],
        visibility="locally_ingested",
    )
    assert metadata["snapshot_sha256"] == snapshot.sha256
    assert metadata["llm_calls"] == 1
    assert metadata["alpha_measured"] is False
    usage = LLMUsageReport.model_validate_json(usage_path.read_bytes())
    assert usage.capture_status == "complete"
    assert usage.llm_calls == 1
    assert [(call.role, call.status) for call in usage.calls] == [
        ("portfolio_allocator", "completed")
    ]


@pytest.mark.parametrize("options", ["wrong_hash", "archive_only", "hash_only"])
def test_analyze_rejects_invalid_archive_before_budget_or_model(
    evidence, tmp_path, monkeypatch, options
):
    from tradingagents import llm_clients
    from tradingagents.graph import trading_graph

    from tradingbot import budget

    _, book, archive, digest, _ = evidence
    output = tmp_path / "run"
    usage_path = tmp_path / "usage.json"

    def unexpected(*args, **kwargs):
        pytest.fail("invalid archive reached the budget or model boundary")

    monkeypatch.setattr(budget, "CallBudget", unexpected)
    monkeypatch.setattr(llm_clients, "create_tier_client", unexpected)
    monkeypatch.setattr(trading_graph, "create_tier_client", unexpected)
    additional = {
        "wrong_hash": ["--data-archive", str(archive), "--data-sha256", "0" * 64],
        "archive_only": ["--data-archive", str(archive)],
        "hash_only": ["--data-sha256", digest],
    }
    assert (
        main(analyze_args(book, output) + additional[options] + ["--usage-output", str(usage_path)])
        == 2
    )
    assert not output.exists()
    assert not usage_path.exists()
