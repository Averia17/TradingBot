from copy import deepcopy
from hashlib import sha256
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.research_costs import ResearchPermitArchive, ResearchRates


def invoke(tmp_path, capsys, rate, *archives, template=None, permit_archives=()):
    def pinned(name, doc):
        raw = msgspec.json.encode(doc)
        path = tmp_path / name
        path.write_bytes(raw)
        return [str(path), sha256(raw).hexdigest()]

    args = ["research-costs", "--rates", *pinned("rates.json", rate)]
    for index, archive in enumerate(archives):
        args.extend(["--usage", *pinned(f"usage-{index}.json", archive)])
    for index, archive in enumerate(permit_archives):
        args.extend(["--permit-archive", *pinned(f"permit-{index}.json", archive)])
    if template is not None:
        args.extend(
            ["--synthetic-opex-template", *pinned("template.json", template), "--series", "paper"]
        )
    status = main(args)
    result = capsys.readouterr()
    if status != 0:
        return status, result.err
    return status, msgspec.json.decode(result.out)


@pytest.fixture
def archive():
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "llm_calls": 1,
        "capture_status": "complete",
        "calls": [
            {
                "run_id": "10000000-0000-4000-8000-000000000001",
                "provider": "fixture",
                "model": "model-a",
                "role": "research",
                "status": "completed",
                "started_at": "2026-11-18T12:20:00Z",
                "input_tokens": 1000,
                "output_tokens": 200,
                "cache_read_tokens": 500,
                "cache_write_tokens": 100,
                "reasoning_tokens": 25,
                "usage_source": "langchain_usage_metadata",
            },
        ],
    }


@pytest.fixture
def rates():
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "currency": "USD",
        "rates": [
            {
                "rate_id": "fixture-november",
                "provider": "fixture",
                "model": "model-a",
                "valid_from": "2026-11-01",
                "end_exclusive": "2026-12-01",
                "uncached_input_usd_per_million": "2",
                "cache_read_usd_per_million": "0.5",
                "cache_write_usd_per_million": "4",
                "output_usd_per_million": "10",
                "reasoning_usd_per_million": "12",
                "fixed_provider_fee_usd": "0.0001",
            },
        ],
    }


def test_value_calls_with_inclusive_token_counts_and_failed_attempts(
    tmp_path, capsys, archive, rates
):
    retried = deepcopy(archive)
    retried["calls"][0].update(
        run_id="10000000-0000-4000-8000-000000000002", status="failed", cache_write_tokens=0
    )
    code, report = invoke(tmp_path, capsys, rates, archive, retried)
    assert code == 0
    assert report["pricing_basis"] == "schedule_estimate"
    # (400*2 + 500*.5 + 100*4 + 175*10 + 25*12) / 1M + .0001.
    assert report["archives"][0]["estimated_cost_usd"] == "0.0036"
    assert report["archives"][1]["estimated_cost_usd"] == "0.0034"
    assert report["estimated_cost_usd"] == "0.007"
    assert report["complete_archive_p95_usd"] == "0.0036"
    assert report["actual_billed_cost_usd"] is None


def test_absent_token_categories_and_absent_rates_remain_unknown(tmp_path, capsys, archive, rates):
    archive["calls"][0]["cache_read_tokens"] = None
    code, report = invoke(tmp_path, capsys, rates, archive)
    assert code == 0
    assert report["estimated_cost_usd"] is None
    assert report["archives"][0]["calls"][0]["unknown"] == ["cache_read_tokens"]
    rates["rates"][0]["cache_read_usd_per_million"] = "2"
    assert invoke(tmp_path, capsys, rates, archive)[1]["estimated_cost_usd"] == "0.00435"
    rates["rates"][0]["fixed_provider_fee_usd"] = None
    partial = invoke(tmp_path, capsys, rates, archive)[1]["archives"][0]["calls"][0]
    assert partial["estimated_token_cost_usd"] == "0.00425"
    assert partial["estimated_cost_usd"] is None
    assert partial["unknown"] == ["provider_fees"]
    rates["rates"][0]["valid_from"] = "2026-11-19"
    assert invoke(tmp_path, capsys, rates, archive)[1]["archives"][0]["calls"][0]["unknown"] == [
        "rate_not_found",
    ]


def test_reject_duplicate_invocations_and_overlapping_rate_windows(
    tmp_path, capsys, archive, rates
):
    code, error = invoke(tmp_path, capsys, rates, archive, archive)
    assert code == 2
    assert "duplicate" in error.lower()
    rates["rates"].append(rates["rates"][0] | {"rate_id": "second"})
    code, error = invoke(tmp_path, capsys, rates, archive)
    assert code == 2
    assert "overlap" in error.lower()


