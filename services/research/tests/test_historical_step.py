import hashlib
import io
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from tradingbot.adapters import offline
from tradingbot.budget import CallBudgetExceeded
from tradingbot.cli import main
from tradingbot.historical_step import MAX_STEP_BYTES, run_historical_step
from tradingbot.llm_usage import LLMUsageReport
from tradingbot.selection import plan_research

ROOT = Path(__file__).resolve().parents[3]


@pytest.fixture
def step(tmp_path):
    document = json.loads((ROOT / "contracts/valid-research-historical-step.json").read_bytes())
    document["completed_at"] = "2020-01-02T16:01:00.123456Z"
    document["job"]["portfolio"]["as_of"] = "2020-01-02T16:00:00Z"
    archive = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "records": [
            {
                "kind": "bar",
                "provider": "historical-fixture",
                "record_id": record_id,
                "symbol": "TSLA",
                "revision": 1,
                "event_time": "2020-01-02T15:30:00Z",
                "available_at": "2020-01-02T15:30:00Z",
                "ingested_at": ingested_at,
                "payload": {"close": price},
            }
            for record_id, price, ingested_at in (
                ("known", "95.12345678", "2020-01-02T15:31:00Z"),
                ("late", "888.12345678", "2020-01-02T16:00:30Z"),
            )
        ],
    }
    write_archive(document, archive, tmp_path)
    return document, archive


def write_archive(document, archive, root):
    raw = json.dumps(archive).encode()
    (root / "market.json").write_bytes(raw)
    document["job"]["data_manifest"] = {
        "ref": "market.json",
        "sha256": hashlib.sha256(raw).hexdigest(),
    }


def test_historical_step_runs_the_scripted_graph_with_a_pinned_clock(step, tmp_path):
    document, _ = step
    encoded = json.dumps(document).encode()
    result = run_historical_step(encoded, tmp_path)
    assert result == run_historical_step(encoded, tmp_path)
    assert result["result_schema_version"] == "0.2"
    assert result["batch"]["created_at"] == document["completed_at"]
    clock = datetime.fromisoformat(document["completed_at"])
    assert datetime.fromisoformat(result["batch"]["expires_at"]) == clock + timedelta(minutes=30)
    assert result["batch"]["intents"][0]["action"] == "HOLD"
    assert result["batch"]["portfolio_snapshot_id"] == document["job"]["portfolio"]["snapshot_id"]
    assert "95.12345678" in result["reports"]["TSLA"]["market_report"]
    assert "888.12345678" not in json.dumps(result["reports"])
    metadata = result["metadata"]
    assert metadata["source_mode"] == "tradingagents_offline"
    assert metadata["alpha_measured"] is False and metadata["intraday_pit_verified"] is False
    assert 1 < metadata["llm_calls"] <= document["job"]["max_llm_calls"]
    assert metadata["provenance"]["job_id"] == document["job"]["job_id"]


def test_historical_identity_cli_preserves_historical_projection(monkeypatch, capsys):
    archive = ROOT / "services/research/examples/selection"
    selection = json.loads((archive / "identity-request.json").read_bytes())
    registry = selection["identity_registry"]
    screened = plan_research(archive, selection, trusted_identity_registry=registry)
    request = screened["job"]
    completion = datetime.fromisoformat(request["portfolio"]["as_of"]) + timedelta(minutes=1)
    encoded = json.dumps({"job": request, "completed_at": completion.isoformat()}).encode()
    monkeypatch.setattr("tradingbot.cli.sys.stdin", io.TextIOWrapper(io.BytesIO(encoded)))
    assert main([
        "historical-step", "--archive-root", str(archive),
        "--identity-registry-ref", registry["ref"], "--identity-registry-sha256", registry["sha256"],
    ]) == 0
    result = json.loads(capsys.readouterr().out)
    assert result == run_historical_step(encoded, archive, owner_identity_registry=registry)
    assert result["result_schema_version"] == "0.3"
    assert datetime.fromisoformat(result["batch"]["created_at"]) == completion
    assert result["metadata"]["provenance"]["projected_snapshot_sha256"] == screened["snapshot_sha256"]
    assert result["metadata"]["provenance"]["security_bindings"] == request["security_bindings"]
    assert "fixture-previous" not in json.dumps(result["reports"])
    assert all(intent["action"] == "HOLD" for intent in result["batch"]["intents"])


