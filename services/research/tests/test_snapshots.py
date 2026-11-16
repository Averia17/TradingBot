import hashlib
import json
from datetime import UTC, datetime

import pytest

from tradingbot.snapshots import ArchiveReference, load_snapshot


def timestamp(hour):
    return f"2026-11-16T{hour:02}:00:00Z"


def record(kind, record_id, *, symbol="TSLA", revision=1, event=8, available=9, payload=None):
    return {
        "kind": kind,
        "provider": "fixture",
        "record_id": record_id,
        "symbol": symbol,
        "revision": revision,
        "event_time": timestamp(event),
        "available_at": timestamp(available),
        "ingested_at": timestamp(available + 1),
        "payload": payload or {"value": "1.25"},
    }


def write_archive(root, records, *, schema_version="0.1"):
    records_path = root / "archive.json"
    raw = json.dumps(
        {"schema_version": schema_version, "source_mode": "synthetic_fixture", "records": records}
    ).encode()
    records_path.write_bytes(raw)
    return ArchiveReference(ref=records_path.name, sha256=hashlib.sha256(raw).hexdigest())


def test_pit_selects_available_revision_by_source_and_symbol(tmp_path):
    manifest = write_archive(
        tmp_path,
        [
            record("filing", "10q", revision=2, available=13, payload={"period_value": "3.75"}),
            record("bar", "daily", event=16, available=17),
            record("news", "headline", symbol="NVDA"),
            record("action", "split", payload={"effective_at": timestamp(19)}),
            record("filing", "10q", payload={"period_value": "2.50"}),
            record("bar", "opening", payload={"close": "120.00001000"}),
            record("news", "general", symbol=None),
        ],
    )
    at_ten = load_snapshot(
        tmp_path, manifest, datetime.fromisoformat(timestamp(10)), symbols=["TSLA"]
    )
    by_id = {item.record_id: item for item in at_ten.records}
    assert set(by_id) == {"10q", "split", "opening", "general"}
    assert by_id["10q"].revision == 1
    assert by_id["10q"].payload["period_value"] == "2.50"
    assert by_id["opening"].payload["close"] == "120.00001000"
    assert at_ten.source_mode == "synthetic_fixture"
    assert at_ten.archive == manifest

    at_release = load_snapshot(tmp_path, manifest, datetime.fromisoformat(timestamp(13)))
    amended = next(item for item in at_release.records if item.record_id == "10q")
    assert amended.revision == 2
    assert amended.payload["period_value"] == "3.75"
    assert amended.ingested_at > at_release.as_of
    assert at_ten.sha256 != at_release.sha256


def test_snapshot_serialization_uses_normalized_cutoff_and_symbol_order(tmp_path):
    manifest = write_archive(tmp_path, [record("bar", "opening")])
    left = load_snapshot(
        tmp_path, manifest, datetime.fromisoformat(timestamp(10)), symbols=["TSLA", "NVDA"]
    )
    right = load_snapshot(
        tmp_path,
        manifest,
        datetime.fromisoformat("2026-11-16T11:00:00+01:00"),
        symbols=["NVDA", "TSLA"],
    )
    assert left.model_dump(mode="json") == right.model_dump(mode="json")
    assert left.sha256 == right.sha256


def test_hash_mismatch_fails_before_decoding(tmp_path):
    manifest = write_archive(tmp_path, [record("bar", "opening")])
    (tmp_path / "archive.json").write_bytes(b"not even JSON")

    with pytest.raises(ValueError, match="archive SHA-256 mismatch"):
        load_snapshot(tmp_path, manifest, datetime(2026, 11, 16, tzinfo=UTC))


@pytest.mark.parametrize(
    "ref",
    [
        "../archive.json",
        "sub/../../archive.json",
        "sub//archive.json",
        "sub/./archive.json",
        "C:\\archive.json",
    ],
)
def test_archive_rejects_path_escape(tmp_path, ref):
    with pytest.raises(ValueError):
        load_snapshot(
            tmp_path, {"ref": ref, "sha256": "0" * 64}, datetime(2026, 11, 16, tzinfo=UTC)
        )


def test_archive_rejects_link_outside_root(tmp_path):
    root = tmp_path / "store"
    root.mkdir()
    manifest = write_archive(tmp_path, [record("bar", "opening")])
    try:
        (root / manifest.ref).symlink_to(tmp_path / manifest.ref)
    except OSError:
        pytest.skip("symbolic link creation unavailable")

    with pytest.raises(ValueError, match="outside the archive root"):
        load_snapshot(root, manifest, datetime(2026, 11, 16, tzinfo=UTC))


@pytest.mark.parametrize(
    ("records", "message"),
    [
        ([record("filing", "10q"), record("filing", "10q")], "duplicate record revision"),
        (
            [record("filing", "10q", available=12), record("filing", "10q", revision=2)],
            "revision timestamps",
        ),
        ([record("bar", "opening", event=12, available=9)], "event_time.*available_at"),
        ([record("bar", "opening", payload={"close": 0.125})], "decimal strings"),
    ],
)
def test_malformed_archive_cannot_publish_snapshot(tmp_path, records, message):
    manifest = write_archive(tmp_path, records)

    with pytest.raises(ValueError, match=message):
        load_snapshot(tmp_path, manifest, datetime(2026, 11, 16, tzinfo=UTC))


def test_cutoff_requires_timezone(tmp_path):
    manifest = write_archive(tmp_path, [record("bar", "opening")])

    with pytest.raises(ValueError, match="timezone"):
        load_snapshot(tmp_path, manifest, datetime(2026, 11, 16))


def test_archive_rejects_unknown_version(tmp_path):
    manifest = write_archive(tmp_path, [record("bar", "opening")], schema_version="1.0")

    with pytest.raises(ValueError, match="schema_version"):
        load_snapshot(tmp_path, manifest, datetime(2026, 11, 16, tzinfo=UTC))


def test_verified_archive_still_rejects_duplicate_json_fields(tmp_path):
    raw = b'{"schema_version":"0.1","source_mode":"local_archive","records":[],"records":[]}'
    (tmp_path / "archive.json").write_bytes(raw)
    manifest = ArchiveReference(ref="archive.json", sha256=hashlib.sha256(raw).hexdigest())

    with pytest.raises(ValueError, match="duplicate JSON field"):
        load_snapshot(tmp_path, manifest, datetime(2026, 11, 16, tzinfo=UTC))
