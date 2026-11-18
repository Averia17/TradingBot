import hashlib
import json
from datetime import UTC, datetime, timedelta
from importlib.metadata import version
from pathlib import Path
from threading import Event
from uuid import uuid4

import pytest

from tradingbot.adapters import offline
from tradingbot.adapters.offline import OfflineAnalysisCancelled, ScriptedModel, analyze_offline
from tradingbot.budget import CallBudgetExceeded
from tradingbot.contracts import DecisionBatch
from tradingbot.snapshots import load_snapshot
from tradingbot.upstream import _git, _verify_repo


@pytest.fixture(autouse=True)
def upstream_pin(monkeypatch):
    root = Path(__file__).resolve().parents[1]
    lock = json.loads((root / "upstream.lock.json").read_text(encoding="utf-8"))
    pin = next(repo for repo in lock["repositories"] if repo["name"] == "trading-agents")
    if not (root / ".upstream-update.lock").is_file():
        return pin
    checkout = root / "vendor" / pin["name"]
    head = _git(checkout, "rev-parse", "HEAD")
    if head == pin["source_commit"]:
        return pin
    # The updater runs compatibility tests at the fetched candidate before
    # publishing its pin. Exercise the same graph and checkout verification.
    assert head == _git(checkout, "rev-parse", "FETCH_HEAD^{commit}")
    pin = pin | {"ref": f"v{version('tradingagents')}", "source_commit": head}
    _verify_repo(root, pin)
    monkeypatch.setattr(offline, "verify", lambda *args, **kwargs: [pin])
    return pin


def job(max_llm_calls=64):
    as_of = datetime.now(UTC) - timedelta(days=1)
    return {
        "job_schema_version": "0.1",
        "job_id": str(uuid4()),
        "portfolio": {
            "snapshot_id": str(uuid4()),
            "as_of": as_of.isoformat(),
            "cash_usd": "1000",
            "reserved_cash_usd": "150",
            "positions": [
                {
                    "symbol": "TSLA",
                    "instrument_id": "synthetic-tsla",
                    "quantity": "1.25",
                    "mark_price_usd": "100",
                }
            ],
        },
        "requested_symbols": ["TSLA", "NVDA"],
        "data_manifest": {"ref": "offline-test", "sha256": "a" * 64},
        "policy_version": "offline-v1",
        "max_llm_calls": max_llm_calls,
    }


def test_offline_graph_produces_bound_portfolio_result_with_observed_usage(upstream_pin, monkeypatch):
    request = job()
    deadline = datetime.now(UTC) + timedelta(minutes=5)
    authorized, invoked = set(), set()
    original_call = ScriptedModel._call

    def before_model(self, messages, stop=None, run_manager=None, **kwargs):
        assert run_manager.run_id in authorized and run_manager.run_id not in invoked
        invoked.add(run_manager.run_id)
        return original_call(self, messages, stop=stop, run_manager=run_manager, **kwargs)

    monkeypatch.setattr(ScriptedModel, "_call", before_model)
    result = analyze_offline(request, 2, deadline, Event(), authorize_call=authorized.add)

    assert result["result_schema_version"] == "0.2"
    batch = DecisionBatch.model_validate(result["batch"])
    assert str(batch.portfolio_snapshot_id) == request["portfolio"]["snapshot_id"]
    assert batch.expires_at == deadline
    assert {entry.symbol for entry in batch.intents} == {"TSLA", "NVDA"}
    assert all(entry.action == "HOLD" and entry.notional_usd == 0 for entry in batch.intents)
    assert all(
        set(report) >= {"market_report", "news_report", "fundamentals_report", "final_trade_decision"}
        for report in result["reports"].values()
    )

    metadata = result["metadata"]
    assert metadata["source_mode"] == "tradingagents_offline"
    assert not metadata["alpha_measured"] and not metadata["intraday_pit_verified"]
    assert 2 < metadata["llm_calls"] <= request["max_llm_calls"]
    assert authorized == invoked and len(authorized) == metadata["llm_calls"]
    assert metadata["missing_usage_calls"] == metadata["llm_calls"]
    assert metadata["input_tokens"] == metadata["output_tokens"] == 0
    assert metadata["usd_cost"] is None
    provenance = metadata["provenance"]
    assert provenance["job_id"] == request["job_id"]
    assert provenance["attempt"] == 2
    assert provenance["policy_version"] == request["policy_version"]
    assert provenance["data_manifest"] == request["data_manifest"]
    assert provenance["upstream_version"] == upstream_pin["ref"].removeprefix("v")
    assert provenance["upstream_sha"] == upstream_pin["source_commit"]
    assert len(provenance["config_sha256"]) == 64
    assert provenance["model_id"] == "scripted-offline-v1"


def test_offline_model_cannot_reuse_a_permit_for_same_run_id():
    authorized = []
    run_id = uuid4()
    budget = offline.OfflineBudget(2, lambda: None, authorized.append)
    budget.on_chat_model_start({}, [], run_id=run_id)
    with pytest.raises(RuntimeError, match="model run ID already authorized"):
        budget.on_chat_model_start({}, [], run_id=run_id)
    with pytest.raises(OfflineAnalysisCancelled):
        budget.on_chat_model_start({}, [], run_id=uuid4())
    assert authorized == [run_id]


