import json
from datetime import date, timedelta
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.power_review import review_forward_power
from tradingbot_evaluation.statistics import load_protocol, protocol_sha256

EXAMPLE = Path(__file__).resolve().parents[1] / "examples" / "forward-paper-protocol.json"


def design_document():
    document = json.loads(EXAMPLE.read_bytes())
    first = date.fromisoformat(document["splits"]["train_start"])
    document["expected_nav_dates"] = [(first + timedelta(days=i)).isoformat() for i in range(43)]
    document["splits"]["end_exclusive"] = (first + timedelta(days=43)).isoformat()
    document["paper_forward_source"]["checkpoints_utc"] = document["expected_nav_dates"][-2:]
    document["paper_forward_source"]["power_review"] = {
        "method": "gaussian_ar1_paired_moving_block_percentile_v1",
        "alternative_increment_bps": 40,
        "daily_excess_sd_bps": 20,
        "lag_one_correlation_bps": 6000,
        "target_detection_bps": 8000,
        "trials": 100,
        "seed": 19,
    }
    return document


def test_preregistered_power_uses_final_calendar_method_and_fixed_seed():
    document = design_document()
    document["comparisons"][0]["minimum_daily_excess_bps"] = 5
    protocol = load_protocol(msgspec.json.encode(document))
    assert (
        protocol_sha256(protocol)
        == "491bc0c2eb3a4109535314063fa813608cb785850e40c7f51360a5a0de6225bd"
    )
    result = review_forward_power(protocol)
    assert result == review_forward_power(protocol)
    assert result["planned_paired_return_count"] == 40
    assert result["status"] == "simulated"
    assert result["modeled_sensitivity"] == "target_met_under_model"
    assert result["null_mean_excess_bps"] == 5
    assert result["alternative_mean_excess_bps"] == 45
    assert result["empirical_returns_used"] is False
    assert result["model_assumptions_verified"] is False
    null, alternative = result["null_upper_rejection"], result["alternative_detection"]
    assert null["success_count"] == 17
    assert alternative["success_count"] == 100
    assert float(alternative["monte_carlo_95_lower"]) < 1
    assert alternative["monte_carlo_95_upper"] == "1.000000"
    assert result["nominal_upper_tail_probability"] == "0.025000"


def test_power_rejects_irregular_calendar_and_caps_simulation_work():
    document = design_document()
    del document["expected_nav_dates"][8]
    with pytest.raises(ValueError, match="complete UTC calendar"):
        load_protocol(msgspec.json.encode(document))
    document = design_document()
    document["bootstrap"]["replications"] = 10_000
    document["paper_forward_source"]["power_review"]["trials"] = 2000
    with pytest.raises(ValueError, match="Power review exceeds the local work limit"):
        load_protocol(msgspec.json.encode(document))


def test_power_reports_low_sensitivity_and_null_inflation_with_persistent_noise():
    document = design_document()
    document["paper_forward_source"]["power_review"].update(
        alternative_increment_bps=1, daily_excess_sd_bps=60, lag_one_correlation_bps=9500
    )
    result = review_forward_power(load_protocol(msgspec.json.encode(document)))
    assert result["modeled_sensitivity"] == "below_target_under_model"
    assert result["modeled_null_calibration"] == "above_nominal_under_model"
    assert (
        result["alternative_detection"]["success_count"]
        >= (result["null_upper_rejection"]["success_count"])
    )


@pytest.mark.parametrize(
    ("config", "reason"),
    [
        ({"minimum_blocks": 21}, "too_few_complete_blocks"),
        ({"family_alpha_bps": 1}, "too_few_corrected_tail_draws"),
    ],
)
def test_power_reports_unavailable_for_underpowered_design(config, reason):
    document = design_document()
    document["bootstrap"].update(config)
    protocol = load_protocol(msgspec.json.encode(document))
    result = review_forward_power(protocol)
    assert result["status"] == "unavailable"
    assert result["reason"] == reason
    assert result["null_upper_rejection"] is None
    assert result["alternative_detection"] is None


def test_known_covariance_review_reproduces_independent_null_and_alternative_trials():
    document = design_document()
    document["paper_forward_source"]["power_review"].update(
        method="gaussian_ar1_paired_mean_v1", alternative_increment_bps=8, trials=2000
    )
    document["comparisons"][0]["minimum_daily_excess_bps"] = 5
    protocol = load_protocol(msgspec.json.encode(document))
    review = review_forward_power(protocol)
    assert review == review_forward_power(protocol)
    assert review["null_and_alternative_share_trials"] is False
    assert review["model_assumptions_verified"] is False
    assert review["analytic_null_upper_rejection_probability"] == "0.025000"
    assert review["mean_standard_error_bps"] == "6.174545"
    for event, analytic in (
        ("null_upper_rejection", "analytic_null_upper_rejection_probability"),
        ("alternative_detection", "analytic_alternative_detection_probability"),
    ):
        assert (
            float(review[event]["monte_carlo_95_lower"])
            <= float(review[analytic])
            <= float(review[event]["monte_carlo_95_upper"])
        )
    assert review["modeled_null_calibration"] == "inconclusive"


def test_known_covariance_review_does_not_require_bootstrap_draws_for_simulation():
    document = design_document()
    document["bootstrap"].update(minimum_blocks=21, replications=10_000)
    document["paper_forward_source"]["power_review"].update(
        method="gaussian_ar1_paired_mean_v1", trials=2000
    )
    result = review_forward_power(load_protocol(msgspec.json.encode(document)))
    assert result["status"] == "simulated"
