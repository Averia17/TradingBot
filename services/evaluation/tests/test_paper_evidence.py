import json
from datetime import date, timedelta
from hashlib import sha256
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.checkpoint_history import CheckpointReceipt
from tradingbot_evaluation.forward_freeze import (
    StrategyFiles,
    forward_configuration_sha256,
    forward_freeze_sha256,
    freeze_forward,
    load_forward_freeze,
    load_forward_plan,
)
from tradingbot_evaluation.operating_costs import COST_CATEGORIES
from tradingbot_evaluation.paper_evidence import (
    PaperBenchmark,
    build_paper_evidence,
    build_paper_evidence_with_cost_diagnostics,
    load_paper_benchmark,
)
from tradingbot_evaluation.statistics import evaluate_statistics, load_protocol, protocol_sha256

ACCOUNT = "11111111-1111-4111-8111-111111111111"
INSTRUMENT = {"instrument_id": "fixture:TSLA", "registry_version": "fixture-v1", "decimals": 2}


def forward_registration(
    root,
    *,
    operating=False,
    with_index=False,
    power_review=None,
    power_days=43,
    checkpoint_history=False,
    regime_review=None,
):
    (root / "strategy.txt").write_text("offline test strategy", encoding="utf-8")
    plan = load_forward_plan(
        json.dumps(
            {
                "schema_version": "0.1",
                "experiment_id": "paper-cash",
                "mode": "paper",
                "primary_strategy": "paper",
                "change_policy": "new_run_for_any_change",
                "insufficient_power": "continue_in_current_mode",
                "strategies": {
                    "paper": {
                        "model_id": "offline",
                        "files": {
                            name: ["strategy.txt"] for name in StrategyFiles.__struct_fields__
                        },
                    }
                },
            }
        ).encode()
    )
    nav = report()
    template, _ = freeze(
        msgspec.json.encode(nav),
        nav,
        benchmark_raw=msgspec.json.encode(benchmark(nav)) if with_index else None,
    )
    document = msgspec.to_builtins(template)
    del document["paper_nav_sha256"]
    document.pop("paper_benchmark_sha256", None)
    document["configuration_sha256"] = forward_configuration_sha256(root, plan)
    document["nav_basis"] = "operating_net" if operating else "execution_net"
    document["paper_forward_source"] = {
        "account_id": ACCOUNT,
        "initial_cash_usd": "1000",
        "valuation_source": {"kind": "synthetic_fixture", "id": "test-marks"},
        "mark_max_age_seconds": 90,
        "checkpoints_utc": ["2026-10-04", "2026-10-05"],
        "maximum_drawdown_bps": 250,
    }
    if checkpoint_history:
        document["paper_forward_source"]["checkpoint_history"] = "require_prior_receipt"
    if regime_review is not None:
        document["paper_forward_source"]["regime_review"] = regime_review
    if with_index:
        source = benchmark(nav)
        document["paper_forward_source"]["benchmark"] = {
            field: source[field]
            for field in ("series_id", "modeled_entry_cost_bps", "max_age_seconds")
        }
    if power_review is not None:
        first_day = date.fromisoformat(document["expected_nav_dates"][0])
        final_day = first_day + timedelta(days=power_days - 1)
        document["expected_nav_dates"] = [
            (first_day + timedelta(days=index)).isoformat() for index in range(power_days)
        ]
        document["splits"]["end_exclusive"] = (final_day + timedelta(days=1)).isoformat()
        if final_day.isoformat() not in document["paper_forward_source"]["checkpoints_utc"]:
            document["paper_forward_source"]["checkpoints_utc"].append(final_day.isoformat())
        document["paper_forward_source"]["power_review"] = power_review
    protocol = load_protocol(msgspec.json.encode(document))
    frozen = freeze_forward(root, plan, protocol)
    protocol_path, freeze_path = root / "protocol.json", root / "freeze.json"
    protocol_path.write_bytes(msgspec.json.encode(protocol, order="deterministic"))
    freeze_path.write_bytes(msgspec.json.encode(frozen, order="deterministic"))
    return [
        "forward-paper-report",
        str(freeze_path),
        str(protocol_path),
        "--root",
        str(root),
        "--freeze-sha256",
        forward_freeze_sha256(frozen),
    ]


def forward_cli(root, prefix, raw, *, through="2026-10-05", nav_pin=None, extra=()):
    nav_path = root / "nav.json"
    nav_path.write_bytes(raw)
    return main(
        [
            *prefix,
            "--nav",
            str(nav_path),
            "--through",
            through,
            "--nav-sha256",
            nav_pin or sha256(raw).hexdigest(),
            *extra,
        ]
    )


