import hashlib
import json
import subprocess
from datetime import datetime
from pathlib import Path

import pytest

from tradingbot.cli import main
from tradingbot.contracts import PortfolioSnapshot
from tradingbot.prepare_identity_review import prepare_identity_review
from tradingbot.security_identity import load_identity_snapshot

ROOT = Path(__file__).resolve().parents[3]
EXAMPLES = ROOT / "services/research/examples"
ACCOUNT = "11111111-1111-4111-8111-111111111111"
ORDER = "22222222-2222-4222-8222-222222222222"
AS_OF = "2026-11-18T10:00:00Z"
INSTRUMENT = {"instrument_id": "fixture:TSLA", "registry_version": "fixture-v1", "decimals": 2}


def pin(path):
    return {"ref": path.name, "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}


@pytest.fixture
def paperwork(tmp_path):
    def copy(path):
        target = tmp_path / path.name
        target.write_bytes(path.read_bytes())
        return target

    policy = copy(ROOT / "contracts/valid-paper-policy.json")
    marks = copy(EXAMPLES / "entry-cycles/marks.json")
    options = {
        "archive_root": tmp_path,
        "account": ACCOUNT,
        "order": ORDER,
        "as_of": AS_OF,
        "policy": policy,
        "marks": marks,
        "data_manifest": pin(copy(EXAMPLES / "selection/identity-market.json")),
        "identity_registry": pin(copy(EXAMPLES / "selection/identity-registry.json")),
        "quote_tape": pin(copy(EXAMPLES / "entry-cycles/identity-tape.json")),
        "max_llm_calls": 20,
    }
    view = {
        "paper_thesis_schema_version": "0.2",
        "account_id": ACCOUNT,
        "account_version": 5,
        "as_of": AS_OF,
        "research_job_id": "33333333-3333-4333-8333-333333333333",
        "decision_id": "44444444-4444-4444-8444-444444444444",
        "opening_order_id": ORDER,
        "instrument": INSTRUMENT.copy(),
        "symbol": "TSLA",
        "state": "open",
        "opening_order_state": "filled",
        "first_fill_event_id": "55555555-5555-4555-8555-555555555555",
        "opened_at": "2026-11-18T09:00:00Z",
        "next_review_at": AS_OF,
        "thesis_expires_at": "2026-11-20T09:00:00Z",
        "horizon": {"min_hours": 1, "max_hours": 48},
        "source_quantity_raw": "100",
        "held_quantity_raw": "123",
        "reserved_quantity_raw": "23",
        "free_quantity_raw": "100",
        "cash_usd": "876.999",
        "reserved_cash_usd": "13.00000001",
        "opening_identity": {
            "symbol": "TSLA",
            "security_id": "fixture-current",
            "instrument": INSTRUMENT.copy(),
            "identity_registry": {"ref": "opening-registry.json", "sha256": "b" * 64},
            "application_sha256": "a" * 64,
        },
        "clock_review": {
            "trigger_id": "66666666-6666-4666-8666-666666666666",
            "kind": "review",
            "reason": "cadence",
            "at": AS_OF,
        },
    }
    snapshot = {
        "snapshot_context": {
            "account_id": ACCOUNT,
            "snapshot_version": 5,
            "policy": json.loads(policy.read_bytes()),
            "marks": json.loads(marks.read_bytes()),
        },
        "portfolio": {
            "snapshot_id": "77777777-7777-4777-8777-777777777777",
            "as_of": AS_OF,
            "currency": "USD",
            "cash_asset": "USD",
            "cash_usd": "876.999",
            "reserved_cash_usd": "13.00000001",
            "max_position_pct": "0.2500",
            "positions": [
                {
                    "symbol": "TSLA",
                    "instrument_id": "fixture:TSLA",
                    "quantity": "1.23000000",
                    "mark_price_usd": "98",
                }
            ],
        },
    }
    return options, view, snapshot


def test_normal_cli_builds_frozen_review_from_only_two_go_read_calls(
    tmp_path, monkeypatch, capsys, paperwork
):
    options, view, snapshot = paperwork
    binary = tmp_path / "paper.exe"
    binary.write_bytes(b"test local executable")
    calls = []

    def run(argv, **kwargs):
        calls.append(argv[1])
        assert Path(argv[0]) == binary
        assert "env" not in kwargs
        assert argv[argv.index("--as-of") + 1] == AS_OF
        if argv[1] == "review":
            assert argv == [
                str(binary),
                "review",
                "--account",
                ACCOUNT,
                "--order",
                ORDER,
                "--as-of",
                AS_OF,
            ]
        else:
            for document in ("policy", "marks"):
                passed = Path(argv[argv.index(f"--{document}") + 1])
                assert passed.read_bytes() == options[document].read_bytes()
        body = view if argv[1] == "review" else snapshot
        return subprocess.CompletedProcess(argv, 0, json.dumps(body).encode(), b"")

    monkeypatch.setattr("tradingbot.paper_cli.subprocess.run", run)
    argv = ["prepare-identity-review", "--paper-cli", str(binary)]
    for name, value in options.items():
        flag = name.replace("_", "-")
        if isinstance(value, dict):
            argv.extend([f"--{flag}-ref", value["ref"], f"--{flag}-sha256", value["sha256"]])
        else:
            argv.extend([f"--{flag}", str(value)])
    assert main(argv) == 0
    payload = json.loads(capsys.readouterr().out)
    projected = load_identity_snapshot(
        tmp_path,
        options["data_manifest"],
        options["identity_registry"],
        datetime.fromisoformat(AS_OF),
        [{"symbol": "TSLA", "security_id": "fixture-current"}],
        positions=PortfolioSnapshot.model_validate(snapshot["portfolio"]).positions,
    )
    assert calls == ["review", "snapshot"]
    assert payload == {
        "review_schedule_schema_version": "0.2",
        "opening_order_id": ORDER,
        "snapshot_context": snapshot["snapshot_context"],
        "portfolio": snapshot["portfolio"],
        "data_manifest": options["data_manifest"],
        "max_llm_calls": options["max_llm_calls"],
        "identity_binding": {
            "identity_registry": options["identity_registry"],
            "quote_tape": options["quote_tape"],
            "projected_snapshot_sha256": projected.sha256,
        },
    }


@pytest.mark.parametrize(
    ("change", "message"),
    [
        ("legacy", "0.2"),
        ("closed", "open"),
        ("wrong-order", "requested account, order or cutoff"),
        ("wrong-opening-instrument", "stable lineage"),
        ("wrong-issuer", "identity_registry_entry_missing"),
        ("rotated-tuple", "replaces the opening instrument"),
        ("new-account-version", "account version"),
        ("different-go-cutoff", "review cutoff"),
        ("cash-drift", "paper balances"),
        ("no-holding", "reviewed opening instrument and quantity"),
        ("extra-quantity", "reviewed opening instrument and quantity"),
        ("raw-number", "valid string"),
        ("different-policy", "supplied owner policy or marks"),
        ("unavailable-marks", "unavailable marks"),
        ("tampered-tape", "archive SHA-256"),
        ("other-account-tape", "owner account or registry"),
        ("different-registry-quote", "tape's identity registry"),
        ("invalid-cutoff", "capture timestamps"),
    ],
)
def test_preparer_rejects_inconsistent_owner_facts_instead_of_scheduling(
    paperwork, change, message
):
    options, view, snapshot = paperwork
    if change == "legacy":
        view["paper_thesis_schema_version"] = "0.1"
    if change == "closed":
        view["state"] = "closed"
    if change == "wrong-order":
        view["opening_order_id"] = ACCOUNT
    if change == "wrong-opening-instrument":
        view["opening_identity"]["instrument"]["registry_version"] = "old-v1"
    if change == "wrong-issuer":
        view["opening_identity"]["security_id"] = "fixture-old"
    if change == "rotated-tuple":
        registry_path = options["archive_root"] / options["identity_registry"]["ref"]
        registry = json.loads(registry_path.read_bytes())
        owner_entry = next(entry for entry in registry["entries"] if entry["symbol"] == "TSLA")
        owner_entry["instrument"]["registry_version"] = "fixture-v2"
        registry_path.write_text(json.dumps(registry), encoding="utf-8")
        options["identity_registry"] = pin(registry_path)
        tape_path = options["archive_root"] / options["quote_tape"]["ref"]
        tape = json.loads(tape_path.read_bytes())
        tape["identity_registry_sha256"] = options["identity_registry"]["sha256"]
        for observation in tape["observations"]:
            observation["quote"]["identity_registry_sha256"] = tape["identity_registry_sha256"]
        tape_path.write_text(json.dumps(tape), encoding="utf-8")
        options["quote_tape"] = pin(tape_path)
    if change == "new-account-version":
        snapshot["snapshot_context"]["snapshot_version"] += 1
    if change == "different-go-cutoff":
        snapshot["portfolio"]["as_of"] = "2026-11-18T10:00:01Z"
    if change == "cash-drift":
        snapshot["portfolio"]["cash_usd"] = "876"
    if change == "no-holding":
        snapshot["portfolio"]["positions"] = []
    if change == "extra-quantity":
        snapshot["portfolio"]["positions"][0]["quantity"] = "2.23"
    if change == "raw-number":
        snapshot["portfolio"]["positions"][0]["quantity"] = 1.23
    if change == "different-policy":
        snapshot["snapshot_context"]["policy"]["max_order_usd"] = "999"
    if change == "unavailable-marks":
        marks = snapshot["snapshot_context"]["marks"]
        marks[0]["available_at"] = "2026-11-18T10:00:01Z"
        options["marks"].write_text(json.dumps(marks), encoding="utf-8")
    if change in {"tampered-tape", "other-account-tape", "different-registry-quote"}:
        path = options["archive_root"] / options["quote_tape"]["ref"]
        tape = json.loads(path.read_bytes())
        if change == "different-registry-quote":
            tape["observations"][0]["quote"]["identity_registry_sha256"] = "c" * 64
        else:
            tape["account_id"] = ORDER
        path.write_text(json.dumps(tape), encoding="utf-8")
        if change != "tampered-tape":
            options["quote_tape"] = pin(path)
    if change == "invalid-cutoff":
        options["as_of"] = "2026-11-18T10:00:00.000000001Z"

    def invoke(command, *_):
        assert command in {"review", "snapshot"}
        return view if command == "review" else snapshot

    with pytest.raises(ValueError, match=message):
        prepare_identity_review(invoke=invoke, **options)
