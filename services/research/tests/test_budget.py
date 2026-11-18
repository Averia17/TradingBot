import json
from pathlib import Path
from types import SimpleNamespace
from uuid import uuid4

import pytest

from tradingbot.budget import CallBudget, CallBudgetExceeded


def test_call_cap_fails_before_another_provider_request():
    budget = CallBudget(max_calls=1)
    budget.on_chat_model_start({}, [], run_id=uuid4())
    with pytest.raises(CallBudgetExceeded):
        budget.on_chat_model_start({}, [], run_id=uuid4())
    assert budget.summary()["llm_calls"] == 1


def test_billed_tokens_count_once_and_missing_usage_is_visible():
    budget = CallBudget(max_calls=3)
    response = SimpleNamespace(
        generations=[
            [
                SimpleNamespace(
                    message=SimpleNamespace(
                        usage_metadata={"input_tokens": 123, "output_tokens": 45}
                    )
                )
            ]
        ]
    )
    run_id = uuid4()
    missing_run_id = uuid4()
    budget.on_chat_model_start({}, [], run_id=run_id)
    budget.on_chat_model_start({}, [], run_id=missing_run_id)
    budget.on_llm_end(response, run_id=run_id)
    budget.on_llm_end(response, run_id=run_id)
    budget.on_llm_end(SimpleNamespace(generations=[]), run_id=missing_run_id)
    assert budget.summary()["input_tokens"] == 123
    assert budget.summary()["output_tokens"] == 45
    assert budget.summary()["missing_usage_calls"] == 1


def test_call_budget_must_be_positive():
    with pytest.raises(ValueError):
        CallBudget(max_calls=0)


def test_real_langchain_callback_pipeline_propagates_budget_failure():
    from langchain_core.language_models.fake_chat_models import FakeListChatModel

    budget = CallBudget(max_calls=1)
    model = FakeListChatModel(responses=["fixture"], callbacks=[budget])
    assert model.invoke("first").content == "fixture"
    with pytest.raises(CallBudgetExceeded):
        model.invoke("must not reach provider")
    assert budget.summary()["llm_calls"] == 1


def test_call_evidence_includes_subsets_once_and_keeps_unknowns_null():
    budget = CallBudget(
        max_calls=3,
        identities={("openai", "gpt-example"): ("openai", "gpt-example")},
    )
    run_id = uuid4()
    with budget.role("research"):
        budget.on_chat_model_start(
            {"api_key": "fixture-do-not-capture"},
            ["fixture-content-not-observed"],
            run_id=run_id,
            metadata={
                "ls_provider": "openai",
                "ls_model_name": "gpt-example",
                "authorization": "fixture-secret",
            },
        )
    response = SimpleNamespace(
        generations=[
            [
                SimpleNamespace(
                    message=SimpleNamespace(
                        usage_metadata={
                            "input_tokens": 80,
                            "output_tokens": 20,
                            "total_tokens": 100,
                            "input_token_details": {"cache_read": 0},
                            "output_token_details": {"reasoning": 7},
                        }
                    )
                )
            ]
        ]
    )
    budget.on_llm_end(response, run_id=run_id)
    budget.on_llm_end(response, run_id=run_id)
    budget.on_llm_end(response, run_id=uuid4())
    report = budget.usage_report("provider_reported", complete=True)
    assert report.llm_calls == 1
    assert report.capture_status == "complete"
    (call,) = report.calls
    assert (call.run_id, call.provider, call.model, call.role, call.status) == (
        run_id,
        "openai",
        "gpt-example",
        "research",
        "completed",
    )
    assert (
        call.input_tokens,
        call.output_tokens,
        call.cache_read_tokens,
        call.cache_write_tokens,
        call.reasoning_tokens,
    ) == (80, 20, 0, None, 7)
    assert call.usage_source == "langchain_usage_metadata"
    assert budget.summary()["input_tokens"] == 80
    assert budget.summary()["output_tokens"] == 20
    assert "fixture-" not in report.model_dump_json()


def test_unfinished_and_failed_model_calls_leave_partial_evidence(tmp_path):
    from tradingbot.budget import capture_usage
    from tradingbot.llm_usage import LLMUsageReport

    target = tmp_path / "usage.json"
    budget = CallBudget(max_calls=2)
    failed, unfinished = uuid4(), uuid4()
    with pytest.raises(RuntimeError, match="fixture exception"):
        with capture_usage(target, budget, "provider_reported"):
            budget.on_chat_model_start({}, [], run_id=failed)
            budget.on_chat_model_start({}, [], run_id=unfinished)
            budget.on_llm_error(RuntimeError("private query"), run_id=failed)
            raise RuntimeError("fixture exception")
    raw = target.read_text()
    evidence = LLMUsageReport.model_validate_json(raw)
    assert evidence.capture_status == "partial"
    assert [row.status for row in evidence.calls] == ["failed", "unfinished"]
    assert all(row.usage_source == "unavailable" for row in evidence.calls)
    assert "private query" not in raw
    with pytest.raises(FileExistsError):
        with capture_usage(target, budget, "provider_reported"):
            pytest.fail("existing evidence must be left intact")
    assert target.read_text() == raw