def test_forward_freezes_policy_before_nav_and_reports_registered_checkpoint(tmp_path, capsys):
    frozen = forward_registration(tmp_path, operating=True)
    nav = report()
    nav["daily_nav"].pop()
    raw = msgspec.json.encode(nav)
    assert forward_cli(tmp_path, frozen, raw, through="2026-10-04") == 0
    first = capsys.readouterr().out
    result = json.loads(first)
    audit = result["forward_paper"]
    assert audit["checkpoint_utc"] == "2026-10-04"
    assert audit["registered_protocol_sha256"] == protocol_sha256(
        load_protocol((tmp_path / "protocol.json").read_bytes())
    )
    assert audit["paper_nav_sha256"] == sha256(raw).hexdigest()
    assert audit["status"] == "inconclusive"
    assert audit["requested_nav_basis_available"] is False
    assert result["test_window"]["through_mark_day"] == "2026-10-04"
    assert result["verification_scope"]["protocol_freeze_time_verified"] is False
    assert result["alpha_measured"] is False
    assert forward_cli(tmp_path, frozen, raw, through="2026-10-04") == 0
    assert capsys.readouterr().out == first
    assert forward_cli(tmp_path, frozen, raw) == 2
    assert "cover every frozen day" in capsys.readouterr().err


def test_forward_checkpoints_require_previous_pin_and_preserve_old_nav_rows(tmp_path, capsys):
    frozen = forward_registration(tmp_path, checkpoint_history=True)
    nav = report()
    earlier = dict(nav, daily_nav=nav["daily_nav"][:-1])
    assert forward_cli(tmp_path, frozen, msgspec.json.encode(earlier), through="2026-10-04") == 0
    first_report = json.loads(capsys.readouterr().out)
    assert first_report["forward_paper"]["registered_return_threshold_status"] is None
    first = first_report["checkpoint_receipt"]
    assert first["checkpoint_utc"] == "2026-10-04"
    assert first["previous_receipt_sha256"] is None
    archive_path = tmp_path / "prior-checkpoint.json"
    raw_receipt = msgspec.json.encode(first)
    archive_path.write_bytes(raw_receipt)
    receipt_pin = sha256(raw_receipt).hexdigest()
    prior = ("--previous-receipt", str(archive_path), "--previous-receipt-sha256", receipt_pin)
    nav["input_sha256"] = "d" * 64
    assert forward_cli(tmp_path, frozen, msgspec.json.encode(nav)) == 2
    assert "previous checkpoint receipt" in capsys.readouterr().err
    assert forward_cli(tmp_path, frozen, msgspec.json.encode(nav), extra=prior) == 0
    final = json.loads(capsys.readouterr().out)["checkpoint_receipt"]
    assert final["previous_receipt_sha256"] == receipt_pin
    assert final["nav"]["artifact_sha256"] == sha256(msgspec.json.encode(nav)).hexdigest()
    assert final["nav"]["prefix_sha256"] != first["nav"]["prefix_sha256"]
    assert (
        forward_cli(tmp_path, frozen, msgspec.json.encode(nav), extra=(*prior[:-1], "a" * 64)) == 2
    )
    assert "receipt SHA-256" in capsys.readouterr().err
    wrong_day = msgspec.json.encode(first | {"checkpoint_utc": "2026-10-03"})
    archive_path.write_bytes(wrong_day)
    assert (
        forward_cli(
            tmp_path,
            frozen,
            msgspec.json.encode(nav),
            extra=(*prior[:-1], sha256(wrong_day).hexdigest()),
        )
        == 2
    )
    assert "immediately previous registered checkpoint" in capsys.readouterr().err
    archive_path.write_bytes(raw_receipt)
    # The altered NAV still reconciles to the altered mark and passes the
    # independent new-artifact hash. The retained prefix must catch it.
    nav["daily_nav"][1]["positions"][0]["mark"]["price_usd"] = "102"
    nav["daily_nav"][1]["nav_usd"] = "1001.95000000"
    assert forward_cli(tmp_path, frozen, msgspec.json.encode(nav), extra=prior) == 2
    assert "previous checkpoint NAV" in capsys.readouterr().err


def test_forward_receipt_preserves_index_and_expense_history(tmp_path, capsys):
    frozen = forward_registration(
        tmp_path, operating=True, with_index=True, checkpoint_history=True
    )
    nav = report()
    index = benchmark(nav)
    costs = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "basis": "additional_to_execution_net",
        "first_mark_day": "2026-10-01",
        "last_mark_day": "2026-10-04",
        "expenses": [
            {
                "cost_id": "first-llm",
                "series_id": "paper",
                "category": "llm",
                "incurred_at": "2026-10-02T18:00:00Z",
                "amount_usd": "0.041",
                "reference_id": "synthetic-call",
            }
        ],
        "coverage": [],
    }

    def pinned(flag, document):
        raw = msgspec.json.encode(document)
        path = tmp_path / (flag + ".json")
        path.write_bytes(raw)
        return (f"--{flag}", str(path), f"--{flag}-sha256", sha256(raw).hexdigest())

    early = (
        *pinned("benchmark", dict(index, levels=index["levels"][:-1])),
        *pinned("operating-costs", costs),
    )
    assert (
        forward_cli(
            tmp_path,
            frozen,
            msgspec.json.encode(dict(nav, daily_nav=nav["daily_nav"][:-1])),
            through="2026-10-04",
            extra=early,
        )
        == 0
    )
    previous = json.loads(capsys.readouterr().out)["checkpoint_receipt"]
    receipt_args = pinned("previous-receipt", previous)
    costs["last_mark_day"] = "2026-10-05"
    costs["expenses"].append(
        {
            **costs["expenses"][0],
            "cost_id": "second-llm",
            "incurred_at": "2026-10-05T10:00:00Z",
            "amount_usd": "0.2",
        }
    )
    nav_raw = msgspec.json.encode(nav)
    args = (*pinned("benchmark", index), *pinned("operating-costs", costs), *receipt_args)
    assert forward_cli(tmp_path, frozen, nav_raw, extra=args) == 0
    final = json.loads(capsys.readouterr().out)["checkpoint_receipt"]
    assert final["previous_receipt_sha256"] == receipt_args[-1]
    assert final["operating_costs"]["prefix_sha256"] != previous["operating_costs"]["prefix_sha256"]
    good_index = pinned("benchmark", index)
    assert forward_cli(tmp_path, frozen, nav_raw, extra=(*good_index, *receipt_args)) == 2
    assert "previous checkpoint operating cost" in capsys.readouterr().err
    costs["expenses"][0]["amount_usd"] = "0.042"
    cost_args = pinned("operating-costs", costs)
    assert (
        forward_cli(tmp_path, frozen, nav_raw, extra=(*good_index, *cost_args, *receipt_args)) == 2
    )
    assert "previous checkpoint operating cost" in capsys.readouterr().err
    costs["expenses"][0]["amount_usd"] = "0.041"
    index["levels"][1]["level"] = "102"
    bad_index = pinned("benchmark", index)
    cost_args = pinned("operating-costs", costs)
    assert (
        forward_cli(tmp_path, frozen, nav_raw, extra=(*bad_index, *cost_args, *receipt_args)) == 2
    )
    assert "previous checkpoint benchmark" in capsys.readouterr().err