def test_offline_graph_cannot_complete_an_attempt_without_call_budget():
    with pytest.raises(CallBudgetExceeded):
        analyze_offline(job(max_llm_calls=1), 1, datetime.now(UTC) + timedelta(minutes=5), Event())


def test_offline_graph_does_not_start_after_cancellation():
    cancelled = Event()
    cancelled.set()
    with pytest.raises(OfflineAnalysisCancelled):
        analyze_offline(job(), 1, datetime.now(UTC) + timedelta(minutes=5), cancelled)


def test_offline_graph_stops_when_canceled_during_analysis(monkeypatch):
    cancelled = Event()
    call = ScriptedModel._call

    def cancel_after_first_response(self, *args, **kwargs):
        response = call(self, *args, **kwargs)
        cancelled.set()
        return response

    monkeypatch.setattr(ScriptedModel, "_call", cancel_after_first_response)
    with pytest.raises(OfflineAnalysisCancelled):
        analyze_offline(job(), 1, datetime.now(UTC) + timedelta(minutes=5), cancelled)


def archive_job(tmp_path, records):
    request = job()
    request["requested_symbols"] = ["TSLA"]
    raw = json.dumps(
        {"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": records}
    ).encode()
    (tmp_path / "market.json").write_bytes(raw)
    request["data_manifest"] = {"ref": "market.json", "sha256": hashlib.sha256(raw).hexdigest()}
    return request


def archived_bar(record_id, value, *, at="2020-01-02T16:00:00Z", symbol="TSLA"):
    return {
        "kind": "bar",
        "provider": "archive-fixture",
        "record_id": record_id,
        "symbol": symbol,
        "revision": 1,
        "event_time": at,
        "available_at": at,
        "ingested_at": at,
        "payload": {"close": value},
    }


def test_verified_archive_observations_reach_actual_graph_prompts_and_evidence(tmp_path, monkeypatch):
    request = archive_job(
        tmp_path,
        [
            archived_bar("early", "123.45678901"),
            archived_bar("oversized", "7" * 8000),
            archived_bar("future", "future-value", at="2020-01-03T16:00:00Z"),
            archived_bar("late-receipt", "locally-unseen") | {"ingested_at": "2020-01-03T16:00:00Z"},
            archived_bar("other", "other-symbol-value", symbol="NVDA"),
        ],
    )
    request["portfolio"]["as_of"] = "2020-01-02T16:00:00Z"
    observed_calls = []
    original_call = ScriptedModel._call

    def observe(self, messages, stop=None, run_manager=None, **kwargs):
        observed_calls.append(
            [message.content for message in messages if isinstance(message.content, str)]
        )
        return original_call(self, messages, stop=stop, run_manager=run_manager, **kwargs)

    monkeypatch.setattr(ScriptedModel, "_call", observe)
    result = analyze_offline(
        request, 1, datetime.now(UTC) + timedelta(minutes=5), Event(), archive_root=tmp_path
    )
    market = result["reports"]["TSLA"]["market_report"]
    expected_snapshot = load_snapshot(
        tmp_path,
        request["data_manifest"],
        datetime.fromisoformat(request["portfolio"]["as_of"]),
        symbols=["TSLA"],
        visibility="locally_ingested",
    )
    assert expected_snapshot.sha256 in market
    assert "123.45678901" in market and '"omitted_records":1' in market
    # The final allocation call also sees the persisted evidence. Require the
    # value in at least one earlier call made by the upstream graph itself.
    assert any("123.45678901" in text for call in observed_calls[:-1] for text in call)
    for text in [market, *(text for call in observed_calls for text in call)]:
        assert all(
            value not in text for value in ("future-value", "other-symbol-value", "locally-unseen")
        )
        assert "7" * 8000 not in text
    assert result["batch"]["evidence_hashes"]["TSLA:market_report"] == hashlib.sha256(
        market.encode()
    ).hexdigest()
    assert result["metadata"]["provenance"]["data_manifest"] == request["data_manifest"]
    assert result["metadata"]["intraday_pit_verified"] is False
    assert result["metadata"]["alpha_measured"] is False


@pytest.mark.parametrize("scenario", ["tampered", "missing", "uncovered"])
def test_invalid_archive_stops_before_permit_or_model_call(tmp_path, monkeypatch, scenario):
    request = archive_job(tmp_path, [archived_bar("bar", "12.5", symbol="NVDA")])
    if scenario == "tampered":
        (tmp_path / "market.json").write_bytes(b"{}")
    if scenario == "missing":
        (tmp_path / "market.json").unlink()

    def forbidden(*_args, **_kwargs):
        pytest.fail("archive validation must finish before starting the graph")

    monkeypatch.setattr(offline, "_verified_upstream", forbidden)
    with pytest.raises((OSError, ValueError)):
        analyze_offline(
            request, 1, datetime.now(UTC) + timedelta(minutes=5), Event(),
            archive_root=tmp_path, authorize_call=forbidden,
        )
