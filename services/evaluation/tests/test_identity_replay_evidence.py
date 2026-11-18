import json
import subprocess
import sys
from dataclasses import dataclass

import msgspec
import pytest
from test_identity_research_replay import identity_inputs
from test_replay_evidence import manifest_document
from test_research_replay import digest, encode, load, store

from tradingbot_evaluation.replay import Replay, replay_sha256
from tradingbot_evaluation.replay_evidence import (
    derive_identity_replay_evidence,
    derive_replay_evidence,
)
from tradingbot_evaluation.research_replay import prepare_research_replay
from tradingbot_evaluation.statistics import (
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)


@dataclass
class Proof:
    document: dict
    tape: bytes
    export: bytes
    registry: bytes
    native: Replay

    def derive(self, **changed):
        protocol = load_protocol(encode(self.document))
        inputs = {
            "tape_raw": self.tape,
            "export_raw": self.export,
            "identity_registry_raw": self.registry,
            "expected_protocol_sha256": protocol_sha256(protocol),
        }
        return protocol, derive_identity_replay_evidence(protocol, **(inputs | changed))


def inputs():
    jobs, tape, registry = identity_inputs()
    old, new = registry["entries"]
    old.update(
        available_at="2026-10-01T00:00:00Z",
        effective_at="2026-10-01T00:00:00Z",
        expires_at="2026-10-03T00:00:00Z",
    )
    new.update(available_at="2026-10-03T00:00:00Z", effective_at="2026-10-03T00:00:00Z")
    benchmark = {
        "instrument_id": "fixture:benchmark",
        "registry_version": "benchmark-v1",
        "decimals": 0,
    }
    registry["entries"].append(
        {
            "symbol": "FUND",
            "security_id": "benchmark-security",
            "available_at": "2026-10-01T00:00:00Z",
            "effective_at": "2026-10-01T00:00:00Z",
            "record_providers": [],
            "instrument": benchmark,
        }
    )
    tape["instruments"].append(
        {"key": "BMK", "security_id": "benchmark-security", "instrument": benchmark}
    )
    tape["replay"]["stock_splits"] = []
    quotes = tape["replay"]["quotes"]
    quotes[:] = [quote for quote in quotes if not quote["observed_at"].endswith(":33Z")]
    for quote in quotes:
        if quote["symbol"] == "NEW":
            for field in ("observed_at", "available_at"):
                quote[field] = quote[field].replace("2026-10-02", "2026-10-03")
    quotes.append(
        quotes[-1]
        | {
            "observed_at": "2026-10-04T12:00:00Z",
            "available_at": "2026-10-04T12:00:00Z",
            "bid_usd": "60",
            "ask_usd": "60",
        }
    )
    for day, price in enumerate(("50", "50", "70", "80"), 1):
        at = f"2026-10-{day:02d}T12:00:00Z"
        quotes.append(
            {
                "symbol": "BMK",
                "observed_at": at,
                "available_at": at,
                "bid_usd": price,
                "ask_usd": price,
                "bid_size": 1000,
                "ask_size": 1000,
            }
        )
    registry_raw = encode(registry)
    pin = tape["identity_registry"] | {"sha256": digest(registry_raw)}
    tape["identity_registry"] = pin
    for job in jobs:
        request, result = load(job, "request"), load(job, "result")
        if job == jobs[-1]:
            for obj, field in (
                (job, "created_at"),
                (job, "completed_at"),
                (request["portfolio"], "as_of"),
                (result["batch"], "as_of"),
                (result["batch"], "created_at"),
                (result["batch"], "expires_at"),
            ):
                obj[field] = obj[field].replace("2026-10-02", "2026-10-03")
        request["identity_registry"] = pin
        result["metadata"]["provenance"]["identity_registry"] = pin
        store(job, "request", request)
        store(job, "result", result)
    export_raw = encode({"schema_version": "0.1", "jobs": jobs})
    tape_raw = encode(tape)
    prepared = prepare_research_replay(
        export_raw,
        tape_raw,
        expected_export_sha256=digest(export_raw),
        expected_tape_sha256=digest(tape_raw),
        identity_registry_raw=registry_raw,
        expected_identity_registry_sha256=digest(registry_raw),
    )
    document = manifest_document(replay_sha256(prepared.replay))
    document["schema_version"] = "0.2"
    source = document["replay_source"]
    source["identity"] = {
        "research_export_sha256": digest(export_raw),
        "tape_sha256": digest(tape_raw),
        "identity_registry_sha256": digest(registry_raw),
        "mark_max_age_seconds": 86400,
    }
    buy_and_hold = source["benchmarks"][1]
    buy_and_hold.pop("symbol")
    buy_and_hold["security_id"] = "benchmark-security"
    return Proof(document, tape_raw, export_raw, registry_raw, prepared.replay)


