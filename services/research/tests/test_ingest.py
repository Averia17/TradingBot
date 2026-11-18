import hashlib
import json
from datetime import datetime
from pathlib import Path

import pytest

from tradingbot.ingest import BLSReleaseReview, CaptureManifest, build_archive
from tradingbot.snapshots import load_snapshot


def pin(root, path, value):
    raw = json.dumps(value).encode()
    (root / path).write_bytes(raw)
    return {"ref": path, "sha256": hashlib.sha256(raw).hexdigest()}


def sec(root):
    original = "0000123456-26-000001"
    amendment = "0000123456-26-000002"
    raw = pin(
        root,
        "sec.json",
        {
            "cik": "0000123456",
            "filings": {
                "recent": {
                    "accessionNumber": [original, amendment],
                    "acceptanceDateTime": ["2026-11-16T16:30:00Z", "2026-11-17T20:00:00Z"],
                    "form": ["10-Q", "10-Q/A"],
                    "reportDate": ["2026-09-30", "2026-09-30"],
                }
            },
        },
    )
    return {
        "kind": "sec_submissions",
        "source": raw,
        "captured_at": "2026-11-17T20:15:00Z",
        "cik": "0000123456",
        "symbol": "TSLA",
        "accessions": [original, amendment],
    }


def alfred(root, vintage, value, capture):
    return {
        "kind": "alfred_observations",
        "source": pin(
            root,
            f"alfred-{vintage}.json",
            {
                "realtime_start": vintage,
                "realtime_end": vintage,
                "units": "lin",
                "output_type": 1,
                "count": 1,
                "offset": 0,
                "observations": [
                    {
                        "realtime_start": vintage,
                        "realtime_end": vintage,
                        "date": "2026-10-01",
                        "value": value,
                    }
                ],
            },
        ),
        "captured_at": capture,
        "series_id": "CPI_TEST",
        "vintage_date": vintage,
    }


def archive(captures):
    return {"schema_version": "0.1", "source_mode": "synthetic_fixture", "captures": captures}


def sec_facts(root, submissions, *, transform=lambda rows: rows):
    original, amendment = submissions["accessions"]
    rows = [
        {
            "accn": accession,
            "form": form,
            "filed": "2026-11-17",
            "start": "2026-07-01",
            "end": "2026-09-30",
            "val": value,
        }
        for accession, form, value in (
            (original, "10-Q", "exact-original"),
            (amendment, "10-Q/A", "exact-revised"),
        )
    ]
    source = {
        "cik": 123456,
        "facts": {"us-gaap": {"EarningsPerShareDiluted": {"units": {"USD/shares": transform(rows)}}}},
    }
    raw = (
        json.dumps(source)
        .replace('"exact-original"', "0.123456789123456789")
        .replace('"exact-revised"', "-2.5e-3")
        .encode()
    )
    (root / "sec-facts.json").write_bytes(raw)
    return {
        "kind": "sec_company_facts",
        "source": {"ref": "sec-facts.json", "sha256": hashlib.sha256(raw).hexdigest()},
        "captured_at": "2026-11-18T15:20:00Z",
        "cik": submissions["cik"],
        "symbol": submissions["symbol"],
        "selectors": [
            {
                "accession": accession,
                "concept": "EarningsPerShareDiluted",
                "unit": "USD/shares",
                "period_start": "2026-07-01",
                "period_end": "2026-09-30",
            }
            for accession in (original, amendment)
        ],
    }


@pytest.mark.parametrize("visibility", ["source_published", "locally_ingested"])
def test_pinned_company_facts_join_exact_accessions_and_do_not_backdate(tmp_path, visibility):
    submissions = sec(tmp_path)
    facts = sec_facts(tmp_path, submissions)
    result = build_archive(tmp_path, archive([facts, submissions]))
    assert result == build_archive(tmp_path, archive([submissions, facts]))
    ref = pin(tmp_path, "archive.json", result)

    before = load_snapshot(
        tmp_path, ref, datetime.fromisoformat("2026-11-18T15:19:59Z"), visibility=visibility
    )
    after = load_snapshot(
        tmp_path, ref, datetime.fromisoformat(facts["captured_at"]), visibility=visibility
    )
    assert len(before.records) == 2
    assert all(row.revision == 1 and "facts" not in row.payload for row in before.records)
    assert [row.record_id for row in after.records] == submissions["accessions"]
    assert [row.payload["form"] for row in after.records] == ["10-Q", "10-Q/A"]
    assert [row.payload["facts"][0]["value"] for row in after.records] == [
        "0.123456789123456789", "-0.0025",
    ]
    assert [row.event_time for row in after.records] == [row.event_time for row in before.records]
    for row in after.records:
        assert row.revision == 2
        assert row.payload["company_facts_sha256"] == facts["source"]["sha256"]
        assert row.payload["source_sha256"] == submissions["source"]["sha256"]


