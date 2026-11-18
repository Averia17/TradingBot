import json
from datetime import date, timedelta
from decimal import Decimal
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.operating_costs import OperatingCosts
from tradingbot_evaluation.statistics import (
    EvaluationEvidence,
    EvaluationProtocol,
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)


def nav_date(day):
    return (date(2026, 1, 1) + timedelta(days=day - 1)).isoformat()


def at(day, hour=0):
    return f"{nav_date(day)}T{hour:02d}:00:00Z"


def protocol_document():
    return {
        "schema_version": "0.1",
        "experiment_id": "synthetic-2026",
        "configuration_sha256": "a" * 64,
        "source_mode": "synthetic_fixture",
        "model_training_cutoff": None,
        "nav_basis": "operating_net",
        "splits": {
            "train_start": nav_date(1),
            "validation_start": nav_date(4),
            "test_start": nav_date(7),
            "end_exclusive": nav_date(20),
            "embargo_calendar_days": 1,
        },
        "expected_nav_dates": [nav_date(day) for day in range(1, 20)],
        "primary_comparison_id": "main",
        "comparisons": [
            {
                "comparison_id": "main",
                "strategy": "agent",
                "benchmark": "baseline",
                "benchmark_access": "synthetic_fixture",
                "minimum_daily_excess_bps": 5,
            },
            {
                "comparison_id": "variant",
                "strategy": "variant",
                "benchmark": "baseline",
                "benchmark_access": "synthetic_fixture",
                "minimum_daily_excess_bps": 5,
            },
        ],
        "bootstrap": {
            "block_marks": 2,
            "minimum_blocks": 4,
            "replications": 500,
            "seed": 137,
            "family_alpha_bps": 500,
        },
    }


def decision(id_, day, last_day, known_day=None, evidence=None):
    return {
        "decision_id": id_,
        "as_of": at(day),
        "label_end": at(last_day, 10),
        "label_available_at": at(known_day or last_day, 11),
        "inputs": evidence or [],
    }


