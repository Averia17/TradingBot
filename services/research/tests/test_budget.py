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
    budget.on_llm_end(response, run_id=run_id)
    budget.on_llm_end(response, run_id=run_id)
    budget.on_llm_end(SimpleNamespace(generations=[]), run_id=uuid4())
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
