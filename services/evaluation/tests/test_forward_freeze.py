import json
from hashlib import sha256
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.forward_freeze import (
    forward_configuration_sha256,
    forward_freeze_sha256,
    freeze_forward,
    load_forward_freeze,
    load_forward_plan,
    verify_forward,
)
from tradingbot_evaluation.statistics import load_protocol, protocol_sha256

EXAMPLES = Path(__file__).resolve().parents[1] / "examples"
ROOT = Path(__file__).resolve().parents[3]
ROLES = (
    "strategy",
    "prompts",
    "model",
    "universe",
    "data",
    "risk",
    "schedule",
    "memory",
    "scorer",
    "costs",
    "runtime",
)


def _plan_doc():
    common = {role: [f"common/{role}.json"] for role in ROLES}
    return {
        "schema_version": "0.1",
        "experiment_id": "frozen-forward",
        "mode": "paper",
        "primary_strategy": "agent",
        "change_policy": "new_run_for_any_change",
        "insufficient_power": "continue_in_current_mode",
        "strategies": {
            "variant": {
                "model_id": "offline/analyst-2",
                "files": common | {"prompts": ["variants/control.txt"]},
            },
            "agent": {
                "model_id": "offline/analyst-1",
                "files": common | {"prompts": ["variants/agent.txt", "variants/shared.txt"]},
            },
        },
    }


def _load(data):
    return load_forward_plan(json.dumps(data).encode())


@pytest.fixture
def bundle(tmp_path):
    doc = _plan_doc()
    for strategy in doc["strategies"].values():
        for group in strategy["files"].values():
            for relative in group:
                artifact = tmp_path / relative
                artifact.parent.mkdir(parents=True, exist_ok=True)
                artifact.write_bytes(("private fixture contents for " + relative + "\n").encode())
    return tmp_path, doc


def _protocol_doc(root, plan):
    doc = json.loads((EXAMPLES / "statistics-protocol.json").read_bytes())
    doc.update(
        configuration_sha256=forward_configuration_sha256(root, plan),
        experiment_id=plan.experiment_id,
        nav_basis="execution_net",
        schema_version="0.2",
    )
    return doc


def _protocol(doc):
    return load_protocol(json.dumps(doc).encode())


def test_freeze_commits_strategy_files_protocol_and_challenger_membership(bundle):
    root, doc = bundle
    plan = _load(doc)
    protocol = _protocol(_protocol_doc(root, plan))
    assert protocol.configuration_sha256 == forward_configuration_sha256(root, plan)
    frozen = freeze_forward(root, plan, protocol)
    encoded = msgspec.json.encode(frozen)
    assert b"private fixture contents" not in encoded
    assert frozen.protocol_sha256 == protocol_sha256(protocol)
    assert frozen.primary_comparison_id == "main"
    assert frozen.comparison_ids == ("main", "variant")
    digest = forward_freeze_sha256(frozen)
    assert len(digest) == 64
    assert (
        verify_forward(root, load_forward_freeze(encoded), protocol, expected_sha256=digest)
        == frozen
    )


def test_cli_emits_exact_receipt_bytes_and_verifies_against_bundle(bundle, capsys):
    root, doc = bundle
    plan = root / "plan.json"
    plan.write_text(json.dumps(doc), encoding="utf-8")
    root_arg = ["--root", str(root)]
    assert main(["forward-config-sha256", str(plan), *root_arg]) == 0
    config_digest = json.loads(capsys.readouterr().out)["configuration_sha256"]
    protocol = root / "protocol.json"
    protocol.write_text(
        json.dumps(_protocol_doc(root, _load(doc)) | {"configuration_sha256": config_digest}),
        encoding="utf-8",
    )
    assert main(["freeze-forward", str(plan), str(protocol), *root_arg]) == 0
    receipt = root / "freeze.json"
    raw = capsys.readouterr().out.encode()
    receipt.write_bytes(raw)
    verify = [
        "verify-forward",
        str(receipt),
        str(protocol),
        *root_arg,
        "--freeze-sha256",
        sha256(raw).hexdigest(),
    ]
    assert main(verify) == 0
    assert json.loads(capsys.readouterr().out)["verified"] is True
    (root / "variants/agent.txt").write_text("different prompt", encoding="utf-8")
    assert main(verify) == 2
    assert "configuration SHA-256 differs" in capsys.readouterr().err


def test_map_order_is_stable_but_changing_artifact_order_changes_bundle(bundle):
    root, doc = bundle
    plan = _load(doc)
    protocol = _protocol(_protocol_doc(root, plan))
    frozen = freeze_forward(root, plan, protocol)
    doc["strategies"] = dict(reversed(list(doc["strategies"].items())))
    assert forward_freeze_sha256(freeze_forward(root, _load(doc), protocol)) == (
        forward_freeze_sha256(frozen)
    )
    doc["strategies"]["agent"]["files"]["prompts"].reverse()
    assert forward_configuration_sha256(root, _load(doc)) != frozen.configuration_sha256


