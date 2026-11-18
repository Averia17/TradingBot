import json
from copy import deepcopy

import msgspec
import pytest
from test_identity_replay_evidence import inputs as replay_inputs
from test_research_replay import digest, encode, load

from tradingbot_evaluation.statistics import (
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)
from tradingbot_evaluation.walk_forward_evidence import derive_walk_forward_evidence


def evidence_inputs():
    replay = replay_inputs()
    records = json.loads(replay.export)["jobs"]
    jobs = [load(record, "request") for record in records]
    steps = [
        {
            "as_of": job["portfolio"]["as_of"],
            "completed_at": record["completed_at"],
            "requested_symbols": job["requested_symbols"],
            "security_bindings": job["security_bindings"],
            "max_llm_calls": job["max_llm_calls"],
            "data_manifest": job["data_manifest"],
        }
        for record, job in zip(records, jobs, strict=True)
    ]
    plan = {
        "schema_version": "0.2",
        "source_mode": "synthetic_fixture",
        "experiment_id": replay.document["experiment_id"],
        "policy_version": jobs[0]["policy_version"],
        "max_position_pct": jobs[0]["portfolio"]["max_position_pct"],
        "max_total_llm_calls": sum(row["max_llm_calls"] for row in steps),
        "identity_registry": jobs[0]["identity_registry"],
        "steps": steps,
    }
    replay_source = replay.document.pop("replay_source")
    replay.document["walk_forward_source"] = {
        "plan_sha256": digest(encode(plan)),
        "tape_sha256": digest(replay.tape),
        "identity_registry_sha256": digest(replay.registry),
        "mark_max_age_seconds": replay_source["identity"]["mark_max_age_seconds"],
        "benchmarks": replay_source["benchmarks"],
    }
    return replay.document, plan, replay.tape, replay.registry, [load(r, "result") for r in records]


def recorded_inference(outputs):
    remaining = iter(deepcopy(outputs))

    def infer(raw):
        request = json.loads(raw)
        result = next(remaining)
        job = request["job"]
        result["batch"].update(
            portfolio_snapshot_id=job["portfolio"]["snapshot_id"],
            as_of=job["portfolio"]["as_of"],
            created_at=request["completed_at"],
        )
        result["metadata"]["provenance"].update(job_id=job["job_id"])
        return encode(result)

    return infer


def test_frozen_identity_walk_evidence_uses_the_same_native_marks_and_persists_registry():
    document, plan, tape_raw, registry, outputs = evidence_inputs()
    protocol = load_protocol(encode(document))
    frozen = protocol_sha256(protocol)

    def derive():
        return derive_walk_forward_evidence(
            protocol,
            encode(plan),
            tape_raw,
            expected_protocol_sha256=frozen,
            identity_registry_raw=registry,
            infer=recorded_inference(outputs),
        )

    evidence = derive()
    assert msgspec.json.encode(evidence) == msgspec.json.encode(derive())
    assert [row.day for row in evidence.daily_nav] == list(protocol.expected_nav_dates)
    assert [row.nav_usd["fixture-strategy"] for row in evidence.daily_nav] == [
        "1000.00",
        "1006.00",
        "1006.00",
        "1026.00",
    ]
    assert [row.nav_usd["stock-hold"] for row in evidence.daily_nav] == [
        "1000.00",
        "1000.00",
        "1400.00",
        "1600.00",
    ]
    proof = msgspec.to_builtins(evidence)
    assert proof["walk_forward_provenance"]["identity_registry_sha256"] == digest(registry)
    report = evaluate_statistics(
        protocol, load_evidence(encode(proof)), expected_protocol_sha256=frozen
    )
    assert report["walk_forward_provenance_declared"]["identity_registry_sha256"] == digest(
        registry
    )
    del proof["walk_forward_provenance"]["identity_registry_sha256"]
    with pytest.raises(ValueError, match="frozen registry SHA-256"):
        evaluate_statistics(protocol, load_evidence(encode(proof)), expected_protocol_sha256=frozen)


@pytest.mark.parametrize(
    "changed", ["missing-file", "file-bytes", "plan-pin", "tape-ref", "quote-window"]
)
def test_frozen_identity_walk_refuses_mixed_or_tampered_sources_before_inference(changed):
    document, plan, tape_raw, registry, _ = evidence_inputs()
    source = document["walk_forward_source"]
    error = "registry"
    if changed == "missing-file":
        registry = None
    elif changed == "file-bytes":
        registry += b"\n"
    elif changed == "plan-pin":
        plan["identity_registry"]["sha256"] = "f" * 64
        source["plan_sha256"] = digest(encode(plan))
    else:
        tape = json.loads(tape_raw)
        if changed == "tape-ref":
            tape["identity_registry"]["ref"] = "renamed-registry.json"
        else:
            price = next(q for q in tape["replay"]["quotes"] if q["symbol"] == "BMK")
            tape["replay"]["quotes"].append(
                price
                | {
                    "observed_at": "2026-09-30T12:00:00Z",
                    "available_at": "2026-09-30T12:00:00Z",
                }
            )
            error = "unambiguous point-in-time eligibility"
        tape_raw = encode(tape)
        source["tape_sha256"] = digest(tape_raw)
    protocol = load_protocol(encode(document))
    with pytest.raises(ValueError, match=error):
        derive_walk_forward_evidence(
            protocol,
            encode(plan),
            tape_raw,
            expected_protocol_sha256=protocol_sha256(protocol),
            identity_registry_raw=registry,
            infer=lambda _: pytest.fail("invalid inputs reached research"),
        )
