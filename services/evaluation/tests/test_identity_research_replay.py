import json
import subprocess
import sys
from pathlib import Path

import pytest
from test_research_replay import at, digest, encode, entry, load, quote, run, store, tape


def identity_inputs():
    old = {"instrument_id": "fixture:old", "registry_version": "old-v1", "decimals": 2}
    new = {"instrument_id": "fixture:new", "registry_version": "new-v1", "decimals": 4}
    registry = {
        "schema_version": "0.1",
        "universe_provider": "synthetic-listings",
        "entries": [
            {
                "symbol": "TEST",
                "security_id": "old-security",
                "available_at": at(0),
                "effective_at": at(0),
                "expires_at": at(25),
                "record_providers": [],
                "instrument": old,
            },
            {
                "symbol": "TEST",
                "security_id": "new-security",
                "available_at": at(25),
                "effective_at": at(25),
                "record_providers": [],
                "instrument": new,
            },
        ],
    }
    reference = {"ref": "identity-registry.json", "sha256": digest(encode(registry))}
    native = tape(
        [
            quote(6, "100", "100", symbol="OLD"),
            quote(9, "100", "100", symbol="OLD"),
            quote(12, "103", "103", symbol="OLD"),
            quote(15, "103", "103", symbol="OLD"),
            quote(30, "50", "50", symbol="NEW"),
            quote(33, "1", "1", symbol="OLD"),
            quote(34, "50", "50", symbol="NEW"),
        ],
        commission_bps=0,
        stock_splits=[
            {
                "action_id": "old-split",
                "symbol": "OLD",
                "available_at": at(31),
                "effective_at": at(33),
                "old_shares": 1,
                "new_shares": 2,
            },
        ],
    )
    wrapped = {
        "schema_version": "0.2",
        "identity_registry": reference,
        "instruments": [
            {"key": "OLD", "security_id": "old-security", "instrument": old.copy()},
            {"key": "NEW", "security_id": "new-security", "instrument": new.copy()},
        ],
        "replay": native,
    }
    jobs = [
        entry(1, "BUY", "TEST", cutoff=6, completed=8),
        entry(
            2,
            "SELL",
            "TEST",
            cutoff=12,
            completed=14,
            cash="800",
            notional="206",
            positions=[
                {
                    "symbol": "TEST",
                    "instrument_id": old["instrument_id"],
                    "quantity": "2",
                    "mark_price_usd": "103",
                }
            ],
        ),
        entry(3, "BUY", "TEST", cutoff=30, completed=32, cash="1006", notional="100"),
    ]
    for job, identity in zip(jobs, ("old-security", "old-security", "new-security"), strict=True):
        bindings = [{"symbol": "TEST", "security_id": identity}]
        stored = load(job, "request")
        stored.update(
            job_schema_version="0.2",
            identity_registry=reference,
            security_bindings=bindings,
        )
        store(job, "request", stored)
        result = load(job, "result")
        result["result_schema_version"] = "0.3"
        result["metadata"]["provenance"].update(
            identity_registry=reference,
            security_bindings=bindings,
            projected_snapshot_sha256="d" * 64,
        )
        store(job, "result", result)
    return jobs, wrapped, registry


def replay_identity(jobs, wrapped, registry, **pins):
    raw = encode(registry)
    return run(
        jobs,
        wrapped,
        identity_registry_raw=raw,
        expected_identity_registry_sha256=pins.get("registry_sha", digest(raw)),
    )


def test_native_replay_keeps_reused_ticker_quotes_splits_and_positions_separate():
    jobs, wrapped, registry = identity_inputs()
    report = replay_identity(jobs, wrapped, registry)
    assert report["research_replay_schema_version"] == "0.2"
    assert report["snapshot_continuity_verified"] is True
    assert report["identity_registry"] == wrapped["identity_registry"]
    assert report["replay_instruments"] == wrapped["instruments"]
    assert [(fill["symbol"], fill["price_usd"], fill["quantity"]) for fill in report["fills"]] == [
        ("OLD", "100.00", "2"),
        ("OLD", "103.00", "2"),
        ("NEW", "50.00", "2"),
    ]
    assert [row["security_id"] for row in report["decisions"]] == [
        "old-security",
        "old-security",
        "new-security",
    ]
    assert report["open_positions"] == {"NEW": "2"}
    assert report["ending_cash_usd"] == "906.00"


def test_bound_replay_requires_verified_owner_registry_and_exact_tuple():
    jobs, wrapped, registry = identity_inputs()
    with pytest.raises(ValueError, match="identity registry"):
        run(jobs, wrapped)
    with pytest.raises(ValueError, match="SHA-256"):
        replay_identity(jobs, wrapped, registry, registry_sha="f" * 64)
    wrapped["instruments"][1]["instrument"]["decimals"] = 2
    with pytest.raises(ValueError, match="instrument"):
        replay_identity(jobs, wrapped, registry)


def test_bound_replay_rejects_changed_result_and_unmapped_held_instrument():
    jobs, wrapped, registry = identity_inputs()
    result = load(jobs[2], "result")
    result["metadata"]["provenance"]["security_bindings"][0]["security_id"] = "old-security"
    store(jobs[2], "result", result)
    with pytest.raises(ValueError, match="security bindings"):
        replay_identity(jobs, wrapped, registry)

    jobs, wrapped, registry = identity_inputs()
    request = load(jobs[1], "request")
    request["portfolio"]["positions"][0]["instrument_id"] = "fixture:new"
    store(jobs[1], "request", request)
    with pytest.raises(ValueError, match="held instrument|registry"):
        replay_identity(jobs, wrapped, registry)


def test_bound_replay_does_not_value_a_successor_before_its_registry_window():
    jobs, wrapped, registry = identity_inputs()
    mark = wrapped["replay"]["quotes"][4]
    mark.update(observed_at=at(23), available_at=at(24))
    report = replay_identity(jobs, wrapped, registry)
    assert report["decisions"][-1]["status"] == "unavailable_reference_mark"
    assert report["open_positions"] == {}
    assert report["ending_cash_usd"] == "1006.00"


def test_cli_replays_identity_export_and_emits_bound_native_input(tmp_path: Path):
    jobs, wrapped, registry = identity_inputs()
    data = (
        ("export", {"schema_version": "0.1", "jobs": jobs}),
        ("tape", wrapped),
        ("identity-registry", registry),
    )
    args = [sys.executable, "-m", "tradingbot_evaluation", "research-replay"]
    for name, value in data:
        path = tmp_path / f"{name}.json"
        raw = encode(value)
        path.write_bytes(raw)
        if name == "identity-registry":
            args.append("--identity-registry")
        args += [str(path), f"--{name}-sha256", digest(raw)]
    result = subprocess.run(args, capture_output=True, check=True, timeout=30)
    assert json.loads(result.stdout)["snapshot_continuity_verified"] is True

    emitted = subprocess.run(
        [*args, "--emit-replay-input"],
        capture_output=True,
        check=True,
        timeout=30,
    )
    evidence = json.loads(emitted.stdout)
    assert evidence["schema_version"] == "0.2"
    assert evidence["identity_registry"] == wrapped["identity_registry"]
    assert evidence["instruments"] == wrapped["instruments"]
    assert [row["symbol"] for row in evidence["replay"]["orders"]] == ["OLD", "OLD", "NEW"]
