import hashlib
import json

import pytest

from tradingbot.equity_thesis import Forecast, StudyInfo, forecast_equity
from tradingbot.research import DecisionError


def forecast_with(expected_return, **overrides):
    return Forecast.model_validate({
        "expected_price_return_bps": expected_return,
        "uncertainty_bps": 31,
        "review_in_hours": 2,
        "thesis": "Test thesis",
        "invalidation_conditions": ["Test invalidation"],
        "evidence_keys": ["market_report"],
    } | overrides)


class ScriptedForecast:
    def __init__(self, response):
        self.response = response

    def with_structured_output(self, schema):
        assert schema is Forecast
        return self

    def invoke(self, prompt):
        request = json.loads(prompt[1]["content"])
        assert request["horizon_hours"] == 4 and request["security"]["symbol"] == "NVDA"
        assert "market_report" in request["reports"]
        return self.response


@pytest.fixture
def study():
    return StudyInfo.model_validate({
        "source_mode": "observed_market",
        "study_sha256": "b" * 64,
        "as_of": "2026-10-05T14:35:00Z",
        "horizon_hours": 4,
        "securities": [{"security_id": "test-nvda", "symbol": "NVDA"}],
        "requires_reconciliation": False,
    })


@pytest.mark.parametrize("expected_return", [-250, 0, 250])
def test_thesis_links_to_the_actual_report_and_leaves_execution_to_go(study, expected_return):
    reports = {"market_report": "Pinned market evidence"}
    model = ScriptedForecast(forecast_with(expected_return))
    thesis = forecast_equity(model, study, study.securities[0], reports)
    assert thesis["expected_price_return_bps"] == expected_return
    assert thesis["evidence_sha256"] == [hashlib.sha256(b"Pinned market evidence").hexdigest()]
    assert not {"evidence_keys", "notional_usd", "leverage", "action"}.intersection(thesis)


@pytest.mark.parametrize("changes", [
    {"review_in_hours": 5},
    {"evidence_keys": ["news_report"]},
    {"evidence_keys": ["market_report", "market_report"]},
    {"thesis": " "},
])
def test_thesis_fails_closed_for_incoherent_model_output(study, changes):
    model = ScriptedForecast(forecast_with(-250, **changes))
    with pytest.raises(DecisionError):
        forecast_equity(model, study, study.securities[0], {"market_report": "Pinned data"})
