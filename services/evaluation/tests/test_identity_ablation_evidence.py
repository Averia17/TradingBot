import json
from copy import deepcopy

import msgspec
import pytest
from test_identity_walk_forward_evidence import evidence_inputs, recorded_inference
from test_research_replay import digest, encode

from tradingbot_evaluation.ablation_evidence import derive_ablation_evidence
from tradingbot_evaluation.statistics import (
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)


def paired_inputs():
    document, plan, tape, registry, outputs = evidence_inputs()
    source = document.pop("walk_forward_source")
    del source["benchmarks"]
    source.update(
        baseline="full",
        variants={"full": "full_graph", "without-memory": "no_memory"},
        max_total_llm_calls=2 * sum(step["max_llm_calls"] for step in plan["steps"]),
    )
    document["ablation_source"] = source
    document["comparisons"] = [
        document["comparisons"][0] | {"strategy": "without-memory", "benchmark": "full"}
    ]
    document["primary_comparison_id"] = document["comparisons"][0]["comparison_id"]
    return document, encode(plan), tape, registry, outputs


def test_stable_ablation_pairs_native_daily_nav_and_persists_shared_registry():
    document, plan, tape, registry, outputs = paired_inputs()
    protocol = load_protocol(encode(document))
    frozen = protocol_sha256(protocol)
    seen = []

    def infer_for(variant):
        results = deepcopy(outputs)
        if variant == "no_memory":
            for result in results:
                result["batch"]["intents"][0].update(action="HOLD", notional_usd="0")
                result["metadata"]["provenance"]["config_sha256"] = "f" * 64
        infer = recorded_inference(results)

        def record_inputs(raw):
            job = json.loads(raw)["job"]
            seen.append((variant, job["data_manifest"], job["identity_registry"]))
            return infer(raw)

        return record_inputs

    evidence = derive_ablation_evidence(
        protocol,
        plan,
        tape,
        expected_protocol_sha256=frozen,
        infer_for=infer_for,
        identity_registry_raw=registry,
    )
    steps = len(outputs)
    assert [role for role, _, _ in seen] == ["full_graph"] * steps + ["no_memory"] * steps
    assert [row[1:] for row in seen[:steps]] == [row[1:] for row in seen[steps:]]
    assert [row.day for row in evidence.daily_nav] == list(protocol.expected_nav_dates)
    assert [row.nav_usd for row in evidence.daily_nav] == [
        {"full": full, "without-memory": "1000.00"}
        for full in ("1000.00", "1006.00", "1006.00", "1026.00")
    ]
    wire = msgspec.to_builtins(evidence)
    persisted = load_evidence(encode(wire))
    report = evaluate_statistics(protocol, persisted, expected_protocol_sha256=frozen)
    arms = report["ablation_provenance_declared"]["arms"]
    assert all(arm["walk"]["identity_registry_sha256"] == digest(registry) for arm in arms.values())
    for invalid in (None, "f" * 64):
        changed = deepcopy(wire)
        one_arm = changed["ablation_provenance"]["arms"]["without-memory"]["walk"]
        if invalid is None:
            del one_arm["identity_registry_sha256"]
        else:
            one_arm["identity_registry_sha256"] = invalid
        with pytest.raises(ValueError, match="frozen registry SHA-256"):
            evaluate_statistics(
                protocol, load_evidence(encode(changed)), expected_protocol_sha256=frozen
            )


@pytest.mark.parametrize("changed", ["missing-file", "missing-pin", "wrong-file", "tape", "budget"])
def test_ablation_rejects_mixed_sources_and_budget_before_constructing_any_arm(changed):
    document, plan, tape, registry, _ = paired_inputs()
    source = document["ablation_source"]
    error = "registry"
    if changed == "missing-file":
        registry = None
    elif changed == "missing-pin":
        del source["identity_registry_sha256"]
        registry = None
    elif changed == "wrong-file":
        registry += b"\n"
    elif changed == "tape":
        tape += b"\n"
        error = "tape SHA-256"
    elif changed == "budget":
        source["max_total_llm_calls"] -= 1
        error = "aggregate planned call budget"
    protocol = load_protocol(encode(document))
    with pytest.raises(ValueError, match=error):
        derive_ablation_evidence(
            protocol,
            plan,
            tape,
            expected_protocol_sha256=protocol_sha256(protocol),
            identity_registry_raw=registry,
            infer_for=lambda _: pytest.fail("Invalid study input reached an arm factory"),
        )