def test_provider_adapter_missing_details_and_malformed_subsets_are_not_zeros():
    from tradingbot.llm_usage import observe_usage

    def response(input_details, output_details):
        return SimpleNamespace(
            generations=[
                [
                    SimpleNamespace(
                        message=SimpleNamespace(
                            usage_metadata={
                                "input_tokens": 100,
                                "output_tokens": 10,
                                "total_tokens": 110,
                                "input_token_details": input_details,
                                "output_token_details": output_details,
                            }
                        )
                    )
                ]
            ]
        )

    anthropic, standard_tariff = observe_usage(
        response({"cache_read": 5, "cache_creation": 0, "ephemeral_5m_input_tokens": 40}, {}),
        "anthropic",
    )
    assert (anthropic.cache_read_tokens, anthropic.cache_write_tokens) == (5, None)
    assert not standard_tariff
    google, _ = observe_usage(response({"cache_read": 0}, {"reasoning": 0}), "google")
    assert (google.cache_read_tokens, google.reasoning_tokens) == (None, None)
    malformed, _ = observe_usage(
        response({"cache_read": 70, "cache_creation": 50}, {"reasoning": 1_000_000_001}),
        "openai",
    )
    assert (malformed.input_tokens, malformed.output_tokens) == (100, 10)
    assert (
        malformed.cache_read_tokens,
        malformed.cache_write_tokens,
        malformed.reasoning_tokens,
    ) == (None, None, None)


def test_alternate_tariff_preserves_tokens_without_a_standard_rate_identity():
    budget = CallBudget(1, fixed_identity=("openai", "gpt-example"))
    run_id = uuid4()
    budget.on_chat_model_start({}, [], run_id=run_id)
    message = SimpleNamespace(
        response_metadata={"service_tier": "priority"},
        usage_metadata={
            "input_tokens": 100,
            "output_tokens": 20,
            "total_tokens": 120,
            "input_token_details": {"priority": 99, "priority_cache_read": 1},
            "output_token_details": {"priority": 20},
        },
    )
    budget.on_llm_end(
        SimpleNamespace(generations=[[SimpleNamespace(message=message)]]), run_id=run_id
    )
    (row,) = budget.usage_report("provider_reported", complete=True).calls
    assert row.provider is None and row.model is None
    assert (row.input_tokens, row.output_tokens) == (100, 20)


def test_model_identity_uses_configured_native_model_and_drops_ambiguous_endpoint(monkeypatch):
    from tradingbot.adapters.tradingagents import usage_model_identities

    monkeypatch.delenv("OPENAI_BASE_URL", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.delenv("GOOGLE_GEMINI_BASE_URL", raising=False)
    monkeypatch.delenv("GOOGLE_VERTEX_BASE_URL", raising=False)
    configured = {
        "llm_provider": "openai",
        "quick_think_llm": "gpt-example",
        "deep_think_llm": "models/gemini-example",
        "deep_think_provider": "google",
    }
    assert usage_model_identities(configured) == {
        ("openai", "gpt-example"): ("openai", "gpt-example"),
        ("google_genai", "gemini-example"): ("google", "gemini-example"),
    }
    assert (
        usage_model_identities(
            configured
            | {
                "deep_think_llm": "gpt-example",
                "deep_think_provider": "gateway",
            }
        )
        == {}
    )
    assert usage_model_identities(configured | {"quick_think_llm": "sk-fixture-hidden"}) == {
        ("google_genai", "gemini-example"): ("google", "gemini-example")
    }


def test_published_usage_contract_matches_serialized_schema_and_fixture(tmp_path):
    from tradingbot.cli import main
    from tradingbot.llm_usage import LLMUsageReport

    root = Path(__file__).resolve().parents[3]
    generated = tmp_path / "llm-usage.schema.json"
    assert main(["schema", "--document", "llm-usage", "--output", str(generated)]) == 0
    published = root / "contracts/research-llm-usage.schema.json"
    assert json.loads(generated.read_bytes()) == json.loads(published.read_bytes())
    example = root / "contracts/valid-research-llm-usage.json"
    report = LLMUsageReport.model_validate_json(example.read_bytes())
    assert report.llm_calls == 2
    assert [call.status for call in report.calls] == ["completed", "failed"]
    assert report.calls[1].usage_source == "langchain_usage_metadata"
