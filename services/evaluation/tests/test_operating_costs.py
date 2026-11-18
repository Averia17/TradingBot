import json
from datetime import date
from decimal import Decimal
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.operating_costs import (
    COST_CATEGORIES,
    apply_operating_costs,
    load_operating_costs,
    operating_costs_sha256,
)
from tradingbot_evaluation.replay import load_replay
from tradingbot_evaluation.replay_evidence import derive_replay_evidence
from tradingbot_evaluation.statistics import (
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)

EXAMPLES = Path(__file__).resolve().parents[1] / "examples"


def encode(document):
    return json.dumps(document).encode()


def expenses():
    return [
        {
            "cost_id": "api-1",
            "series_id": "fixture-strategy",
            "category": "llm",
            "incurred_at": "2026-10-02T10:00:00Z",
            "amount_usd": "0.12345678",
            "reference_id": "fixture-usage-1",
        },
        {
            "cost_id": "data-1",
            "series_id": "fixture-strategy",
            "category": "data",
            "incurred_at": "2026-10-03T10:00:00Z",
            "amount_usd": "5.00",
            "reference_id": "fixture-data-1",
        },
        {
            "cost_id": "compute-1",
            "series_id": "fixture-strategy",
            "category": "infrastructure",
            "incurred_at": "2026-10-04T00:30:00+03:00",
            "amount_usd": "0.05",
            "reference_id": "fixture-compute-1",
        },
    ]


def cost_document():
    entries = expenses()
    covered = {(entry["series_id"], entry["category"]) for entry in entries}
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "basis": "additional_to_execution_net",
        "first_mark_day": "2026-10-01",
        "last_mark_day": "2026-10-04",
        "expenses": entries,
        "coverage": [
            {
                "series_id": series,
                "category": category,
                "status": "complete" if (series, category) in covered else "zero",
                "reference_id": f"fixture-declaration.{series}.{category}",
            }
            for series in ("fixture-strategy", "usd-cash", "stock-hold", "universe")
            for category in COST_CATEGORIES
        ],
    }


def setup(costs):
    document = json.loads((EXAMPLES / "replay-evidence-protocol.json").read_bytes())
    document.update(
        schema_version="0.2",
        nav_basis="operating_net",
        operating_costs_sha256=operating_costs_sha256(costs),
    )
    protocol = load_protocol(encode(document))
    frozen = protocol_sha256(protocol)
    replay = load_replay((EXAMPLES / "replay-evidence-quotes.json").read_bytes())
    return protocol, derive_replay_evidence(protocol, replay, expected_protocol_sha256=frozen)


def test_operating_net_accounts_for_exact_costs_after_native_spread_and_commission():
    costs = load_operating_costs(encode(cost_document()))
    protocol, evidence = setup(costs)
    assert [row.nav_usd["fixture-strategy"] for row in evidence.daily_nav] == [
        "1000.00",
        "988.00",
        "990.00",
        "1017.80",
    ]
    result = evaluate_statistics(
        protocol,
        evidence,
        expected_protocol_sha256=protocol_sha256(protocol),
        operating_costs=costs,
    )
    assert result["comparisons_nav_basis"] == "operating_net"
    report = result["operating_costs"]
    assert report["sha256"] == operating_costs_sha256(costs)
    assert report["method"] == "cumulative_costs_subtracted_from_execution_net_marks"
    assert report["funding_and_execution_recomputed"] is False
    assert report["known_cost_usd_by_series"]["fixture-strategy"]["llm"] == "0.12345678"
    assert [row["nav_usd"]["fixture-strategy"] for row in report["daily_nav"]] == [
        "1000.00",
        "987.87654322",
        "984.82654322",
        "1012.62654322",
    ]
    assert [row["nav_usd"]["stock-hold"] for row in report["daily_nav"]] == [
        row.nav_usd["stock-hold"] for row in evidence.daily_nav
    ]
    assert result["execution_net_comparisons"] != result["comparisons"]
    assert (
        result["execution_net_comparisons"][0]["risk"]["strategy"]["mean_return_bps"]
        != result["comparisons"][0]["risk"]["strategy"]["mean_return_bps"]
    )
    assert (
        result["execution_net_comparisons"][0]["risk"]["benchmark"]
        == result["comparisons"][0]["risk"]["benchmark"]
    )
    assert result["verification_scope"]["costs_verified"] is False
    assert result["alpha_measured"] is False