def test_company_facts_wait_for_later_submissions_capture(tmp_path):
    submissions = sec(tmp_path)
    facts = sec_facts(tmp_path, submissions)
    submissions["captured_at"] = "2026-11-18T16:00:00Z"
    result = build_archive(tmp_path, archive([facts, submissions]))
    ref = pin(tmp_path, "archive.json", result)
    before = load_snapshot(tmp_path, ref, datetime.fromisoformat(facts["captured_at"]))
    after = load_snapshot(tmp_path, ref, datetime.fromisoformat(submissions["captured_at"]))
    assert before.records == ()
    assert [row.revision for row in after.records] == [2, 2]


@pytest.mark.parametrize(
    ("change", "message"),
    [
        ("different_symbol", "matching SEC submission"),
        ("before_acceptance", "before SEC acceptance"),
        ("different_form", "SEC submission form"),
        ("ambiguous_fact", "exactly one SEC fact"),
        ("missing_fact", "exactly one SEC fact"),
        ("duplicate_selector", "duplicate SEC fact selector"),
        ("unbounded_decimal", "SEC fact value"),
        ("changed_bytes", "SHA-256 mismatch"),
    ],
)
def test_company_facts_reject_unverifiable_or_ambiguous_capture(tmp_path, change, message):
    submissions = sec(tmp_path)

    def transform(rows):
        if change == "different_form":
            rows[0]["form"] = "10-K"
        if change == "ambiguous_fact":
            rows.append(rows[0] | {"val": 123})
        if change == "missing_fact":
            rows.pop()
        return rows

    facts = sec_facts(tmp_path, submissions, transform=transform)
    if change == "different_symbol":
        facts["symbol"] = "NVDA"
    if change == "before_acceptance":
        facts["captured_at"] = "2026-11-17T15:20:00Z"
    if change == "duplicate_selector":
        facts["selectors"].append(facts["selectors"][0])
    if change in {"unbounded_decimal", "changed_bytes"}:
        path = tmp_path / "sec-facts.json"
        raw = path.read_bytes().replace(b"-2.5e-3", b"2.5e+500000")
        path.write_bytes(raw)
        if change == "unbounded_decimal":
            facts["source"]["sha256"] = hashlib.sha256(raw).hexdigest()
    with pytest.raises(ValueError, match=message):
        build_archive(tmp_path, archive([submissions, facts]))


def test_pinned_sec_amendment_keeps_accession_and_uses_known_capture_time(tmp_path):
    capture = sec(tmp_path)
    result = build_archive(tmp_path, archive([capture]))
    assert [item["record_id"] for item in result["records"]] == capture["accessions"]
    assert result["records"][0]["event_time"] == "2026-11-16T16:30:00Z"
    assert result["records"][1]["payload"]["form"] == "10-Q/A"
    assert result["records"][1]["payload"]["source_sha256"] == capture["source"]["sha256"]
    ref = pin(tmp_path, "archive.json", result)
    early = load_snapshot(tmp_path, ref, datetime.fromisoformat("2026-11-17T20:14:59Z"))
    late = load_snapshot(tmp_path, ref, datetime.fromisoformat("2026-11-17T20:15:00Z"))
    assert early.records == ()
    assert len(late.records) == 2


def test_alfred_keeps_exact_revisions_and_defers_date_only_vintage_to_capture(tmp_path):
    first = alfred(tmp_path, "2026-11-16", "100.12345678", "2026-11-16T15:45:00Z")
    revised = alfred(tmp_path, "2026-11-17", "100.12345679", "2026-11-17T14:00:00Z")
    result = build_archive(tmp_path, archive([revised, first]))
    assert result == build_archive(tmp_path, archive([first, revised]))
    assert [item["revision"] for item in result["records"]] == [1, 2]
    ref = pin(tmp_path, "archive.json", result)
    before = load_snapshot(tmp_path, ref, datetime.fromisoformat("2026-11-17T13:59:59Z"))
    after = load_snapshot(tmp_path, ref, datetime.fromisoformat("2026-11-17T14:00:00Z"))
    assert before.records[0].payload["value"] == "100.12345678"
    assert after.records[0].payload["value"] == "100.12345679"