def test_historical_identity_validates_owner_before_starting_the_graph(monkeypatch):
    archive = ROOT / "services/research/examples/selection"
    selection = json.loads((archive / "identity-request.json").read_bytes())
    registry = selection["identity_registry"]
    request = plan_research(archive, selection, trusted_identity_registry=registry)["job"]
    encoded = json.dumps({
        "job": request, "completed_at": datetime.fromisoformat(request["portfolio"]["as_of"]).isoformat(),
    }).encode()

    def graph_must_not_start(*args, **kwargs):
        pytest.fail("unbound historical input reached the research graph")

    monkeypatch.setattr(offline, "_verified_upstream", graph_must_not_start)
    for pin in (None, registry | {"sha256": "e" * 64}):
        with pytest.raises(ValueError, match="owner-approved identity registry"):
            run_historical_step(encoded, archive, owner_identity_registry=pin)


def test_budget_is_enforced_by_the_graph(step, tmp_path):
    document, _ = step
    document["job"]["max_llm_calls"] = 1
    with pytest.raises(CallBudgetExceeded):
        run_historical_step(json.dumps(document).encode(), tmp_path)


def test_historical_capture_preserves_graph_result_and_attributes_actual_callbacks(
    step, tmp_path, monkeypatch, capsys
):
    document, _ = step
    encoded = json.dumps(document).encode()
    destination = tmp_path / "usage.json"
    monkeypatch.setattr("tradingbot.cli.sys.stdin", io.TextIOWrapper(io.BytesIO(encoded)))
    assert (
        main(
            ["historical-step", "--archive-root", str(tmp_path), "--usage-output", str(destination)]
        )
        == 0
    )
    result = json.loads(capsys.readouterr().out)
    assert result == run_historical_step(encoded, tmp_path)
    report = LLMUsageReport.model_validate_json(destination.read_bytes())
    assert report.llm_calls == result["metadata"]["llm_calls"]
    assert report.capture_status == "complete"
    assert report.source_mode == "synthetic_fixture"
    assert [call.role for call in report.calls].count("portfolio_allocator") == 1
    assert {call.role for call in report.calls} == {"research", "portfolio_allocator"}
    assert {(call.provider, call.model) for call in report.calls} == {
        ("offline", "scripted-offline-v1")
    }
    assert all(call.usage_source == "unavailable" for call in report.calls)
    assert all(
        call.started_at >= datetime.fromisoformat(document["completed_at"]) for call in report.calls
    )


def test_historical_budget_failure_keeps_partial_call_sidecar(step, tmp_path):
    document, _ = step
    document["job"]["max_llm_calls"] = 1
    destination = tmp_path / "partial.json"
    with pytest.raises(CallBudgetExceeded):
        run_historical_step(json.dumps(document).encode(), tmp_path, usage_output=destination)
    report = LLMUsageReport.model_validate_json(destination.read_bytes())
    assert report.capture_status == "partial"
    assert report.llm_calls == 1


