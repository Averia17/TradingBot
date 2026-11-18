import json
from datetime import date, timedelta
from hashlib import sha256

import pytest

from tradingbot_evaluation.paper_evidence import build_paper_evidence
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


def freeze(raw, report):
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
