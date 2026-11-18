import hashlib
import json
from datetime import datetime
from pathlib import Path

import pytest

from tradingbot.contracts import Position
from tradingbot.paper_memory import build_contexts
from tradingbot.security_identity import (
    IdentityProjectionError,
    SecurityBinding,
    SecurityRegistry,
    load_identity_snapshot,
)

CUTOFF = datetime.fromisoformat("2026-11-18T16:00:00Z")
EFFECTIVE = "2026-11-18T14:30:00Z"
OLD = "2026-11-17T15:00:00Z"
NEW = "2026-11-18T15:00:00Z"
NEW_SECURITY = {"symbol": "ABC", "security_id": "issuer-new"}
INSTRUMENT = {"instrument_id": "fixture:ABC-new", "registry_version": "fixture-v1", "decimals": 2}


def pin(root, name, value):
    raw = json.dumps(value).encode()
    (root / name).write_bytes(raw)
    return {"ref": name, "sha256": hashlib.sha256(raw).hexdigest()}


def registry():
    return {
        "schema_version": "0.1",
        "universe_provider": "fixture-roster",
        "entries": [
            NEW_SECURITY
            | {
                "available_at": OLD,
                "effective_at": EFFECTIVE,
                "record_providers": ["fixture-market", "fixture-news"],
                "sec_cik": "0000320000",
                "instrument": INSTRUMENT,
            },
        ],
    }


def observation(name, kind, provider, payload, *, symbol="ABC", at=NEW, received=None):
    return {
        "record_id": name,
        "kind": kind,
        "provider": provider,
        "symbol": symbol,
        "revision": 1,
        "event_time": at,
        "available_at": at,
        "ingested_at": received or at,
        "payload": payload,
    }


def listing(name, security_id, status, *, effective=EFFECTIVE, symbol="ABC", received=None):
    return observation(
        name,
        "listing",
        "fixture-roster",
        {
            "security_id": security_id,
            "status": status,
            "effective_at": effective,
            "source_sha256": "a" * 64,
        },
        symbol=symbol,
        at=OLD,
        received=received,
    )


def archive(root, *records, version="0.1"):
    return pin(
        root,
        "archive.json",
        {
            "schema_version": version,
            "source_mode": "synthetic_fixture",
            "records": records,
        },
    )


def position(instrument_id):
    return Position(symbol="ABC", instrument_id=instrument_id, quantity="2", mark_price_usd="10")


def test_stable_binding_projects_only_proven_observations_and_known_macro(tmp_path):
    registry_ref = pin(tmp_path, "registry.json", registry())
    manifest = archive(
        tmp_path,
        listing("old-list", "issuer-old", "listed", effective=OLD),
        listing("old-delist", "issuer-old", "delisted"),
        listing("new-list", "issuer-new", "listed"),
        observation(
            "good-bar", "bar", "fixture-market", {"security_id": "issuer-new", "close": "25"}
        ),
        observation(
            "old-issuer", "bar", "fixture-market", {"security_id": "issuer-old", "close": "250"}
        ),
        observation(
            "late-good",
            "bar",
            "fixture-market",
            {"security_id": "issuer-new"},
            received="2026-11-18T16:00:01Z",
        ),
        observation("wrong-source", "bar", "other-market", {"security_id": "issuer-new"}),
        observation(
            "before-binding", "bar", "fixture-market", {"security_id": "issuer-new"}, at=OLD
        ),
        observation("alias", "bar", "fixture-market", {"security_id": "issuer-new"}, symbol="XYZ"),
        observation("sound-news", "news", "fixture-news", {"security_id": "issuer-new"}),
        observation("ticker-only-news", "news", "fixture-news", {"headline": "unbound issuer"}),
        observation("sec-filing", "filing", "sec-edgar", {"cik": "0000320000"}),
        observation(
            "wrong-cik", "filing", "sec-edgar", {"cik": "0000320001", "security_id": "issuer-new"}
        ),
        observation("macro-known", "macro", "bls", {"value": "100"}, symbol=None),
        observation(
            "macro-future",
            "macro",
            "bls",
            {"value": "200"},
            symbol=None,
            received="2026-11-18T16:00:01Z",
        ),
        observation("general-news", "news", "fixture-news", {}, symbol=None),
    )
    binding = SecurityBinding.model_validate(NEW_SECURITY)
    snapshot = load_identity_snapshot(tmp_path, manifest, registry_ref, CUTOFF, [binding])
    assert {row.record_id for row in snapshot.records} == {
        "new-list",
        "good-bar",
        "sound-news",
        "sec-filing",
        "macro-known",
    }
    assert snapshot.identity_registry.model_dump() == registry_ref
    assert snapshot.security_bindings == (binding,)
    replacement = pin(tmp_path, "a-different-registry.json", registry())
    same_records = load_identity_snapshot(tmp_path, manifest, replacement, CUTOFF, [binding])
    assert same_records.records == snapshot.records
    assert same_records.sha256 != snapshot.sha256