def test_published_checkpoint_receipt_matches_serialized_contract():
    root = Path(__file__).resolve().parents[3]
    schema = (root / "contracts/forward-checkpoint-receipt.schema.json").read_bytes()
    assert json.loads(schema) == {
        "$schema": "https://json-schema.org/draft/2020-12/schema"
    } | msgspec.json.schema(CheckpointReceipt)


def test_forward_reports_same_frozen_design_before_nav_and_at_interim_checkpoint(tmp_path, capsys):
    prefix = forward_registration(
        tmp_path,
        operating=True,
        power_review={
            "method": "gaussian_ar1_paired_moving_block_percentile_v1",
            "alternative_increment_bps": 40,
            "daily_excess_sd_bps": 20,
            "lag_one_correlation_bps": 6000,
            "target_detection_bps": 8000,
            "trials": 100,
            "seed": 19,
        },
    )
    protocol_path = tmp_path / "protocol.json"
    assert not (tmp_path / "nav.json").exists()
    assert main(["forward-paper-power", str(protocol_path)]) == 0
    before_nav = json.loads(capsys.readouterr().out)
    assert before_nav["power_review"]["planned_paired_return_count"] == 40
    nav = report()
    nav["daily_nav"].pop()
    assert forward_cli(tmp_path, prefix, msgspec.json.encode(nav), through="2026-10-04") == 0
    result = json.loads(capsys.readouterr().out)
    assert result["power_review"] == before_nav["power_review"]
    assert result["forward_paper"]["registered_protocol_sha256"] == before_nav["protocol_sha256"]
    assert result["test_window"]["paired_return_count"] == 1
    assert result["power_status"] == "conditionally_simulated"
    assert result["power_review"]["registered_nav_basis"] == "operating_net"
    assert result["forward_paper"]["status"] == "inconclusive"
    assert result["forward_paper"]["inference_scope"] == "descriptive_interim"
    assert result["alpha_measured"] is False
    document = json.loads(protocol_path.read_bytes())
    document["paper_forward_source"]["power_review"]["seed"] += 1
    protocol_path.write_bytes(msgspec.json.encode(document))
    assert forward_cli(tmp_path, prefix, msgspec.json.encode(nav), through="2026-10-04") == 2
    assert "frozen evaluation protocol" in capsys.readouterr().err


def test_forward_regimes_use_only_start_of_interval_index_history(tmp_path, capsys):
    design = {
        "lookback_utc_days": 2,
        "minimum_distinct_observations": 3,
        "rising_threshold_bps": 100,
        "declining_threshold_bps": 100,
        "minimum_pairs_per_regime": 2,
    }
    prefix = forward_registration(tmp_path, with_index=True, operating=True, regime_review=design)
    nav = report()
    index = benchmark(nav)
    for row, level in zip(index["levels"], ("100", "105", "102", "100", "120"), strict=True):
        row["level"] = level

    def evaluate():
        raw_index = msgspec.json.encode(index)
        path = tmp_path / "index.json"
        path.write_bytes(raw_index)
        args = ("--benchmark", str(path), "--benchmark-sha256", sha256(raw_index).hexdigest())
        assert forward_cli(tmp_path, prefix, msgspec.json.encode(nav), extra=args) == 0
        result = json.loads(capsys.readouterr().out)
        assert result["alpha_measured"] is False
        return result["regime_review"]

    first = evaluate()
    assert first["nav_basis"] == "execution_net"
    assert first["requested_nav_basis_available"] is False
    assert first["total_paired_returns"] == 2
    assert first["all_registered_regimes_covered"] is False
    assert [row["regime"] for row in first["pairs"]] == ["rising", "declining"]
    assert first["pairs"][0]["trailing_index_return_bps"] == "200.000000"
    assert first["pairs"][1]["through_utc"] == "2026-10-05"
    assert first["regimes"]["flat"]["paired_returns"] == 0
    index["levels"][-1]["level"] = "80"
    changed_outcome = evaluate()
    assert [row["regime"] for row in changed_outcome["pairs"]] == ["rising", "declining"]
    assert (
        changed_outcome["pairs"][-1]["daily_excess_bps"] != first["pairs"][-1]["daily_excess_bps"]
    )
    index["levels"][1] = dict(index["levels"][0], day="2026-10-02")
    repeated_price = evaluate()
    assert repeated_price["pairs"][0]["regime"] == "unclassified"
    assert repeated_price["pairs"][0]["reason"] == "too_few_distinct_index_observations"
    assert repeated_price["total_paired_returns"] == 2