def evidence_document(manifest):
    series = dict.fromkeys(("agent", "baseline", "variant"), Decimal("1000.00"))
    days = []
    for index, day in enumerate(manifest["expected_nav_dates"]):
        if index:
            shared = Decimal("0.01") if index % 4 in (0, 1) else Decimal("-0.01")
            excess = Decimal("0.0020") + (index // 2 % 2) * Decimal("0.0001")
            series = {
                name: (value * (1 + shared + factor * excess)).quantize(Decimal("0.00000001"))
                for (name, value), factor in zip(series.items(), (1, 0, -1), strict=True)
            }
        days.append({"day": day, "nav_usd": {name: str(value) for name, value in series.items()}})
    parsed = load_protocol(encode(manifest))
    return {
        "schema_version": "0.1",
        "protocol_sha256": protocol_sha256(parsed),
        "daily_nav": days,
        "decisions": [
            decision("train", 1, 2),
            decision("horizon_leak", 2, 4),
            decision("late_label", 2, 3, 4),
            decision("embargo_val", 4, 5),
            decision("val", 5, 6),
            decision("embargo_test", 7, 8),
            decision("test", 8, 9),
            decision("untested_label", 19, 20),
        ],
    }


def encode(value):
    return json.dumps(value).encode()


def evaluate(manifest, evidence, *, expected=None):
    protocol = load_protocol(encode(manifest))
    return evaluate_statistics(
        protocol,
        load_evidence(encode(evidence)),
        expected_protocol_sha256=expected or protocol_sha256(protocol),
    )


def test_purge_embargo_and_deterministic_paired_intervals_cover_registered_family():
    manifest = protocol_document()
    evidence = evidence_document(manifest)
    result = evaluate(manifest, evidence)
    assert result == evaluate(manifest, evidence)
    assert result["protocol_sha256"] == evidence["protocol_sha256"]
    assert result["split_audit"]["retained"] == {
        "train": ["train"],
        "validation": ["val"],
        "test": ["test"],
    }
    assert {row["decision_id"]: row["reason"] for row in result["split_audit"]["excluded"]} == {
        "horizon_leak": "label_horizon_overlap",
        "late_label": "label_not_available_before_boundary",
        "embargo_val": "embargo",
        "embargo_test": "embargo",
        "untested_label": "label_horizon_overlap",
    }
    assert result["test_window"] == {
        "anchor_mark_day": nav_date(8),
        "through_mark_day": nav_date(19),
        "paired_return_count": 11,
        "coverage": "all_declared_nav_marks_present",
    }
    assert result["bootstrap"]["nominal_family_confidence"] == "0.950000"
    assert result["bootstrap"]["nominal_per_comparison_confidence"] == "0.975000"
    assert result["bootstrap"]["complete_blocks"] == 5
    assert result["primary_comparison_id"] == "main"
    first, second = result["comparisons"]
    assert first["status"] == "above_registered_threshold"
    assert second["status"] == "below_registered_threshold"
    ci = first["daily_excess_bps_interval"]
    assert 19.0 < float(ci["lower"]) < float(ci["upper"]) < 22.0
    assert result["evidence_classification"] == "synthetic_fixture"
    assert result["clean_out_of_sample"] is False and result["alpha_measured"] is False


@pytest.mark.parametrize("kind", ["market_observation", "factual_memory"])
def test_future_information_fails_even_when_decision_would_be_purged(kind):
    manifest = protocol_document()
    evidence = evidence_document(manifest)
    evidence["decisions"][1]["inputs"] = [
        {"reference_id": "future", "kind": kind, "observed_at": at(1), "available_at": at(2, 1)}
    ]
    with pytest.raises(ValueError, match="future"):
        evaluate(manifest, evidence)


@pytest.mark.parametrize(
    "problem", ["missing", "out_of_order", "absent_benchmark", "unequal_funding"]
)
def test_incomplete_or_incomparable_nav_is_rejected(problem):
    manifest = protocol_document()
    evidence = evidence_document(manifest)
    days = evidence["daily_nav"]
    if problem == "missing":
        days.pop(9)
    elif problem == "out_of_order":
        days[9], days[10] = days[10], days[9]
    elif problem == "absent_benchmark":
        days[9]["nav_usd"].pop("baseline")
    else:
        days[0]["nav_usd"]["agent"] = "1000.01"
    with pytest.raises(ValueError, match="NAV|funding"):
        evaluate(manifest, evidence)


def test_protocol_change_is_detected_and_no_unregistered_compare_is_accepted():
    manifest = protocol_document()
    evidence = evidence_document(manifest)
    frozen = evidence["protocol_sha256"]
    manifest["bootstrap"]["seed"] += 1
    with pytest.raises(ValueError, match="protocol SHA-256"):
        evaluate(manifest, evidence, expected=frozen)
    with pytest.raises(ValueError, match="protocol SHA-256"):
        evaluate(manifest, evidence)
    manifest["primary_comparison_id"] = "late-pick"
    with pytest.raises(ValueError, match="registered"):
        load_protocol(encode(manifest))


def test_utc_boundary_purge_and_embargo_nav_does_not_enter_paired_returns():
    manifest = protocol_document()
    original = evidence_document(manifest)
    changed = evidence_document(manifest)
    changed["daily_nav"][6]["nav_usd"].update(agent="1.00", baseline="8000.00")
    examples = changed["decisions"]
    examples.append(decision("offset_decision", 3, 5) | {"as_of": "2026-01-03T23:30:00-02:00"})
    examples.append(decision("end_on_boundary", 3, 4) | {"label_end": at(4)})
    result = evaluate(manifest, changed)
    assert result["comparisons"] == evaluate(manifest, original)["comparisons"]
    exclusions = {row["decision_id"]: row for row in result["split_audit"]["excluded"]}
    assert exclusions["offset_decision"]["split"] == "validation"
    assert exclusions["offset_decision"]["reason"] == "embargo"
    assert exclusions["end_on_boundary"]["reason"] == "label_horizon_overlap"


def test_insufficient_corrected_quantile_resolution_is_inconclusive():
    manifest = protocol_document()
    manifest["bootstrap"].update(family_alpha_bps=100, replications=100)
    report = evaluate(manifest, evidence_document(manifest))
    assert report["bootstrap"]["corrected_tail_draws_floor"] == 0
    assert report["comparisons"][0]["daily_excess_bps_interval"] is None
    assert report["comparisons"][0]["inconclusive_reason"] == "too_few_corrected_tail_draws"


@pytest.mark.parametrize("cutoff", [None, "2025-07-01"])
def test_retrospective_evidence_and_weak_sample_cannot_claim_oos_or_power(cutoff):
    manifest = protocol_document()
    manifest["source_mode"] = "retrospective"
    manifest["model_training_cutoff"] = cutoff
    manifest["bootstrap"]["minimum_blocks"] = 9
    evidence = evidence_document(manifest)
    result = evaluate(manifest, evidence)
    suffix = "unknown" if cutoff is None else "unverified"
    assert result["evidence_classification"] == f"retrospective_training_cutoff_{suffix}"
    assert result["clean_out_of_sample"] is result["alpha_measured"] is False
    assert result["power_status"] == "unmeasured"
    assert all(row["status"] == "inconclusive" for row in result["comparisons"])
    assert result["comparisons"][0]["daily_excess_bps_interval"] is None
    evidence["clean_out_of_sample"] = True
    with pytest.raises(ValueError, match="unknown field"):
        evaluate(manifest, evidence)


def test_published_statistics_schemas_and_cli_use_frozen_protocol(tmp_path, capsys):
    contracts = Path(__file__).resolve().parents[3] / "contracts"
    for name, dto in (
        ("protocol", EvaluationProtocol),
        ("evidence", EvaluationEvidence),
        ("operating-costs", OperatingCosts),
    ):
        assert main(["statistics-schema", name]) == 0
        actual = json.loads(capsys.readouterr().out)
        assert actual == json.loads(
            (contracts / f"evaluation-{name}.schema.json").read_text("utf-8")
        )
        assert actual.pop("$schema") == "https://json-schema.org/draft/2020-12/schema"
        assert actual == json.loads(msgspec.json.encode(msgspec.json.schema(dto)))

    manifest = protocol_document()
    evidence = evidence_document(manifest)
    protocol_file = tmp_path / "protocol.json"
    evidence_file = tmp_path / "evidence.json"
    protocol_file.write_bytes(encode(manifest))
    evidence_file.write_bytes(encode(evidence))
    assert main(["freeze-statistics", str(protocol_file)]) == 0
    frozen = json.loads(capsys.readouterr().out)["protocol_sha256"]
    assert frozen == evidence["protocol_sha256"]
    assert (
        main(["statistics", str(protocol_file), str(evidence_file), "--protocol-sha256", frozen])
        == 0
    )
    assert json.loads(capsys.readouterr().out)["protocol_sha256"] == frozen


def test_paper_nav_provenance_must_be_bound_to_its_exclusive_protocol_source():
    manifest = protocol_document() | {
        "schema_version": "0.2",
        "nav_basis": "execution_net",
        "paper_nav_sha256": "b" * 64,
    }
    evidence = evidence_document(manifest) | {
        "schema_version": "0.2",
        "daily_nav_basis": "execution_net",
    }
    with pytest.raises(ValueError, match="paper NAV provenance"):
        evaluate(manifest, evidence)
    evidence["paper_nav_provenance"] = {
        "method": "paper_ledger_export",
        "paper_nav_sha256": "c" * 64,
        "account_id": "paper-fixture",
    }
    with pytest.raises(ValueError, match="paper NAV provenance"):
        evaluate(manifest, evidence)
    evidence["paper_nav_provenance"]["paper_nav_sha256"] = manifest["paper_nav_sha256"]
    result = evaluate(manifest, evidence)
    assert result["comparisons_nav_basis"] == "execution_net"
    assert result["paper_nav_provenance_declared"] == evidence["paper_nav_provenance"]
    assert result["verification_scope"]["ledger_provenance_verified"] is False
    manifest["replay_source"] = {
        "replay_sha256": "c" * 64,
        "benchmarks": [{"kind": "cash", "series_id": "baseline"}],
    }
    with pytest.raises(ValueError, match="mutually exclusive"):
        load_protocol(encode(manifest))


def risk_sample(days, strategy, benchmark):
    manifest = protocol_document()
    manifest.update(
        nav_basis="execution_net",
        expected_nav_dates=[nav_date(day) for day in (1, 4, *days)],
        comparisons=manifest["comparisons"][:1],
    )
    evidence = {
        "schema_version": "0.1",
        "protocol_sha256": protocol_sha256(load_protocol(encode(manifest))),
        "daily_nav": [
            {"day": nav_date(day), "nav_usd": {"agent": s, "baseline": b}}
            for day, s, b in zip(
                (1, 4, *days), ("100", "100", *strategy), ("100", "100", *benchmark), strict=True
            )
        ],
    }
    return evaluate(manifest, evidence)


def test_risk_uses_matched_paths_and_declares_unequal_mark_intervals():
    report = risk_sample(
        (8, 9, 11, 12), ("100", "110", "99", "108.9"), ("100", "120", "108", "108")
    )
    method = report["risk_method"]
    assert method["mark_count"] == 4 and method["return_count"] == 3
    assert method["calendar_days_per_interval"] == {"min": 1, "max": 2}
    assert method["return_unit"] == "basis_points_per_observed_interval"
    assert method["annualized"] is False
    comparison = report["comparisons"][0]
    assert comparison["status"] == "inconclusive"
    strategy, benchmark, excess = (
        comparison["risk"][path] for path in ("strategy", "benchmark", "excess")
    )
    assert strategy == {
        "max_drawdown_bps": "1000.000000",
        "mean_return_bps": "333.333333",
        "volatility_bps": "1154.700538",
        "downside_deviation_bps": "577.350269",
        "mean_return_per_volatility": "0.288675",
        "mean_return_per_downside_deviation": "0.577350",
    }
    assert benchmark["max_drawdown_bps"] == "1000.000000"
    assert benchmark["volatility_bps"] == "1527.525232"
    assert excess["max_relative_wealth_drawdown_bps"] == "833.333333"
    assert excess["mean_return_bps"] == "0.000000"
    assert excess["volatility_bps"] == "1000.000000"
    assert excess["downside_deviation_bps"] == "577.350269"


@pytest.mark.parametrize(
    ("days", "strategy", "volatility", "downside", "drawdown"),
    [
        ((8,), ("100",), None, None, "0.000000"),
        ((8, 9), ("100", "90"), None, "1000.000000", "1000.000000"),
        ((8, 9, 10), ("100", "90", "81"), "0.000000", "1000.000000", "1900.000000"),
    ],
)
def test_risk_ratios_need_observations_and_nonzero_variation(
    days, strategy, volatility, downside, drawdown
):
    report = risk_sample(days, strategy, ("100",) * len(days))
    risk = report["comparisons"][0]["risk"]["strategy"]
    assert risk["volatility_bps"] == volatility
    assert risk["downside_deviation_bps"] == downside
    assert risk["max_drawdown_bps"] == drawdown
    assert risk["mean_return_per_volatility"] is None
    assert risk["mean_return_per_downside_deviation"] is None