def test_ablation_keeps_the_archive_and_changes_only_selected_graph_inputs(
    step, tmp_path, monkeypatch
):
    document, archive = step
    archive["schema_version"] = "0.2"
    account_id = "00000000-0000-0000-0000-000000000001"
    event_id = "00000000-0000-0000-0000-000000000008"
    archive["records"].append(
        {
            "kind": "paper_outcome",
            "provider": "paper-ledger",
            "record_id": f"{account_id}-{event_id}",
            "symbol": "TSLA",
            "revision": 1,
            "event_time": "2020-01-02T15:25:00Z",
            "available_at": "2020-01-02T15:29:00Z",
            "ingested_at": "2020-01-02T15:29:00Z",
            "payload": {
                "schema_version": "0.1",
                "source_mode": "paper_simulated_ledger",
                "accounting_method": "fifo_recording_order_usd_scale8_v1",
                "account_id": account_id,
                "account_version": 8,
                "fill_event_id": event_id,
                "decision_id": "00000000-0000-0000-0000-000000000009",
                "policy_version": document["job"]["policy_version"],
                "risk_group": "tech",
                "instrument": {
                    "instrument_id": "fixture:TSLA",
                    "registry_version": "fixture-v1",
                    "decimals": 2,
                },
                "quantity_raw": "100",
                "gross_usd": "99",
                "fee_usd": "0.25",
                "cost_basis_usd": "100",
                "net_pnl_usd": "-1.25",
            },
        }
    )
    write_archive(document, archive, tmp_path)

    manager_prompts = []
    model_call = offline.ScriptedModel._call

    def capture_manager_prompt(self, messages, *args, **kwargs):
        manager_prompts.extend(
            message.content
            for message in messages
            if isinstance(message.content, str)
            and message.content.startswith("As the Portfolio Manager,")
        )
        return model_call(self, messages, *args, **kwargs)

    monkeypatch.setattr(offline.ScriptedModel, "_call", capture_manager_prompt)
    results = {
        variant: run_historical_step(json.dumps(document | {"variant": variant}).encode(), tmp_path)
        for variant in ("full_graph", "no_memory", "single_analyst", "factual_reflection")
    }
    default = run_historical_step(json.dumps(document).encode(), tmp_path)
    assert default == results["full_graph"]
    assert {
        item["metadata"]["provenance"]["data_manifest"]["sha256"] for item in results.values()
    } == {document["job"]["data_manifest"]["sha256"]}
    assert len({item["metadata"]["provenance"]["config_sha256"] for item in results.values()}) == 4
    assert len({item["batch"]["run_id"] for item in results.values()}) == 4
    assert len({item["reports"]["TSLA"]["market_report"] for item in results.values()}) == 1
    assert [item["metadata"]["llm_calls"] for item in results.values()] == [12, 12, 10, 12]
    assert len(manager_prompts) == 5
    assert manager_prompts[-1] == manager_prompts[0]
    for (variant, item), prompt in zip(results.items(), manager_prompts[:-1], strict=True):
        reports = item["reports"]["TSLA"]
        assert item["batch"]["intents"][0]["action"] == "HOLD"
        assert ("news_report" in reports) == (variant != "single_analyst")
        assert ("fundamentals_report" in reports) == (variant != "single_analyst")
        assert (event_id in reports["final_trade_decision"]) == (variant != "no_memory")
        assert ('"factual_reflection":' in reports["final_trade_decision"]) == (
            variant == "factual_reflection"
        )
        assert (event_id in prompt) == (variant != "no_memory")
        assert ('"factual_reflection":' in prompt) == (variant == "factual_reflection")


@pytest.mark.parametrize(
    "scenario",
    [
        "future",
        "before_cutoff",
        "archive_source",
        "unknown_field",
        "unknown_variant",
        "nanoseconds",
        "cash_number",
        "missing_cash_asset",
        "snapshot_nanoseconds",
    ],
)
def test_rejects_unusable_input_before_graph_start(step, tmp_path, monkeypatch, scenario):
    document, archive = step

    def forbidden():
        pytest.fail("invalid input reached the graph")

    monkeypatch.setattr(offline, "_verified_upstream", forbidden)
    if scenario == "future":
        document["completed_at"] = (datetime.now(UTC) + timedelta(days=1)).isoformat()
    elif scenario == "before_cutoff":
        document["completed_at"] = "2020-01-02T15:59:00Z"
    elif scenario == "archive_source":
        archive["source_mode"] = "local_archive"
        write_archive(document, archive, tmp_path)
    elif scenario == "unknown_field":
        document["attempt"] = 2
    elif scenario == "unknown_variant":
        document["variant"] = "custom_graph"
    elif scenario == "nanoseconds":
        document["completed_at"] = "2020-01-02T16:01:00.123456789Z"
    elif scenario == "cash_number":
        document["job"]["portfolio"]["cash_usd"] = 900
    elif scenario == "missing_cash_asset":
        del document["job"]["portfolio"]["cash_asset"]
    else:
        document["job"]["portfolio"]["as_of"] = "2020-01-02T16:00:00.123456789Z"
    with pytest.raises(ValueError):
        run_historical_step(json.dumps(document).encode(), tmp_path)


def test_cli_reads_stdout_as_one_result_and_rejects_unbounded_input(
    step,
    tmp_path,
    monkeypatch,
    capsys,
):
    document, _ = step
    argv = ["historical-step", "--archive-root", str(tmp_path)]
    monkeypatch.setattr("sys.stdin", io.TextIOWrapper(io.BytesIO(json.dumps(document).encode())))
    assert main(argv) == 0
    captured = capsys.readouterr()
    assert json.loads(captured.out)["batch"]["created_at"] == document["completed_at"]
    monkeypatch.setattr("sys.stdin", io.TextIOWrapper(io.BytesIO(b" " * (MAX_STEP_BYTES + 1))))
    assert main(argv) == 2
    assert not capsys.readouterr().out
