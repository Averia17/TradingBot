import json
from copy import deepcopy

import pytest
from test_identity_research_replay import identity_inputs
from test_research_replay import digest, encode, load

from tradingbot_evaluation.walk_forward import run_walk_forward


def _walk_fixture():
    records, tape, registry = identity_inputs()
    requests = [load(row, "request") for row in records]
    steps = [
        {
            "as_of": req["portfolio"]["as_of"],
            "completed_at": row["completed_at"],
            "requested_symbols": req["requested_symbols"],
            "security_bindings": req["security_bindings"],
            "max_llm_calls": req["max_llm_calls"],
            "data_manifest": req["data_manifest"],
        }
        for req, row in zip(requests, records, strict=True)
    ]
    return (
        {
            "schema_version": "0.2",
            "source_mode": "synthetic_fixture",
            "experiment_id": "stable-walk",
            "policy_version": requests[0]["policy_version"],
            "max_position_pct": requests[0]["portfolio"]["max_position_pct"],
            "max_total_llm_calls": sum(step["max_llm_calls"] for step in steps),
            "identity_registry": tape["identity_registry"],
            "steps": steps,
        },
        tape,
        registry,
        [load(row, "result") for row in records],
    )


def _run(plan, tape, registry, infer):
    plan_raw, tape_raw, registry_raw = map(encode, (plan, tape, registry))
    return run_walk_forward(
        plan_raw,
        tape_raw,
        infer=infer,
        expected_plan_sha256=digest(plan_raw),
        expected_tape_sha256=digest(tape_raw),
        identity_registry_raw=registry_raw,
        expected_identity_registry_sha256=digest(registry_raw),
        include_replay_input=True,
    )


def test_identity_walk_rebuilds_the_held_instrument_and_executes_on_the_new_key():
    plan, tape, registry, results = _walk_fixture()
    observed = []

    def infer(raw):
        request = json.loads(raw)
        job = request["job"]
        result = deepcopy(results[len(observed)])
        observed.append(job)
        result["batch"].update(
            portfolio_snapshot_id=job["portfolio"]["snapshot_id"],
            as_of=job["portfolio"]["as_of"],
            created_at=request["completed_at"],
        )
        result["metadata"]["provenance"].update(job_id=job["job_id"])
        return encode(result)

    report = _run(plan, tape, registry, infer)
    assert observed[1]["portfolio"]["positions"] == [
        {"symbol": "TEST", "instrument_id": "fixture:old", "quantity": "2", "mark_price_usd": "103"}
    ]
    assert observed[2]["portfolio"]["cash_usd"] == "1006.00"
    assert observed[2]["portfolio"]["positions"] == []
    assert all(job["job_schema_version"] == "0.2" for job in observed)
    assert [job["security_bindings"] for job in observed] == [
        step["security_bindings"] for step in plan["steps"]
    ]
    assert report["walk_forward_schema_version"] == "0.2"
    assert report["native_replay_input"]["identity_registry"] == tape["identity_registry"]
    assert report["open_positions"] == {"NEW": "2"} and report["ending_cash_usd"] == "906.00"


def test_identity_walk_refuses_inactive_security_before_its_analysis():
    plan, tape, registry, _ = _walk_fixture()
    plan["steps"][0]["security_bindings"][0]["security_id"] = "new-security"
    with pytest.raises(ValueError, match="registry instrument"):
        _run(plan, tape, registry, lambda _: pytest.fail("inactive issuer reached the graph"))


def test_identity_walk_refuses_a_terminal_mark_for_an_expired_holding():
    plan, tape, registry, results = _walk_fixture()
    plan["steps"] = plan["steps"][:1]

    def infer(raw):
        request = json.loads(raw)
        job = request["job"]
        result = results[0]
        result["batch"].update(
            portfolio_snapshot_id=job["portfolio"]["snapshot_id"],
            created_at=request["completed_at"],
        )
        result["metadata"]["provenance"].update(job_id=job["job_id"])
        return encode(result)

    with pytest.raises(ValueError, match="terminal .*holding"):
        _run(plan, tape, registry, infer)