def test_changed_bytes_and_wrong_expected_hash_fail_verification(bundle):
    root, doc = bundle
    plan = _load(doc)
    protocol = _protocol(_protocol_doc(root, plan))
    frozen = freeze_forward(root, plan, protocol)
    digest = forward_freeze_sha256(frozen)
    with pytest.raises(ValueError, match="expected forward freeze SHA-256"):
        verify_forward(root, frozen, protocol, expected_sha256="a" * 64)
    # Same length and logical role. Bytes within the previously frozen run differ.
    artifact = root / "variants/agent.txt"
    artifact.write_bytes(artifact.read_bytes().replace(b"agent", b"other"))
    with pytest.raises(ValueError, match="configuration SHA-256"):
        verify_forward(root, frozen, protocol, expected_sha256=digest)
    changed = freeze_forward(root, plan, _protocol(_protocol_doc(root, plan)))
    assert changed.configuration_sha256 != frozen.configuration_sha256
    assert forward_freeze_sha256(changed) != digest


def test_dropped_challenger_or_replaced_comparison_is_detected(bundle):
    root, plan_doc = bundle
    plan = _load(plan_doc)
    protocol_doc = _protocol_doc(root, plan)
    protocol = _protocol(protocol_doc)
    frozen = freeze_forward(root, plan, protocol)
    protocol_doc["comparisons"] = protocol_doc["comparisons"][:1]
    with pytest.raises(ValueError, match="registered strategies"):
        freeze_forward(root, plan, _protocol(protocol_doc))
    protocol_doc = _protocol_doc(root, plan)
    protocol_doc["primary_comparison_id"] = "variant"
    with pytest.raises(ValueError, match="primary comparison"):
        freeze_forward(root, plan, _protocol(protocol_doc))
    protocol_doc["primary_comparison_id"] = "main"
    protocol_doc["comparisons"][1]["minimum_daily_excess_bps"] += 1
    edited_protocol = _protocol(protocol_doc)
    with pytest.raises(ValueError, match="frozen evaluation protocol SHA-256"):
        verify_forward(root, frozen, edited_protocol, expected_sha256=forward_freeze_sha256(frozen))


def test_freeze_covers_the_paired_baseline_and_challenger(bundle):
    root, plan_doc = bundle
    plan = _load(plan_doc)
    protocol_doc = _protocol_doc(root, plan)
    protocol_doc["comparisons"] = [protocol_doc["comparisons"][0] | {"benchmark": "variant"}]
    protocol_doc["splits"]["end_exclusive"] = "2026-01-20"
    protocol_doc["ablation_source"] = {
        "plan_sha256": "1" * 64,
        "tape_sha256": "2" * 64,
        "mark_max_age_seconds": 86400,
        "max_total_llm_calls": 32,
        "baseline": "variant",
        "variants": {"variant": "full_graph", "agent": "single_analyst"},
    }
    protocol = _protocol(protocol_doc)
    frozen = freeze_forward(root, plan, protocol)
    assert set(frozen.configuration.plan.strategies) == {"agent", "variant"}
    assert (
        verify_forward(root, frozen, protocol, expected_sha256=forward_freeze_sha256(frozen))
        == frozen
    )


@pytest.mark.parametrize(
    "bad", ["../secret", "a/../b", "/tmp/x", "C:/thing", "data/NUL", "one/./x"]
)
def test_rejects_nonportable_or_outside_paths(bundle, bad):
    root, doc = bundle
    doc["strategies"]["agent"]["files"]["strategy"] = [bad]
    with pytest.raises(ValueError, match="path|regex"):
        forward_configuration_sha256(root, _load(doc))


def test_rejects_escaped_symlink(bundle):
    root, doc = bundle
    plan = _load(doc)
    outside = root.parent / "elsewhere.txt"
    outside.write_bytes(b"external bytes")
    art = root / "variants/agent.txt"
    art.unlink()
    try:
        art.symlink_to(outside)
    except OSError:
        pytest.skip("creating symbolic links requires Windows privileges")
    with pytest.raises(ValueError, match="inside the configuration root"):
        forward_configuration_sha256(root, plan)


def test_rejects_unknown_versions_fields_and_duplicates():
    doc = _plan_doc()
    doc["schema_version"] = "0.2"
    with pytest.raises(ValueError, match="schema_version"):
        _load(doc)
    doc["schema_version"] = "0.1"
    doc["unregistered"] = "unknown"
    with pytest.raises(ValueError, match="[Uu]nknown field"):
        _load(doc)
    with pytest.raises(ValueError, match="[Dd]uplicate"):
        load_forward_plan(b'{"schema_version":"0.1","schema_version":"0.1"}')


def test_file_missing_or_unbounded_is_rejected(bundle, monkeypatch):
    root, doc = bundle
    plan = _load(doc)
    art = root / "variants/agent.txt"
    art.unlink()
    with pytest.raises(ValueError, match="Cannot read configuration artifact"):
        forward_configuration_sha256(root, plan)
    art.write_bytes(b"too large" * 32)
    monkeypatch.setattr("tradingbot_evaluation.forward_freeze.MAX_ARTIFACT_BYTES", 128)
    with pytest.raises(ValueError, match="artifact size"):
        forward_configuration_sha256(root, plan)


def test_checked_in_contracts_match_serialized_documents():
    from tradingbot_evaluation.forward_freeze import ForwardFreeze, ForwardPlan

    for path, contract in (("forward-plan", ForwardPlan), ("forward-freeze", ForwardFreeze)):
        published = json.loads((ROOT / "contracts" / (path + ".schema.json")).read_bytes())
        assert published == {
            "$schema": "https://json-schema.org/draft/2020-12/schema"
        } | msgspec.json.schema(contract)
    plan = load_forward_plan((ROOT / "contracts" / "valid-forward-plan.json").read_bytes())
    digest = forward_configuration_sha256(ROOT, plan)
    assert len(digest) == 64