@pytest.mark.parametrize(
    "gap", ["missing_coverage", "unknown_amount", "unknown_coverage", "missing_expense", "file"]
)
def test_missing_costs_preserve_execution_net_without_operating_returns(gap):
    document = cost_document()
    if gap == "missing_coverage":
        document["coverage"] = document["coverage"][1:]
    elif gap == "unknown_amount":
        document["expenses"][0]["amount_usd"] = None
    elif gap == "unknown_coverage":
        document["coverage"][0]["status"] = "unknown"
    elif gap == "missing_expense":
        document["expenses"].pop(0)
    costs = load_operating_costs(encode(document))
    protocol, evidence = setup(costs)
    result = evaluate_statistics(
        protocol,
        evidence,
        expected_protocol_sha256=protocol_sha256(protocol),
        operating_costs=None if gap == "file" else costs,
    )
    assert result["operating_costs"]["status"] == "unavailable"
    assert result["operating_costs"]["daily_nav"] is None
    assert result["comparisons_nav_basis"] == "execution_net"
    assert result["comparisons"] == result["execution_net_comparisons"]


def test_statistics_cli_rejects_tampered_cost_ledger_and_legacy_opex_stays_unverified(
    capsys, tmp_path
):
    document = cost_document()
    costs = load_operating_costs(encode(document))
    protocol, evidence = setup(costs)
    protocol_path = tmp_path / "protocol.json"
    evidence_path = tmp_path / "evidence.json"
    cost_path = tmp_path / "costs.json"
    protocol_path.write_bytes(msgspec.json.encode(protocol))
    evidence_path.write_bytes(msgspec.json.encode(evidence))
    cost_path.write_bytes(encode(document))
    assert main(["freeze-operating-costs", str(cost_path)]) == 0
    assert json.loads(capsys.readouterr().out)["operating_costs_sha256"] == (
        protocol.operating_costs_sha256
    )
    argv = [
        "statistics",
        str(protocol_path),
        str(evidence_path),
        "--protocol-sha256",
        protocol_sha256(protocol),
        "--operating-costs",
        str(cost_path),
    ]
    assert main(argv) == 0
    assert json.loads(capsys.readouterr().out)["comparisons_nav_basis"] == "operating_net"
    document["expenses"][0]["amount_usd"] = "0.12345679"
    cost_path.write_bytes(encode(document))
    assert main(argv) == 2
    assert "cost ledger SHA-256" in capsys.readouterr().err

    legacy = load_protocol((EXAMPLES / "statistics-protocol.json").read_bytes())
    result = evaluate_statistics(
        legacy,
        load_evidence((EXAMPLES / "statistics-evidence.json").read_bytes()),
        expected_protocol_sha256=protocol_sha256(legacy),
    )
    assert result["comparisons_nav_basis"] == "unverified_input"
    assert result["operating_costs"]["status"] == "unavailable"


def test_no_mark_on_incurred_day_and_costs_exhausting_nav():
    dates = (date(2026, 10, 1), date(2026, 10, 2), date(2026, 10, 4))
    document = cost_document()
    marks = {
        name: list(map(Decimal, ("1000.00", "998.00", "1010.00")))
        for name in ("fixture-strategy", "stock-hold", "universe", "usd-cash")
    }
    costs = load_operating_costs(encode(document))
    navs, _ = apply_operating_costs(dates, marks, operating_costs_sha256(costs), costs)
    assert navs["fixture-strategy"] == list(
        map(Decimal, ("1000.00", "997.87654322", "1004.82654322"))
    )
    document["expenses"][1]["amount_usd"] = "3000.00"
    costs = load_operating_costs(encode(document))
    navs, report = apply_operating_costs(dates, marks, operating_costs_sha256(costs), costs)
    assert navs is None
    assert report["blockers"] == [
        {
            "day": "2026-10-04",
            "reason": "non_positive_operating_nav",
            "series_id": "fixture-strategy",
        }
    ]
    assert marks["fixture-strategy"][-1] == Decimal("1010.00")


@pytest.mark.parametrize("category", ["spread", "slippage", "commission"])
def test_replay_native_execution_cost_categories_are_not_accepted_in_overlay(category):
    document = cost_document()
    document["expenses"][0]["category"] = category
    with pytest.raises(ValueError, match="category"):
        load_operating_costs(encode(document))


@pytest.mark.parametrize("change", ["duplicate", "window", "float", "zero"])
def test_invalid_cost_evidence_fails_before_adjusting_nav(change):
    document = cost_document()
    if change == "duplicate":
        document["expenses"].append(document["expenses"][0])
        with pytest.raises(ValueError, match="Duplicate cost ID"):
            load_operating_costs(encode(document))
    elif change == "window":
        document["expenses"][0]["incurred_at"] = "2026-10-01T12:00:00Z"
        with pytest.raises(ValueError, match="after the funding mark"):
            load_operating_costs(encode(document))
    elif change == "float":
        document["expenses"][0]["amount_usd"] = 0.125
        with pytest.raises(ValueError, match="amount_usd"):
            load_operating_costs(encode(document))
    else:
        document["coverage"][0]["status"] = "zero"
        costs = load_operating_costs(encode(document))
        protocol, evidence = setup(costs)
        with pytest.raises(ValueError, match="Zero cost coverage"):
            evaluate_statistics(
                protocol,
                evidence,
                expected_protocol_sha256=protocol_sha256(protocol),
                operating_costs=costs,
            )
