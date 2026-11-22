import hashlib
import json
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from pathlib import Path
from threading import Event
from uuid import uuid4

import httpx2
import pytest
from langchain_core.language_models import SimpleChatModel
from langchain_core.messages import AIMessage
from langchain_core.outputs import ChatGeneration, ChatResult
from langchain_core.runnables import RunnableLambda

from tradingbot import worker
from tradingbot.adapters import prepared_provider
from tradingbot.contracts import IntentProposals
from tradingbot.selection import plan_research
from tradingbot.snapshots import ArchiveReference

CONFIG = {"llm_provider": "chatgpt", "deep_think_llm": "gpt-5.4"}
EXAMPLES = Path(__file__).resolve().parents[1] / "examples" / "selection"
PROPOSALS = {"intents": [{
    "symbol": "TSLA",
    "action": "BUY",
    "notional_usd": "100",
    "confidence": 0.8,
    "expected_return_pct": 6.0,
    "horizon": {"min_hours": 24, "max_hours": 48},
    "thesis": ["The observed close and report justify a new allocation."],
    "invalidation_conditions": ["Reassess if the new report disappoints."],
    "evidence_ids": ["TSLA:market_report", "TSLA:fundamentals_report"],
    "max_execution_cost_bps": 100,
}]}


class MockProvider(SimpleChatModel):
    observe_request: Callable

    @property
    def _llm_type(self) -> str:
        return "provider-transport-test"

    def _call(self, messages, stop=None, run_manager=None, **kwargs):
        raise AssertionError("the transport returns structured evidence")

    def _generate(self, messages, stop=None, run_manager=None, **kwargs):
        self.observe_request(json.loads(messages[1].content))
        return ChatResult(generations=[ChatGeneration(message=AIMessage(
            content="typed response fixture",
            usage_metadata={"input_tokens": 80, "output_tokens": 240, "total_tokens": 320},
        ))])

    def with_structured_output(self, schema, **kwargs):
        assert schema is IntentProposals
        return self | RunnableLambda(lambda _: IntentProposals.model_validate(PROPOSALS))


def prepared_job(root, source_mode):
    selection = json.loads((EXAMPLES / "identity-request.json").read_bytes())
    registry = selection["identity_registry"]
    (root / registry["ref"]).write_bytes((EXAMPLES / registry["ref"]).read_bytes())
    manifest = selection["data_manifest"]
    archive = json.loads((EXAMPLES / manifest["ref"]).read_bytes())
    archive["source_mode"] = source_mode
    filing = next(record for record in archive["records"] if record["kind"] == "filing")
    archive["records"].append(filing | {"kind": "news", "record_id": "provider-news"})
    raw = json.dumps(archive).encode()
    (root / manifest["ref"]).write_bytes(raw)
    manifest["sha256"] = hashlib.sha256(raw).hexdigest()
    selection["candidates"] = [candidate for candidate in selection["candidates"] if candidate["symbol"] == "TSLA"]
    job = plan_research(root, selection, trusted_identity_registry=registry)["job"]
    return job | {
        "job_schema_version": "0.3", "analysis_mode": "prepared_provider", "max_llm_calls": 1,
    }, registry