@pytest.mark.parametrize(
    ("with_index", "minimum_observations", "message"),
    [(False, 3, "primary total-return index"), (True, 4, "feasible observation minimum")],
)
def test_regime_policy_rejects_missing_index_and_impossible_observation_count(
    tmp_path, with_index, minimum_observations, message
):
    with pytest.raises(ValueError, match=message):
        forward_registration(
            tmp_path,
            with_index=with_index,
            regime_review={
                "lookback_utc_days": 2,
                "minimum_distinct_observations": minimum_observations,
                "rising_threshold_bps": 100,
                "declining_threshold_bps": 100,
                "minimum_pairs_per_regime": 2,
            },
        )


@pytest.mark.parametrize("operating", [False, True])
def test_known_covariance_paper_interval_requires_final_requested_basis(
    tmp_path, capsys, operating
):
    prefix = forward_registration(
        tmp_path,
        operating=operating,
        power_days=5,
        power_review={
            "method": "gaussian_ar1_paired_mean_v1",
            "alternative_increment_bps": 5,
            "daily_excess_sd_bps": 1,
            "lag_one_correlation_bps": 0,
            "target_detection_bps": 8000,
            "trials": 200,
            "seed": 23,
        },
    )
    nav = report()
    latest = nav["daily_nav"].pop()
    assert forward_cli(tmp_path, prefix, msgspec.json.encode(nav), through="2026-10-04") == 0
    interim = json.loads(capsys.readouterr().out)
    assert interim["model_conditional_inference"]["reason"] == "final_checkpoint_required"

    nav["daily_nav"].append(latest)
    assert forward_cli(tmp_path, prefix, msgspec.json.encode(nav)) == 0
    result = json.loads(capsys.readouterr().out)
    modeled = result["model_conditional_inference"]
    assert modeled["model_assumptions_verified"] is False
    assert result["comparisons"][0]["daily_excess_bps_interval"] is None
    assert result["forward_paper"]["status"] == "inconclusive"
    assert result["alpha_measured"] is False
    if operating:
        assert modeled["reason"] == "registered_nav_basis_unavailable"
        assert modeled["daily_excess_bps_interval"] is None
    else:
        assert modeled["status"] == "below_threshold_under_model"
        assert modeled["daily_excess_bps_interval"] == {"lower": "-6.271287", "upper": "-3.499478"}
        assert modeled["mean_standard_error_bps"] == "0.707107"


def test_forward_refuses_relocked_results_outside_registered_policy(tmp_path, capsys):
    frozen = forward_registration(tmp_path)
    nav = report()
    raw = msgspec.json.encode(nav)
    assert forward_cli(tmp_path, frozen, raw, nav_pin="a" * 64) == 2
    assert "SHA-256" in capsys.readouterr().err
    assert forward_cli(tmp_path, frozen, raw, through="2026-10-03") == 2
    assert "checkpoint" in capsys.readouterr().err
    nav["valuation_source"]["id"] = "replacement"
    assert forward_cli(tmp_path, frozen, msgspec.json.encode(nav)) == 2
    assert "registered paper source" in capsys.readouterr().err
    (tmp_path / "strategy.txt").write_text("changed policy", encoding="utf-8")
    assert forward_cli(tmp_path, frozen, raw) == 2
    assert "configuration SHA-256" in capsys.readouterr().err