def test_estimate_flows_only_to_synthetic_opex_without_double_count(
    tmp_path, capsys, archive, rates
):
    template = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "basis": "additional_to_execution_net",
        "first_mark_day": "2026-11-17",
        "last_mark_day": "2026-11-20",
        "expenses": [],
        "coverage": [],
    }
    code, report = invoke(tmp_path, capsys, rates, archive, template=template)
    assert code == 0
    ledger = report["synthetic_operating_costs"]
    assert ledger["coverage"][0]["status"] == "complete"
    assert ledger["expenses"][0]["amount_usd"] == "0.00360000"
    assert ledger["expenses"][0]["category"] == "llm"
    assert report["actual_billed_cost_usd"] is None
    code, message = invoke(tmp_path, capsys, rates, archive, template=ledger)
    assert (code, "contains LLM" in message) == (2, True)
    archive["capture_status"] = "partial"
    result = invoke(tmp_path, capsys, rates, archive, template=template)[1]
    assert result["synthetic_operating_costs"]["coverage"][0]["status"] == "unknown"
    assert result["estimated_cost_usd"] is None
    template["source_mode"] = "archived_local"
    code, message = invoke(tmp_path, capsys, rates, archive, template=template)
    assert (code, "require synthetic" in message) == (2, True)


def test_archive_validation_and_rate_wire_schema(tmp_path, capsys, archive, rates):
    archive["calls"][0]["cache_write_tokens"] = 501
    code, message = invoke(tmp_path, capsys, rates, archive)
    assert code == 2
    assert "inclusive token total" in message
    rates_path = tmp_path / "rates.json"
    assert (
        main(
            [
                "research-costs",
                "--rates",
                str(rates_path),
                "a" * 64,
                "--usage",
                str(tmp_path / "usage-0.json"),
                "b" * 64,
            ]
        )
        == 2
    )
    assert "rate schedule SHA-256" in capsys.readouterr().err
    root = Path(__file__).resolve().parents[3]
    schema_path = root / "contracts" / "research-model-rates.schema.json"
    assert msgspec.json.decode(schema_path.read_bytes()) == {
        "$schema": "https://json-schema.org/draft/2020-12/schema"
    } | msgspec.json.schema(ResearchRates)


def test_shared_research_usage_fixture_prices_with_evaluation_cli(capsys):
    root = Path(__file__).resolve().parents[3]
    rate_path = root / "services/evaluation/examples/research-rate-schedule.json"
    usage_path = root / "contracts/valid-research-llm-usage.json"
    assert (
        main(
            [
                "research-costs",
                "--rates",
                str(rate_path),
                sha256(rate_path.read_bytes()).hexdigest(),
                "--usage",
                str(usage_path),
                sha256(usage_path.read_bytes()).hexdigest(),
            ]
        )
        == 0
    )
    result = msgspec.json.decode(capsys.readouterr().out)
    assert result["estimated_cost_usd"] == "0.00433"
    assert [call["status"] for call in result["archives"][0]["calls"]] == ["completed", "failed"]


