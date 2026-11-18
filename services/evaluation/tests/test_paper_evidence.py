import json
from datetime import date, timedelta
from hashlib import sha256
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.paper_evidence import PaperBenchmark, build_paper_evidence
from tradingbot_evaluation.statistics import evaluate_statistics, load_protocol, protocol_sha256

ACCOUNT = "11111111-1111-4111-8111-111111111111"
INSTRUMENT = {"instrument_id": "fixture:TSLA", "registry_version": "fixture-v1", "decimals": 2}


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