def test_forward_requires_registered_benchmark_and_reports_pinned_expenses(tmp_path, capsys):
    frozen = forward_registration(
        tmp_path,
        operating=True,
        with_index=True,
        regime_review={
            "lookback_utc_days": 2,
            "minimum_distinct_observations": 2,
            "rising_threshold_bps": 100,
            "declining_threshold_bps": 100,
            "minimum_pairs_per_regime": 1,
        },
        power_days=5,
        power_review={
            "method": "gaussian_ar1_paired_mean_v1",
            "alternative_increment_bps": 10,
            "daily_excess_sd_bps": 20,
            "lag_one_correlation_bps": 6000,
            "target_detection_bps": 8000,
            "trials": 100,
            "seed": 19,
        },
    )
    nav = report()
    raw = msgspec.json.encode(nav)
    assert forward_cli(tmp_path, frozen, raw) == 2
    assert "registered index" in capsys.readouterr().err
    index = benchmark(nav)
    benchmark_path = tmp_path / "benchmark.json"
    costs_path = tmp_path / "expenses.json"
    costs = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "basis": "additional_to_execution_net",
        "first_mark_day": "2026-10-01",
        "last_mark_day": "2026-10-05",
        "expenses": [
            {
                "cost_id": "expense",
                "series_id": "paper",
                "category": "llm",
                "incurred_at": "2026-10-02T12:00:00Z",
                "amount_usd": "30",
                "reference_id": "scripted-example",
            }
        ],
        "coverage": [
            {
                "series_id": series,
                "category": category,
                "status": "complete" if (series, category) == ("paper", "llm") else "zero",
                "reference_id": "scripted-example",
            }
            for series in ("paper", "uninvested-cash", "fixture-index")
            for category in COST_CATEGORIES
        ],
    }
    costs_raw = msgspec.json.encode(costs)
    costs_path.write_bytes(costs_raw)
    cost_pin = sha256(costs_raw).hexdigest()
    index_raw = msgspec.json.encode(index)
    benchmark_path.write_bytes(index_raw)
    inputs = (
        "--benchmark",
        str(benchmark_path),
        "--benchmark-sha256",
        sha256(index_raw).hexdigest(),
        "--operating-costs",
        str(costs_path),
        "--operating-costs-sha256",
        cost_pin,
    )
    assert forward_cli(tmp_path, frozen, raw, extra=inputs) == 0
    result = json.loads(capsys.readouterr().out)
    audit = result["forward_paper"]
    assert audit["period_status"] == "final"
    assert audit["operating_costs_file_sha256"] == cost_pin
    assert audit["status"] == "drawdown_limit_exceeded"
    assert result["comparisons_nav_basis"] == "operating_net"
    assert result["regime_review"]["nav_basis"] == "operating_net"
    assert result["regime_review"]["requested_nav_basis_available"] is True
    assert result["operating_costs"]["daily_nav"][1]["nav_usd"]["paper"] == "970.95000000"
    modeled = result["model_conditional_inference"]
    assert modeled["nav_basis"] == "operating_net"
    assert modeled["comparison_id"] == "index-panel"
    assert modeled["nominal_two_sided_confidence_under_model"] == "0.975000"
    assert modeled["mean_daily_excess_bps"] == result["comparisons"][0]["mean_daily_excess_bps"]
    assert (
        modeled["mean_daily_excess_bps"]
        != result["execution_net_comparisons"][0]["mean_daily_excess_bps"]
    )
    cost_diagnostics = result["execution_cost_diagnostics"]
    assert cost_diagnostics["gross_nav"]["status"] == "unavailable"
    assert cost_diagnostics["daily_nav"][1] == {
        "day": "2026-10-02",
        "execution_net_nav_usd": "1000.95000000",
        "cumulative_recorded_fill_fees_usd": "0.05",
        "nav_before_recorded_execution_fees_usd": "1001.00000000",
    }
    # The full-window limit catches the earlier funding loss even though the
    # comparison window starts after the charge and shows a smaller drawdown.
    assert float(result["comparisons"][0]["risk"]["strategy"]["max_drawdown_bps"]) < 250
    index["modeled_entry_cost_bps"] += 1
    changed = msgspec.json.encode(index)
    benchmark_path.write_bytes(changed)
    assert forward_cli(tmp_path, frozen, raw, extra=inputs) == 2
    assert "Paper benchmark SHA-256" in capsys.readouterr().err
    assert (
        forward_cli(
            tmp_path, frozen, raw, extra=(*inputs[:3], sha256(changed).hexdigest(), *inputs[4:])
        )
        == 2
    )
    assert "registered settings" in capsys.readouterr().err


def report():
    first = date(2026, 10, 1)
    daily = []
    for offset, price in enumerate([None, "101", "103", "98", "102"]):
        day = (first + timedelta(days=offset)).isoformat()
        cutoff = f"{day}T23:59:59.999999999Z"
        mark = {
            "mark_id": f"mark-{offset}",
            "instrument": INSTRUMENT,
            "price_usd": price,
            "observed_at": f"{day}T23:59:10Z",
            "available_at": f"{day}T23:59:20Z",
        }
        daily.append(
            {
                "day": day,
                "cutoff": cutoff,
                "account_version": 1 if offset == 0 else 3,
                "last_event_id": ACCOUNT if offset == 0 else "22222222-2222-4222-8222-222222222222",
                "ledger_prefix_sha256": ("a" if offset == 0 else "b") * 64,
                "cash_usd": "1000" if offset == 0 else "899.95",
                "recorded_fees_usd": "0" if offset == 0 else "0.05",
                "nav_usd": "1000.00000000" if offset == 0 else f"{899 + int(price)}.95000000",
                "positions": []
                if offset == 0
                else [
                    {
                        "instrument": INSTRUMENT,
                        "quantity_raw": "100",
                        "mark": mark,
                    }
                ],
            }
        )
    return {
        "schema_version": "0.1",
        "status": "complete",
        "account_id": ACCOUNT,
        "input_sha256": "c" * 64,
        "valuation_source": {"kind": "synthetic_fixture", "id": "test-marks"},
        "mark_max_age_seconds": 90,
        "nav_basis": "execution_net",
        "external_costs_usd": None,
        "mark_policy": "last_locally_available_observation_at_utc_day_end",
        "ledger_hash_policy": "recorded_event_json_lines_v1",
        "initial_funding": {"cash_usd": "1000", "recorded_at": "2026-10-01T12:00:00Z"},
        "daily_nav": daily,
    }