def test_stable_daily_marks_use_one_frozen_calendar_and_exact_benchmark_security():
    proof = inputs()
    protocol, evidence = proof.derive()
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
    assert all(row.nav_usd["stock-hold"] == row.nav_usd["universe"] for row in evidence.daily_nav)
    assert all(row.nav_usd["usd-cash"] == "1000.00" for row in evidence.daily_nav)
    assert evidence.replay_provenance.identity == protocol.replay_source.identity
    assert proof.derive() == (protocol, evidence)
    restored = load_evidence(msgspec.json.encode(evidence))
    report = evaluate_statistics(
        protocol, restored, expected_protocol_sha256=protocol_sha256(protocol)
    )
    assert report["alpha_measured"] is False
    assert (
        report["replay_provenance_declared"]["identity"]
        == proof.document["replay_source"]["identity"]
    )


def test_stable_evidence_rechecks_every_hash_and_rejects_legacy_replay():
    proof = inputs()
    protocol = load_protocol(encode(proof.document))
    frozen = protocol_sha256(protocol)
    for raw, field, label in (
        (proof.tape, "tape_raw", "tape SHA-256"),
        (proof.export, "export_raw", "export SHA-256"),
        (proof.registry, "identity_registry_raw", "registry SHA-256"),
    ):
        with pytest.raises(ValueError, match=label):
            proof.derive(**{field: raw + b"\n"})
    with pytest.raises(ValueError, match="original research export"):
        derive_replay_evidence(protocol, proof.native, expected_protocol_sha256=frozen)
    with pytest.raises(ValueError, match="Frozen protocol"):
        proof.derive(expected_protocol_sha256="f" * 64)
    legacy = msgspec.json.encode(proof.native)
    pins = proof.document["replay_source"]["identity"]
    pins["tape_sha256"] = digest(legacy)
    with pytest.raises(ValueError, match="Legacy research tape"):
        proof.derive(tape_raw=legacy)


def test_stable_evidence_refuses_bad_benchmark_pin_bad_cutoff_and_swapped_proof():
    proof = inputs()
    protocol, evidence = proof.derive()
    provenance = msgspec.to_builtins(evidence)
    provenance["replay_provenance"]["identity"]["tape_sha256"] = "f" * 64
    with pytest.raises(ValueError, match="all frozen source pins"):
        evaluate_statistics(
            protocol,
            load_evidence(encode(provenance)),
            expected_protocol_sha256=protocol_sha256(protocol),
        )
    source = proof.document["replay_source"]
    source["replay_sha256"] = "f" * 64
    with pytest.raises(ValueError, match="Normalized replay SHA-256"):
        proof.derive()
    source["replay_sha256"] = replay_sha256(proof.native)
    mapping = source["benchmarks"][1]
    mapping["security_id"] = "missing-key"
    with pytest.raises(ValueError, match="benchmark security_id"):
        proof.derive()
    mapping["security_id"] = "benchmark-security"
    source["identity"]["mark_max_age_seconds"] = 30
    with pytest.raises(ValueError, match="Identity replay missing or stale mark"):
        proof.derive()


def test_stable_evidence_excludes_ticker_aliases_and_premature_benchmark_quotes():
    proof = inputs()
    source = proof.document["replay_source"]
    mapping = source["benchmarks"][1]
    security = mapping.pop("security_id")
    mapping["symbol"] = "BMK"
    with pytest.raises(ValueError, match="security_id"):
        proof.derive()
    del mapping["symbol"]
    mapping["security_id"] = security

    tape = json.loads(proof.tape)
    tape["replay"]["quotes"].append(
        next(quote for quote in tape["replay"]["quotes"] if quote["symbol"] == "NEW")
        | {"observed_at": "2026-10-02T12:00:00Z", "available_at": "2026-10-02T12:00:00Z"}
    )
    tape_raw = encode(tape)
    source["identity"]["tape_sha256"] = digest(tape_raw)
    with pytest.raises(ValueError, match="unambiguous point-in-time eligibility"):
        proof.derive(tape_raw=tape_raw)


def test_cli_writes_bound_evidence_for_statistics(tmp_path):
    proof = inputs()
    protocol = load_protocol(encode(proof.document))
    frozen = protocol_sha256(protocol)
    files = {}
    for name, raw in (
        ("protocol", encode(proof.document)),
        ("tape", proof.tape),
        ("export", proof.export),
        ("registry", proof.registry),
    ):
        files[name] = tmp_path / f"{name}.json"
        files[name].write_bytes(raw)
    runtime = [sys.executable, "-m", "tradingbot_evaluation"]
    command = [
        *runtime,
        "replay-evidence",
        str(files["protocol"]),
        str(files["tape"]),
        "--protocol-sha256",
        frozen,
        "--research-export",
        str(files["export"]),
        "--identity-registry",
        str(files["registry"]),
    ]
    stdout = subprocess.run(command, capture_output=True, check=True, timeout=30).stdout
    assert stdout == subprocess.run(command, capture_output=True, check=True, timeout=30).stdout
    retained = tmp_path / "evidence.json"
    retained.write_bytes(stdout)
    report = subprocess.run(
        [
            *runtime,
            "statistics",
            str(files["protocol"]),
            str(retained),
            "--protocol-sha256",
            frozen,
            "--evidence-sha256",
            digest(stdout),
        ],
        capture_output=True,
        check=True,
        timeout=30,
    )
    value = json.loads(report.stdout)
    assert value["verification_scope"]["evidence_file_hash_verified"] is True
    assert (
        value["replay_provenance_declared"]["identity"]
        == proof.document["replay_source"]["identity"]
    )