def test_postgres_permit_archive_prices_new_attempt_without_assuming_old_expense_zero(
    tmp_path, capsys, archive, rates
):
    call = archive["calls"][0]
    call.update(provider="offline", model="scripted-offline-v1")
    rates["rates"][0].update(provider=call["provider"], model=call["model"])
    raw = msgspec.json.encode(call)
    permits = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "job_id": "30000000-0000-4000-8000-000000000001",
        "job_status": "completed",
        "attempts": 2,
        "legacy_budget_unknown": False,
        "permit_history_known": True,
        "permits": [
            {
                "call_id": "10000000-0000-4000-8000-000000000002",
                "attempt": 1,
                "granted_at": "2026-11-17T12:00:00Z",
                "callback_payload": None,
                "callback_sha256": None,
                "recorded_at": None,
            },
            {
                "call_id": call["run_id"],
                "attempt": 2,
                "granted_at": "2026-11-18T12:19:58Z",
                "callback_payload": msgspec.to_builtins(raw),
                "callback_sha256": sha256(raw).hexdigest(),
                "recorded_at": "2026-11-18T12:20:01Z",
            },
        ],
    }
    code, report = invoke(tmp_path, capsys, rates, permit_archives=(permits,))
    assert code == 0
    assert report["estimated_cost_usd"] is None
    assert report["fully_valued_calls_cost_usd"] == "0.0036"
    evidence = report["archives"][0]
    assert evidence["capture_status"] == "partial"
    assert evidence["permit_provenance"]["permits_without_callback"] == 1
    old, latest = evidence["calls"]
    assert old["status"] == "permit_without_callback"
    assert old["started_at"] is None
    assert old["unknown"] == ["missing_terminal_callback"]
    assert latest["permit"]["attempt"] == 2
    assert latest["started_at"] == call["started_at"]
    assert latest["cost_attribution_utc"] == "2026-11-18T12:19:58Z"
    permits["permits"][1]["callback_sha256"] = "0" * 64
    code, message = invoke(tmp_path, capsys, rates, permit_archives=(permits,))
    assert code == 2
    assert "callback SHA-256" in message
    permits["permits"] = []
    permits["permit_history_known"] = False
    code, report = invoke(tmp_path, capsys, rates, permit_archives=(permits,))
    assert code == 0
    assert report["archives"][0]["permit_provenance"]["permit_count"] == 0
    assert report["archives"][0]["capture_status"] == "partial"
    assert report["estimated_cost_usd"] is None
    permits["permit_history_known"] = True
    permits["attempts"] = 0
    code, message = invoke(tmp_path, capsys, rates, permit_archives=(permits,))
    assert code == 2
    assert "requires at least one recorded attempt" in message


def test_published_permit_archive_schema_and_partial_costs(capsys):
    root = Path(__file__).resolve().parents[3]
    schema = root / "contracts/research-permit-archive.schema.json"
    assert msgspec.json.decode(schema.read_bytes()) == {
        "$schema": "https://json-schema.org/draft/2020-12/schema"
    } | msgspec.json.schema(ResearchPermitArchive)
    rate_path = root / "services/evaluation/examples/research-rate-schedule.json"
    archive_path = root / "contracts/valid-research-permit-archive.json"
    assert (
        main(
            [
                "research-costs",
                "--rates",
                str(rate_path),
                sha256(rate_path.read_bytes()).hexdigest(),
                "--permit-archive",
                str(archive_path),
                sha256(archive_path.read_bytes()).hexdigest(),
            ]
        )
        == 0
    )
    result = msgspec.json.decode(capsys.readouterr().out)
    assert result["fully_valued_calls_cost_usd"] == "0.00373"
    assert result["estimated_cost_usd"] is None
    assert result["complete_archive_p95_usd"] is None


def test_provider_permit_requires_real_callback_and_archived_rate_schedule(
    tmp_path, capsys, archive, rates
):
    call = archive["calls"][0]
    call.update(provider="openai", model="gpt-6-sol", role="portfolio_allocator")
    rates["source_mode"] = "archived_schedule"
    rates["rates"][0].update(provider=call["provider"], model=call["model"])
    raw = msgspec.json.encode(call)
    permit_archive = {
        "schema_version": "0.1",
        "source_mode": "provider_reported",
        "job_id": "30000000-0000-4000-8000-000000000001",
        "job_status": "completed",
        "attempts": 1,
        "legacy_budget_unknown": False,
        "permit_history_known": True,
        "permits": [{
            "call_id": call["run_id"],
            "attempt": 1,
            "granted_at": "2026-11-18T12:19:58Z",
            "callback_payload": msgspec.to_builtins(raw),
            "callback_sha256": sha256(raw).hexdigest(),
            "recorded_at": "2026-11-18T12:20:01Z",
        }],
    }
    code, report = invoke(tmp_path, capsys, rates, permit_archives=(permit_archive,))
    assert code == 0
    assert report["source_mode"] == "provider_reported"
    assert report["estimated_cost_usd"] == "0.0036"
    assert report["actual_billed_cost_usd"] is None

    rates["source_mode"] = "synthetic_fixture"
    code, message = invoke(tmp_path, capsys, rates, permit_archives=(permit_archive,))
    assert code == 2
    assert "source differs" in message

    rates["source_mode"] = "archived_schedule"
    raw = msgspec.json.encode(call | {"provider": "offline", "model": "scripted-offline-v1"})
    permit_archive["permits"][0].update(
        callback_payload=msgspec.to_builtins(raw), callback_sha256=sha256(raw).hexdigest(),
    )
    code, message = invoke(tmp_path, capsys, rates, permit_archives=(permit_archive,))
    assert code == 2
    assert "does not match its provider permit" in message