def test_import_refuses_byte_changes_ambiguous_vintage_and_future_sec_acceptance(tmp_path):
    capture = sec(tmp_path)
    capture["captured_at"] = "2026-11-17T19:00:00Z"
    with pytest.raises(ValueError, match="captured before acceptance"):
        build_archive(tmp_path, archive([capture]))
    (tmp_path / "sec.json").write_bytes(b"{}")
    with pytest.raises(ValueError, match="SHA-256 mismatch"):
        build_archive(tmp_path, archive([capture]))
    wrong = alfred(tmp_path, "2026-11-17", "102", "2026-11-17T14:00:00Z")
    wrong["vintage_date"] = "2026-11-16"
    with pytest.raises(ValueError, match="single declared vintage"):
        build_archive(tmp_path, archive([wrong]))


def test_published_manifest_schema_and_pins_match_importer():
    research = Path(__file__).resolve().parents[1]
    schema = research.parent.parent / "contracts/pit-capture-manifest.schema.json"
    assert json.loads(schema.read_text(encoding="utf-8")) == CaptureManifest.model_json_schema()
    root = research / "examples/pit-import"
    result = build_archive(root, json.loads((root / "manifest.json").read_text(encoding="utf-8")))
    assert result["source_mode"] == "synthetic_fixture"
    assert [record["kind"] for record in result["records"]] == [
        "filing",
        "filing",
        "macro",
        "macro",
    ]
    enriched = build_archive(
        root, json.loads((root / "sec-facts-manifest.json").read_text(encoding="utf-8"))
    )
    assert [row["revision"] for row in enriched["records"]] == [1, 2, 1, 2]
    assert enriched["records"][-1]["payload"]["facts"][0]["value"] == "0.123456789123456788"


@pytest.mark.parametrize("visibility", ["source_published", "locally_ingested"])
def test_reviewed_bls_releases_keep_official_values_without_backdating_receipt(
    tmp_path, visibility
):
    research = Path(__file__).resolve().parents[1]
    schema = research.parent.parent / "contracts/pit-bls-release.schema.json"
    assert json.loads(schema.read_text(encoding="utf-8")) == BLSReleaseReview.model_json_schema()
    root = research / "examples/pit-import"
    manifest = json.loads((root / "bls-manifest.json").read_text(encoding="utf-8"))
    result = build_archive(root, manifest)
    assert result["source_mode"] == "local_archive"
    assert [
        (row["revision"], row["payload"]["released_at"], row["payload"]["value"])
        for row in result["records"]
    ] == [
        (1, "2024-11-01T12:30:00Z", "12000"),
        (2, "2024-12-06T13:30:00Z", "36000"),
    ]
    for record, capture in zip(result["records"], manifest["captures"], strict=True):
        assert record["payload"]["availability_basis"] == "local_document_review"
        assert record["payload"]["source_sha256"] == capture["source"]["sha256"]
        assert record["available_at"] == record["ingested_at"] == capture["reviewed_at"]

    ref = pin(tmp_path, "archive.json", result)
    cutoffs = (
        ("2024-12-06T13:30:00Z", []),
        ("2026-11-18T11:10:28Z", []),
        ("2026-11-18T11:10:29Z", ["12000"]),
        ("2026-11-18T11:12:41Z", ["12000"]),
        ("2026-11-18T11:12:42Z", ["36000"]),
    )
    for cutoff, expected in cutoffs:
        snapshot = load_snapshot(
            tmp_path, ref, datetime.fromisoformat(cutoff), visibility=visibility
        )
        assert [row.payload["value"] for row in snapshot.records] == expected


def test_bls_import_rejects_duplicate_release_and_review_before_publication():
    root = Path(__file__).resolve().parents[1] / "examples/pit-import"
    manifest = json.loads((root / "bls-manifest.json").read_text(encoding="utf-8"))
    first, second = manifest["captures"]
    with pytest.raises(ValueError, match="duplicate BLS observation"):
        build_archive(root, manifest | {"captures": [first, first]})
    first["reviewed_at"] = "2024-11-01T12:29:59Z"
    with pytest.raises(ValueError, match="reviewed before its official release"):
        build_archive(root, manifest)
    first["reviewed_at"] = second["reviewed_at"]
    first["source"]["sha256"] = "0" * 64
    with pytest.raises(ValueError, match="SHA-256 mismatch"):
        build_archive(root, manifest)