@pytest.mark.parametrize("denial", [None, "permit", "usage", "synthetic"])
def test_opt_in_worker_publishes_real_model_boundary_only_after_durable_permit_and_usage(
    tmp_path, monkeypatch, denial,
):
    request, registry = prepared_job(
        tmp_path, "synthetic_fixture" if denial == "synthetic" else "local_archive"
    )
    stop = Event()
    lease_token = str(uuid4())
    deadline = datetime.now(UTC) + timedelta(seconds=30)
    lease = {
        "job_id": request["job_id"], "worker_id": "provider-test", "status": "leased",
        "lease_token": lease_token, "attempt": 1, "lease_expires_at": deadline.isoformat(),
        "attempt_deadline": deadline.isoformat(),
    }
    events, results = [], []
    permit = None

    def observe_request(prompt):
        assert events == ["claim", "heartbeat", "authorize-call"]
        events.append("model")
        assert prompt["portfolio"] == request["portfolio"]
        records = [
            row for text in prompt["reports"]["TSLA"].values()
            for row in json.loads(text)["records"]
        ]
        assert all(row["payload"]["security_id"] == "fixture-current" for row in records)

    def make_model(config, *, callbacks, timeout):
        assert config["llm_provider"] == "chatgpt"
        assert config["deep_think_llm"] == "gpt-5.4"
        assert config["deep_think_provider"] == "chatgpt"
        assert config["backend_url"] == "https://api.openai.com/v1"
        assert config["deep_think_backend_url"] is None
        assert config["llm_max_retries"] == 0
        assert 0 < timeout <= 30
        return MockProvider(callbacks=callbacks, observe_request=observe_request)

    def post(self, endpoint, body, expected, timeout=None):
        nonlocal permit
        payload = json.loads(body)
        route = endpoint.split("/")[-1]
        if route == "claim" and events:
            stop.set()
            return None
        events.append(route)
        if route == "claim":
            assert payload == {
                "worker_id": "provider-test", "claim_id": payload["claim_id"],
                "identity_registry_sha256": registry["sha256"],
                "analysis_mode": "prepared_provider",
            }
            lease["claim_id"] = payload["claim_id"]
            return lease | {"request": request}
        assert payload["lease_token"] == lease_token
        if route == "heartbeat":
            return lease
        if route == "authorize-call":
            if denial == "permit":
                raise worker.ControlError(status=409)
            permit = {"job_id": request["job_id"], "attempt": 1, "call_id": payload["call_id"]}
            return permit | {"remaining_llm_calls": 0}
        if route == "record-call":
            assert payload["call_usage_schema_version"] == "0.2"
            call = payload["call"]
            assert call["run_id"] == permit["call_id"]
            assert (call["provider"], call["model"], call["role"], call["status"]) == (
                "chatgpt", "gpt-5.4", "portfolio_allocator", "completed",
            )
            assert (call["input_tokens"], call["output_tokens"]) == (80, 240)
            if denial == "usage":
                raise worker.ControlError(status=409)
            return permit | {"recorded_at": datetime.now(UTC).isoformat()}
        assert route == "complete"
        result = {key: val for key, val in payload.items() if key not in {"worker_id", "lease_token"}}
        results.append(result)
        return {
            "job_id": request["job_id"], "status": "completed", "attempt": 1,
            "completed_at": datetime.now(UTC).isoformat(), "result": result,
        }

    monkeypatch.setenv("TRADINGAGENTS_DEEP_THINK_PROVIDER", "anthropic")
    monkeypatch.setenv("TRADINGAGENTS_DEEP_THINK_BACKEND_URL", "https://unused.invalid")
    monkeypatch.setattr(prepared_provider, "_create_model", make_model)
    monkeypatch.setattr(worker.ControlClient, "post", post)
    monkeypatch.setattr(worker.ControlClient, "get", lambda self, path: {
        "research_call_usage": True,
        "research_job_schema_versions": ["0.1", "0.2", "0.3"],
        "research_result_schema_versions": ["0.2", "0.3", "0.4"],
    })
    worker.run_worker(
        "http://127.0.0.1:8080", "provider-test", stop=stop, archive_root=tmp_path,
        identity_registry=registry, provider_config=CONFIG, poll_interval=0.001,
    )
    expected = ["claim", "heartbeat"]
    if denial != "synthetic":
        expected.append("authorize-call")
    if denial in {None, "usage"}:
        expected += ["model", "record-call"]
    if denial is None:
        expected.append("complete")
    assert events == expected
    if denial is not None:
        assert not results
        return

    result, = results
    assert result["result_schema_version"] == "0.4"
    assert result["batch"]["intents"][0]["action"] == "BUY"
    assert datetime.fromisoformat(result["batch"]["expires_at"]) <= deadline
    assert set(result["reports"]["TSLA"]) == {"market_report", "fundamentals_report", "news_report"}
    metadata = result["metadata"]
    assert metadata["source_mode"] == "prepared_archive_research"
    assert (metadata["llm_calls"], metadata["missing_usage_calls"], metadata["usd_cost"]) == (1, 0, None)
    proof = metadata["provenance"]
    assert proof["archive_source_mode"] == "local_archive"
    assert proof["model_provider"] == "chatgpt"
    assert proof["model_id"] == "gpt-5.4"
    assert proof["security_bindings"] == request["security_bindings"]