@pytest.mark.parametrize(
    ("change", "reason"),
    [
        ("unseen-registry", "identity_registry_entry_missing"),
        ("retired-registry", "identity_registry_entry_missing"),
        ("registry-overlap", "identity_registry_ambiguous"),
        ("unseen-listing", "universe_evidence_missing"),
        ("delisted", "security_delisted"),
        ("listing-alias", "universe_symbol_mismatch"),
        ("listing-overlap", "ambiguous_universe_symbol"),
        ("held-old", "ambiguous_held_security"),
    ],
)
def test_missing_and_ambiguous_identity_are_not_inferred(tmp_path, change, reason):
    registry_input = registry()
    current = registry_input["entries"][0]
    current_listing = listing("new-list", "issuer-new", "listed")
    records = [
        current_listing,
        observation("good-bar", "bar", "fixture-market", {"security_id": "issuer-new"}),
    ]
    held = []
    if change == "unseen-registry":
        current["available_at"] = "2026-11-18T16:00:01Z"
    if change == "retired-registry":
        current["expires_at"] = CUTOFF.isoformat()
    if change == "registry-overlap":
        registry_input["entries"].append(current | {"security_id": "issuer-old"})
    if change == "unseen-listing":
        current_listing["ingested_at"] = "2026-11-18T16:00:01Z"
    if change == "delisted":
        current_listing["payload"]["status"] = "delisted"
    if change == "listing-alias":
        current_listing["symbol"] = "XYZ"
    if change == "listing-overlap":
        records.append(listing("old-list", "issuer-old", "listed", effective=OLD))
    if change == "held-old":
        held.append(position("fixture:ABC-old"))
    binding = SecurityBinding.model_validate(NEW_SECURITY)
    with pytest.raises(IdentityProjectionError) as exc:
        load_identity_snapshot(
            tmp_path,
            archive(tmp_path, *records),
            pin(tmp_path, "registry.json", registry_input),
            CUTOFF,
            [binding],
            positions=held,
        )
    assert exc.value.reason == reason


def test_held_instrument_requires_the_exact_owner_match_and_registry_hash(tmp_path):
    raw = registry()
    registry_ref = pin(tmp_path, "registry.json", raw)
    manifest = archive(tmp_path, listing("new-list", "issuer-new", "listed"))
    binding = SecurityBinding.model_validate(NEW_SECURITY)
    snapshot = load_identity_snapshot(
        tmp_path,
        manifest,
        registry_ref,
        CUTOFF,
        [binding],
        positions=[position("fixture:ABC-new")],
    )
    assert snapshot.records[0].record_id == "new-list"
    (tmp_path / "registry.json").write_text("{}", encoding="utf-8")
    with pytest.raises(ValueError, match="SHA-256 mismatch"):
        load_identity_snapshot(tmp_path, manifest, registry_ref, CUTOFF, [binding])


def test_paper_outcomes_require_matching_registry_version_and_exact_instrument(tmp_path):
    from test_paper_memory import paper_record

    current, old_version, old_identity = (paper_record(number, "ABC") for number in (5, 6, 7))
    for record in (current, old_version, old_identity):
        record.update(event_time=NEW, available_at=NEW, ingested_at=NEW)
        record["payload"]["instrument"] = INSTRUMENT.copy()
    old_version["payload"]["instrument"]["registry_version"] = "fixture-v0"
    old_identity["payload"]["instrument"]["instrument_id"] = "fixture:ABC-old"
    manifest = pin(
        tmp_path,
        "archive.json",
        {
            "schema_version": "0.2",
            "source_mode": "synthetic_fixture",
            "records": [
                listing("new-list", "issuer-new", "listed"),
                current,
                old_version,
                old_identity,
            ],
        },
    )
    snapshot = load_identity_snapshot(
        tmp_path,
        manifest,
        pin(tmp_path, "registry.json", registry()),
        CUTOFF,
        [NEW_SECURITY],
    )
    assert {record.record_id for record in snapshot.records} == {"new-list", current["record_id"]}


@pytest.mark.parametrize(
    ("prior_id", "prior_version"),
    [("issuer-old", "fixture-v1"), ("issuer-new", "fixture-v0")],
)
def test_held_symbol_cannot_reuse_a_known_historical_instrument_id(tmp_path, prior_id, prior_version):
    raw = registry()
    current = raw["entries"][0]
    raw["entries"].append(current | {
        "security_id": prior_id,
        "effective_at": OLD,
        "expires_at": EFFECTIVE,
        "instrument": INSTRUMENT | {"registry_version": prior_version},
    })
    manifest = archive(tmp_path, listing("new-list", "issuer-new", "listed"))
    with pytest.raises(IdentityProjectionError, match="ambiguous_held_security"):
        load_identity_snapshot(
            tmp_path, manifest, pin(tmp_path, "registry.json", raw), CUTOFF, [NEW_SECURITY],
            positions=[position(INSTRUMENT["instrument_id"])],
        )