def freeze(raw, report, benchmark_raw=None):
    doc = {
        "schema_version": "0.2",
        "experiment_id": "paper-cash",
        "configuration_sha256": "d" * 64,
        "paper_nav_sha256": sha256(raw).hexdigest(),
        "source_mode": "synthetic_fixture",
        "nav_basis": "execution_net",
        "model_training_cutoff": None,
        "expected_nav_dates": [row["day"] for row in report["daily_nav"]],
        "primary_comparison_id": "cash-panel",
        "splits": {
            "train_start": "2026-10-01",
            "validation_start": "2026-10-02",
            "test_start": "2026-10-03",
            "end_exclusive": "2026-10-06",
            "embargo_calendar_days": 0,
        },
        "comparisons": [
            {
                "comparison_id": "cash-panel",
                "strategy": "paper",
                "benchmark": "uninvested-cash",
                "benchmark_access": "synthetic_fixture",
                "minimum_daily_excess_bps": 0,
            }
        ],
        "bootstrap": {
            "block_marks": 2,
            "minimum_blocks": 2,
            "replications": 100,
            "seed": 8,
            "family_alpha_bps": 500,
        },
    }
    if benchmark_raw is not None:
        doc["paper_benchmark_sha256"] = sha256(benchmark_raw).hexdigest()
        doc["primary_comparison_id"] = "index-panel"
        doc["comparisons"].insert(
            0,
            {
                "comparison_id": "index-panel",
                "strategy": "paper",
                "benchmark": "fixture-index",
                "benchmark_access": "synthetic_fixture",
                "minimum_daily_excess_bps": 0,
            },
        )
    protocol = load_protocol(json.dumps(doc).encode())
    return protocol, protocol_sha256(protocol)


def test_generate_aligned_cash_benchmark_and_reproduce_statistics():
    doc = report()
    raw = json.dumps(doc).encode()
    protocol, frozen = freeze(raw, doc)
    evidence = build_paper_evidence(protocol, raw, expected_protocol_sha256=frozen)
    assert evidence.daily_nav[-1].nav_usd == {"paper": "1001.95000000", "uninvested-cash": "1000"}
    result = evaluate_statistics(protocol, evidence, expected_protocol_sha256=frozen)
    assert result["comparisons"][0]["status"] == "inconclusive"
    assert result["alpha_measured"] is False
    assert result["comparisons"][0]["benchmark_total_return_bps"] == "0.000000"
    with pytest.raises(ValueError, match="paper NAV SHA-256"):
        build_paper_evidence(protocol, raw + b" ", expected_protocol_sha256=frozen)


def test_recorded_fees_diagnostic_keeps_exact_dates_and_execution_net_marks():
    doc = report()
    for row in doc["daily_nav"][1:]:
        row["recorded_fees_usd"] = "0.05000001"
        row["cash_usd"] = "899.94999999"
        row["nav_usd"] = row["nav_usd"].replace(".95000000", ".94999999")
    raw = msgspec.json.encode(doc)
    protocol, frozen = freeze(raw, doc)
    evidence, costs = build_paper_evidence_with_cost_diagnostics(
        protocol, raw, expected_protocol_sha256=frozen
    )
    assert evidence == build_paper_evidence(protocol, raw, expected_protocol_sha256=frozen)
    assert costs["basis"] == "execution_net_plus_recorded_fill_fees"
    assert costs["gross_nav"] == {
        "status": "unavailable",
        "reason": "independent_execution_reference_missing",
    }
    rows = costs["daily_nav"]
    assert [row["day"] for row in rows] == [row["day"] for row in doc["daily_nav"]]
    assert rows[0]["nav_before_recorded_execution_fees_usd"] == "1000.00000000"
    assert rows[-1] == {
        "day": "2026-10-05",
        "execution_net_nav_usd": "1001.94999999",
        "cumulative_recorded_fill_fees_usd": "0.05000001",
        "nav_before_recorded_execution_fees_usd": "1002.00000000",
    }


def benchmark(report):
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "series_id": "fixture-index",
        "basis": "usd_total_return_index",
        "modeled_entry_cost_bps": 50,
        "max_age_seconds": 86490,
        "levels": [
            {
                "day": day["day"],
                "observed_at": f"{day['day']}T23:59:10Z",
                "available_at": f"{day['day']}T23:59:20Z",
                "level": level,
            }
            for day, level in zip(
                report["daily_nav"], ["100", "110", "123.45678913", "100", "105"], strict=True
            )
        ],
    }