def test_provider_worker_requires_owner_pins_before_control(monkeypatch):
    def unexpected(*args, **kwargs):
        pytest.fail("provider worker must reject missing owner pins before creating a client")

    monkeypatch.setattr(worker, "ControlClient", unexpected)
    with pytest.raises(ValueError, match="requires an archive root and identity registry"):
        worker.run_worker("http://127.0.0.1:8080", provider_config=CONFIG)


def test_real_chatgpt_sdk_adapter_accepts_one_streamed_structured_response(tmp_path, monkeypatch):
    from tradingagents.llm_clients import chatgpt_auth

    request, registry = prepared_job(tmp_path, "local_archive")
    authorized, receipts, sends = [], [], []
    monkeypatch.setattr(chatgpt_auth, "credential", lambda: lambda: "synthetic-test-token")
    monkeypatch.setenv("TRADINGAGENTS_OPENAI_REASONING_EFFORT", "medium")

    def send(self, outbound, **kwargs):
        assert len(authorized) == 1 and not receipts and not sends
        sends.append(outbound.url)
        assert str(outbound.url) == "https://api.openai.com/v1/responses"
        payload = json.loads(outbound.content)
        assert payload["model"] == CONFIG["deep_think_llm"]
        assert [message["role"] for message in payload["input"]] == ["developer", "user"]
        assert payload["stream"] is True and payload["store"] is False
        assert len(payload["tools"]) == 1
        assert payload["tools"][0]["name"] == "IntentProposals"
        call = {"id": "func_fixture", "type": "function_call", "name": "IntentProposals",
                "call_id": "call_fixture", "arguments": "", "status": "in_progress"}
        arguments = json.dumps(PROPOSALS)
        response = {
            "id": "resp_fixture", "object": "response", "status": "completed",
            "model": payload["model"], "created_at": datetime.now(UTC).timestamp(),
            "error": None, "service_tier": "default",
            "output": [call | {"arguments": arguments, "status": "completed"}],
            "usage": {"input_tokens": 80, "output_tokens": 240, "total_tokens": 320,
                      "input_tokens_details": {"cached_tokens": 0},
                      "output_tokens_details": {"reasoning_tokens": 0}},
        }
        events = [
            {"type": "response.output_item.added", "output_index": 0, "item": call},
            {"type": "response.function_call_arguments.delta", "output_index": 0,
             "item_id": call["id"], "delta": arguments},
            {"type": "response.completed", "response": response},
        ]
        body = "".join(
            f"data: {json.dumps(event | {'sequence_number': index})}\n\n"
            for index, event in enumerate(events)
        ).encode()
        return httpx2.Response(200, request=outbound, headers={"content-type": "text/event-stream"},
                               content=body)

    monkeypatch.setattr(httpx2.Client, "send", send)
    result = prepared_provider.analyze_prepared_provider(
        request, 1, datetime.now(UTC) + timedelta(seconds=30), Event(),
        archive_root=tmp_path, owner_identity_registry=ArchiveReference.model_validate(registry),
        provider_config=prepared_provider.ProviderConfig.model_validate(CONFIG),
        authorize_call=authorized.append, record_call=receipts.append,
    )
    assert len(sends) == 1
    call, = receipts
    assert call.run_id == authorized[0] and call.status == "completed"
    assert (call.input_tokens, call.output_tokens) == (80, 240)
    assert result["batch"]["intents"][0]["action"] == "BUY"