def test_reused_exact_ledger_tuple_cannot_assign_a_sale_to_the_new_issuer(tmp_path):
    from test_paper_memory import paper_record

    raw = registry()
    current = raw["entries"][0]
    old = current | {"security_id": "issuer-old", "effective_at": OLD, "expires_at": EFFECTIVE}
    raw["entries"].append(old)
    sale = paper_record(5, "ABC")
    sale.update(event_time=NEW, available_at=NEW, ingested_at=NEW)
    sale["payload"]["instrument"] = INSTRUMENT
    manifest = archive(tmp_path, listing("new-list", "issuer-new", "listed"), sale, version="0.2")

    def project():
        return load_identity_snapshot(
            tmp_path, manifest, pin(tmp_path, "registry.json", raw), CUTOFF, [NEW_SECURITY],
        )

    disputed = project()
    assert [record.record_id for record in disputed.records] == ["new-list"]
    assert build_contexts(disputed, ["ABC"], "policy-v1", include_reflections=True) == {}

    old["available_at"] = "2026-11-18T16:00:01Z"
    known_at_cutoff = project()
    assert {record.record_id for record in known_at_cutoff.records} == {"new-list", sale["record_id"]}


@pytest.mark.parametrize("memory_state", ["active", "retracted", "stale", "legacy"])
def test_stable_memory_uses_own_outcomes_and_durable_reflection_at_exact_cutoff(tmp_path, memory_state):
    from test_paper_memory import durable_record, paper_record

    raw = registry()
    other = {"symbol": "XYZ", "security_id": "issuer-xyz"}
    other_instrument = INSTRUMENT | {"instrument_id": "fixture:XYZ"}
    raw["entries"].append(raw["entries"][0] | other | {"instrument": other_instrument})
    own = paper_record(9, "ABC") if memory_state == "legacy" else durable_record()
    own.update(symbol="ABC", event_time=NEW, available_at=NEW, ingested_at=NEW)
    own["payload"]["instrument"] = INSTRUMENT
    if memory_state != "legacy":
        memory = own["payload"]["durable_memory"]
        memory["as_of"] = CUTOFF.isoformat()
        memory["active"]["available_at"] = "2026-11-18T15:30:00Z"
        if memory_state == "retracted":
            memory["active"] = None
        if memory_state == "stale":
            memory["as_of"] = "2026-11-18T15:59:59Z"
    peer = paper_record(7, "XYZ")
    peer.update(event_time=NEW, available_at=NEW, ingested_at=NEW)
    peer["payload"]["instrument"] = other_instrument
    manifest = archive(
        tmp_path,
        listing("abc-list", "issuer-new", "listed"),
        listing("xyz-list", "issuer-xyz", "listed", symbol="XYZ"),
        own, peer, version="0.2",
    )
    reference = pin(tmp_path, "registry.json", raw)
    projected = load_identity_snapshot(tmp_path, manifest, reference, CUTOFF, [NEW_SECURITY, other])
    contexts = build_contexts(projected, ["ABC", "XYZ"], "policy-v1", include_reflections=True)
    abc = json.loads(contexts["ABC"].split("\n", 1)[1])
    xyz = json.loads(contexts["XYZ"].split("\n", 1)[1])
    assert abc["security_binding"] == NEW_SECURITY
    assert abc["identity_registry_sha256"] == reference["sha256"]
    assert [item["record_id"] for item in abc["records"]] == [own["record_id"]]
    assert [item["record_id"] for item in xyz["records"]] == [peer["record_id"]]
    assert ("factual_reflection" in abc["records"][0]) == (memory_state == "active")
    assert "factual_reflection" not in xyz["records"][0]


def test_projection_preserves_binding_order(tmp_path):
    raw = registry()
    other = {"symbol": "XYZ", "security_id": "issuer-xyz"}
    raw["entries"].append(raw["entries"][0] | other | {"instrument": None, "sec_cik": None})
    manifest = archive(
        tmp_path,
        listing("abc-list", "issuer-new", "listed"),
        listing("xyz-list", "issuer-xyz", "listed", symbol="XYZ"),
    )
    snapshot = load_identity_snapshot(
        tmp_path,
        manifest,
        pin(tmp_path, "registry.json", raw),
        CUTOFF,
        [other, NEW_SECURITY],
    )
    assert snapshot.symbols == ("XYZ", "ABC")
    assert [(entry.symbol, entry.security_id) for entry in snapshot.security_bindings] == [
        ("XYZ", "issuer-xyz"),
        ("ABC", "issuer-new"),
    ]


def test_public_registry_schema_and_example_match_consumer():
    root = Path(__file__).resolve().parents[3] / "contracts"
    assert json.loads((root / "research-identity-registry.schema.json").read_bytes()) == (
        SecurityRegistry.model_json_schema()
    )
    SecurityRegistry.model_validate_json(
        (root / "valid-research-identity-registry.json").read_bytes()
    )