def test_paper_evidence_normalizes_pinned_total_returns_and_modeled_entry_friction():
    doc = report()
    raw = json.dumps(doc).encode()
    levels = benchmark(doc)
    levels["levels"][3] = levels["levels"][2] | {"day": levels["levels"][3]["day"]}
    benchmark_raw = json.dumps(levels).encode()
    protocol, frozen = freeze(raw, doc, benchmark_raw)
    evidence = build_paper_evidence(
        protocol, raw, expected_protocol_sha256=frozen, benchmark_raw=benchmark_raw
    )
    assert [mark.nav_usd["fixture-index"] for mark in evidence.daily_nav] == [
        "1000",
        "1094.50000000",
        "1228.39505184",
        "1228.39505184",
        "1044.75000000",
    ]
    assert [mark.nav_usd["uninvested-cash"] for mark in evidence.daily_nav] == ["1000"] * 5
    result = evaluate_statistics(protocol, evidence, expected_protocol_sha256=frozen)
    assert result["alpha_measured"] is False
    assert result["comparisons"][0]["benchmark_access_declared"] == "synthetic_fixture"
    assert (
        result["paper_nav_provenance_declared"]["paper_benchmark_sha256"]
        == sha256(benchmark_raw).hexdigest()
    )
    assert result["comparisons"][0]["benchmark_total_return_bps"] == "-1494.999932"


@pytest.mark.parametrize("access", ["theoretical_reference", "unknown"])
def test_archived_paper_benchmark_preserves_unknown_market_evidence(access):
    nav = report()
    synthetic, _ = freeze(msgspec.json.encode(nav), nav, msgspec.json.encode(benchmark(nav)))
    nav["valuation_source"]["kind"] = "local_archive"
    index = benchmark(nav) | {
        "source_mode": "local_archive",
        "archive_source_id": "fixture-archive",
    }
    nav_raw, index_raw = msgspec.json.encode(nav), msgspec.json.encode(index)
    document = msgspec.to_builtins(synthetic)
    document.update(
        source_mode="retrospective",
        paper_nav_sha256=sha256(nav_raw).hexdigest(),
        paper_benchmark_sha256=sha256(index_raw).hexdigest(),
    )
    document["comparisons"][0]["benchmark_access"] = access
    protocol = load_protocol(msgspec.json.encode(document))
    pin = protocol_sha256(protocol)
    evidence = build_paper_evidence(
        protocol, nav_raw, expected_protocol_sha256=pin, benchmark_raw=index_raw
    )
    result = evaluate_statistics(protocol, evidence, expected_protocol_sha256=pin)
    assert evidence.daily_nav[-1].nav_usd["fixture-index"] == "1044.75000000"
    assert result["comparisons"][0]["benchmark_access_declared"] == access
    assert (
        result["paper_nav_provenance_declared"]["paper_benchmark_archive_source_id"]
        == "fixture-archive"
    )
    assert result["verification_scope"]["benchmark_access_verified"] is False
    assert result["alpha_measured"] is False
    missing_archive = msgspec.structs.replace(
        evidence,
        paper_nav_provenance=msgspec.structs.replace(
            evidence.paper_nav_provenance,
            paper_benchmark_archive_source_id=msgspec.UNSET,
        ),
    )
    with pytest.raises(ValueError, match="archived source identity"):
        evaluate_statistics(protocol, missing_archive, expected_protocol_sha256=pin)
    document["comparisons"][0]["benchmark_access"] = "synthetic_fixture"
    with pytest.raises(ValueError, match="Paper benchmark"):
        load_protocol(msgspec.json.encode(document))


def test_paper_benchmark_archive_identity_matches_its_source_mode():
    synthetic = benchmark(report())
    with pytest.raises(ValueError, match="archive identity"):
        load_paper_benchmark(msgspec.json.encode(synthetic | {"source_mode": "local_archive"}))
    with pytest.raises(ValueError, match="archive identity"):
        load_paper_benchmark(msgspec.json.encode(synthetic | {"archive_source_id": "fixture"}))


def test_forward_archived_benchmark_requires_the_registered_source_and_complete_days(
    tmp_path, capsys
):
    prefix = forward_registration(tmp_path, with_index=True)
    freeze_path, protocol_path = map(Path, prefix[1:3])
    previous = load_forward_freeze(freeze_path.read_bytes())
    document = msgspec.json.decode(protocol_path.read_bytes())
    document["source_mode"] = "retrospective"
    document["comparisons"][0]["benchmark_access"] = "theoretical_reference"
    source = document["paper_forward_source"]
    source["valuation_source"]["kind"] = "local_archive"
    source["benchmark"]["archive_source_id"] = "fixture-archive"
    protocol = load_protocol(msgspec.json.encode(document))
    frozen = freeze_forward(tmp_path, previous.configuration.plan, protocol)
    protocol_path.write_bytes(msgspec.json.encode(protocol, order="deterministic"))
    freeze_path.write_bytes(msgspec.json.encode(frozen, order="deterministic"))
    prefix[-1] = forward_freeze_sha256(frozen)
    nav = report()
    nav["valuation_source"]["kind"] = "local_archive"
    index = benchmark(nav) | {
        "source_mode": "local_archive",
        "archive_source_id": "fixture-archive",
    }
    index_path = tmp_path / "archived-benchmark.json"

    def render(document):
        raw = msgspec.json.encode(document)
        index_path.write_bytes(raw)
        return forward_cli(
            tmp_path,
            prefix,
            msgspec.json.encode(nav),
            extra=("--benchmark", str(index_path), "--benchmark-sha256", sha256(raw).hexdigest()),
        )

    assert render(index) == 0
    result = msgspec.json.decode(capsys.readouterr().out)
    assert result["comparisons"][0]["benchmark_access_declared"] == "theoretical_reference"
    assert (
        result["paper_nav_provenance_declared"]["paper_benchmark_archive_source_id"]
        == "fixture-archive"
    )
    assert result["alpha_measured"] is False

    for altered, error in (
        (index | {"archive_source_id": "substituted-archive"}, "registered settings"),
        (index | {"levels": index["levels"][:-1]}, "cover every frozen"),
        (benchmark(nav), "source mode"),
    ):
        assert render(altered) == 2
        assert error in capsys.readouterr().err


@pytest.mark.parametrize(
    "reason",
    [
        "missing_day",
        "after_cutoff",
        "stale",
        "backdated_revision",
        "restated_level",
        "zero_level",
        "subprecision_nav",
        "wrong_series",
    ],
)
def test_paper_evidence_rejects_benchmark_availability_and_identity_mismatches(reason):
    doc = report()
    raw = json.dumps(doc).encode()
    levels = benchmark(doc)
    marks = levels["levels"]
    if reason == "missing_day":
        del marks[2]
    elif reason == "after_cutoff":
        marks[2]["available_at"] = "2026-10-04T00:00:00Z"
    elif reason == "stale":
        levels["max_age_seconds"] = 49
    elif reason == "backdated_revision":
        marks[2]["observed_at"] = "2026-10-02T23:59:40Z"
        marks[2]["available_at"] = "2026-10-02T23:59:50Z"
    elif reason == "restated_level":
        marks[2]["observed_at"] = marks[1]["observed_at"]
        marks[2]["available_at"] = marks[1]["available_at"]
    elif reason == "zero_level":
        marks[2]["level"] = "0"
    elif reason == "subprecision_nav":
        marks[0]["level"] = "10000000000000000000000000000"
        marks[2]["level"] = "0.00000001"
    else:
        levels["series_id"] = "different-index"
    benchmark_raw = json.dumps(levels).encode()
    protocol, frozen = freeze(raw, doc, benchmark_raw)
    with pytest.raises(ValueError, match="benchmark"):
        build_paper_evidence(
            protocol, raw, expected_protocol_sha256=frozen, benchmark_raw=benchmark_raw
        )


def test_paper_evidence_requires_benchmark_bytes_only_when_pinned():
    doc = report()
    raw = json.dumps(doc).encode()
    benchmark_raw = json.dumps(benchmark(doc)).encode()
    protocol, frozen = freeze(raw, doc, benchmark_raw)
    with pytest.raises(ValueError, match="benchmark"):
        build_paper_evidence(protocol, raw, expected_protocol_sha256=frozen)
    with pytest.raises(ValueError, match="benchmark"):
        build_paper_evidence(
            protocol, raw, expected_protocol_sha256=frozen, benchmark_raw=benchmark_raw + b" "
        )
    protocol, frozen = freeze(raw, doc)
    with pytest.raises(ValueError, match="benchmark"):
        build_paper_evidence(
            protocol, raw, expected_protocol_sha256=frozen, benchmark_raw=benchmark_raw
        )


def test_published_total_return_schema_and_cli(capsys):
    service = Path(__file__).resolve().parents[1]
    root = service.parents[1]
    examples = service / "examples"
    schema = json.loads((root / "contracts/paper-benchmark.schema.json").read_bytes())
    assert schema == {"$schema": "https://json-schema.org/draft/2020-12/schema"} | (
        msgspec.json.schema(PaperBenchmark)
    )
    protocol_path = examples / "paper-total-return-protocol.json"
    protocol = load_protocol(protocol_path.read_bytes())
    assert (
        main(
            [
                "paper-evidence",
                str(protocol_path),
                str(examples / "paper-cash-nav.json"),
                "--benchmark-input",
                str(examples / "paper-total-return-benchmark.json"),
                "--protocol-sha256",
                protocol_sha256(protocol),
            ]
        )
        == 0
    )
    evidence = json.loads(capsys.readouterr().out)
    assert evidence["daily_nav"][-1]["nav_usd"]["fixture-index"] == "1044.75000000"
    assert evidence["paper_nav_provenance"]["paper_benchmark_sha256"] == (
        protocol.paper_benchmark_sha256
    )


@pytest.mark.parametrize(
    "reason", ["missing", "future_mark", "balance_mismatch", "old_mark", "changed_version"]
)
def test_paper_evidence_refuses_partial_or_inconsistent_ledgers(reason):
    doc = report()
    if reason == "missing":
        doc["status"] = "incomplete"
        doc["daily_nav"][2]["nav_usd"] = None
    elif reason == "future_mark":
        doc["daily_nav"][2]["positions"][0]["mark"]["available_at"] = "2026-10-04T00:00:00Z"
    elif reason == "old_mark":
        doc["daily_nav"][2]["positions"][0]["mark"]["observed_at"] = "2026-10-03T23:00:00Z"
    elif reason == "changed_version":
        doc["daily_nav"][2]["ledger_prefix_sha256"] = "f" * 64
    else:
        doc["daily_nav"][2]["nav_usd"] = "1003.00000000"
    raw = json.dumps(doc).encode()
    protocol, frozen = freeze(raw, doc)
    with pytest.raises(ValueError):
        build_paper_evidence(protocol, raw, expected_protocol_sha256=frozen)
