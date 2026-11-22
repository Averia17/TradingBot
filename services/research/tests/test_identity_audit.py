import base64
import hashlib
import json
from datetime import datetime
from pathlib import Path

import pytest

from tradingbot.identity_audit import MAX_IDENTITY_EXPORT_BYTES, audit_identity_export
from tradingbot.prepared_analysis import build_prepared_reports
from tradingbot.security_identity import load_identity_snapshot

ROOT = Path(__file__).resolve().parents[3]
FIXTURES = ROOT / "services/research/examples/selection"
CREATED = "2026-11-18T10:01:00Z"
COMPLETED = "2026-11-18T10:02:00Z"


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


@pytest.fixture
def case(tmp_path):
    job = json.loads((ROOT / "contracts/valid-research-identity-job.json").read_bytes())
    result = json.loads(
        (ROOT / "contracts/valid-research-identity-result-request.json").read_bytes()
    )
    result.pop("worker_id")
    result.pop("lease_token")
    for field, filename in {
        "data_manifest": "identity-market.json",
        "identity_registry": "identity-registry.json",
    }.items():
        source = (FIXTURES / filename).read_bytes()
        (tmp_path / filename).write_bytes(source)
        job[field] = {"ref": filename, "sha256": digest(source)}
    job["portfolio"]["as_of"] = "2026-11-18T10:00:00Z"
    job["security_bindings"] = [
        {"symbol": "TSLA", "security_id": "fixture-current"},
        {"symbol": "NVDA", "security_id": "fixture-nvda"},
    ]
    projected = load_identity_snapshot(
        tmp_path,
        job["data_manifest"],
        job["identity_registry"],
        datetime.fromisoformat(job["portfolio"]["as_of"]),
        job["security_bindings"],
    )
    result["metadata"]["provenance"].update(
        data_manifest=job["data_manifest"].copy(),
        identity_registry=job["identity_registry"].copy(),
        security_bindings=[binding.copy() for binding in job["security_bindings"]],
        projected_snapshot_sha256=projected.sha256,
    )
    result["batch"].update(
        as_of=job["portfolio"]["as_of"],
        created_at=CREATED,
        expires_at="2026-11-18T10:30:00Z",
    )
    return job, result, job["identity_registry"].copy()


def row(job, result):
    request = encoded(job)
    content = encoded(result)
    return {
        "job_id": job["job_id"],
        "attempt": 1,
        "created_at": CREATED,
        "completed_at": COMPLETED,
        "request_base64": base64.b64encode(request).decode(),
        "request_sha256": digest(request),
        "result_base64": base64.b64encode(content).decode(),
        "result_sha256": digest(content),
    }


def export(*rows):
    return encoded({"schema_version": "0.1", "jobs": rows}) + b"\n"


def verify(payload, tmp_path, owner):
    return audit_identity_export(payload, digest(payload), tmp_path, owner)


def test_audit_reconstructs_the_pinned_identity_and_preserves_exact_go_receipt(tmp_path, case):
    job, result, owner = case
    receipt = row(job, result)
    raw = export(receipt)
    audit = verify(raw, tmp_path, owner)
    assert audit == {
        "schema_version": "0.1",
        "status": "verified",
        "export_sha256": digest(raw),
        "identity_registry": owner,
        "jobs": [
            {
                **{key: value for key, value in receipt.items() if not key.endswith("_base64")},
                "data_manifest": job["data_manifest"],
                "security_bindings": job["security_bindings"],
                "projected_snapshot_sha256": result["metadata"]["provenance"][
                    "projected_snapshot_sha256"
                ],
            }
        ],
    }


def test_audit_validates_body_size_outer_digest_and_duplicate_json_first(tmp_path, case):
    job, result, owner = case
    receipt = row(job, result)
    raw = export(receipt)
    with pytest.raises(ValueError, match="export SHA-256"):
        audit_identity_export(raw, "0" * 64, tmp_path, owner)
    with pytest.raises(ValueError, match="document limit"):
        audit_identity_export(b" " * (MAX_IDENTITY_EXPORT_BYTES + 1), "0" * 64, tmp_path, owner)
    malformed = raw.replace(
        b'"schema_version":"0.1"', b'"schema_version":"0.1","schema_version":"0.1"', 1
    )
    with pytest.raises(ValueError, match="duplicate JSON"):
        verify(malformed, tmp_path, owner)
    tampered = b'{"job_schema_version":"0.2",' + encoded(job)[1:]
    with pytest.raises(ValueError, match="duplicate JSON"):
        verify(
            export(
                receipt
                | {
                    "request_base64": base64.b64encode(tampered).decode(),
                    "request_sha256": digest(tampered),
                }
            ),
            tmp_path,
            owner,
        )


def test_audit_preserves_go_completion_order_for_distinct_jobs(tmp_path, case):
    job, result, owner = case
    first = row(job, result)
    other_id = "00000000-0000-0000-0000-000000000005"
    result["metadata"]["provenance"]["job_id"] = other_id
    earlier_id = row(job | {"job_id": other_id}, result)
    audit = verify(export(earlier_id, first), tmp_path, owner)
    assert [receipt["job_id"] for receipt in audit["jobs"]] == [other_id, job["job_id"]]
    with pytest.raises(ValueError, match="ordered by completion"):
        verify(export(first, earlier_id), tmp_path, owner)


@pytest.mark.parametrize("source", ["local_archive", "synthetic_fixture"])
def test_prepared_audit_rebuilds_reports_from_the_pinned_local_archive(tmp_path, case, source):
    job, result, owner = case
    archive_path = tmp_path / job["data_manifest"]["ref"]
    archive = json.loads(archive_path.read_bytes())
    archive["source_mode"] = source
    template = next(record for record in archive["records"] if record["kind"] == "filing")
    for binding in job["security_bindings"]:
        for kind in ("filing", "news"):
            symbol = binding["symbol"]
            archive["records"].append(
                template | {
                    "kind": kind,
                    "record_id": f"example-{kind}-{symbol}",
                    "symbol": symbol,
                    "payload": {"headline": "Archive evidence", "security_id": binding["security_id"]},
                }
            )
    archive_bytes = encoded(archive)
    archive_path.write_bytes(archive_bytes)
    job["data_manifest"]["sha256"] = digest(archive_bytes)
    job.update(job_schema_version="0.3", analysis_mode="prepared_provider", max_llm_calls=1)
    projected = load_identity_snapshot(
        tmp_path,
        job["data_manifest"],
        job["identity_registry"],
        datetime.fromisoformat(job["portfolio"]["as_of"]),
        job["security_bindings"],
    )
    result["result_schema_version"] = "0.4"
    result["reports"] = build_prepared_reports(projected, job["requested_symbols"])
    result["metadata"].update(
        source_mode="prepared_archive_research", llm_calls=1, missing_usage_calls=1,
    )
    result["metadata"]["provenance"].update(
        data_manifest=job["data_manifest"],
        projected_snapshot_sha256=projected.sha256,
        archive_source_mode="local_archive",
        model_provider="openai",
        model_id="gpt-6-sol",
    )

    def seal():
        result["batch"]["evidence_hashes"] = {
            f"{symbol}:{field}": digest(text.encode())
            for symbol, reports in result["reports"].items()
            for field, text in reports.items()
        }
        return export(row(job, result))

    raw = seal()
    if source == "synthetic_fixture":
        with pytest.raises(ValueError, match="local archive"):
            verify(raw, tmp_path, owner)
        return
    assert verify(raw, tmp_path, owner)["status"] == "verified"
    result["reports"]["TSLA"]["market_report"] += "\nchanged"
    with pytest.raises(ValueError, match="prepared reports differ"):
        verify(seal(), tmp_path, owner)
    job["job_schema_version"] = "0.2"
    del job["analysis_mode"]
    with pytest.raises(ValueError, match="result version"):
        verify(seal(), tmp_path, owner)


@pytest.mark.parametrize(
    ("change", "message"),
    [
        ("duplicate", "duplicate job"),
        ("old-job", "0.2"),
        ("old-result", "0.3"),
        ("different-attempt", "provenance"),
        ("different-job-id", "job ID"),
        ("different-owner-pin", "owner registry"),
        ("different-registry-in-result", "provenance"),
        ("reversed-result-bindings", "provenance"),
        ("reversed-job-bindings", "requested symbols"),
        ("different-snapshot-id", "portfolio snapshot"),
        ("different-snapshot-time", "portfolio snapshot"),
        ("future-decision", "timestamps"),
        ("changed-result-hash", "result SHA-256"),
        ("malformed-base64", "base64"),
        ("changed-report", "evidence hash"),
        ("changed-projection", "projected snapshot SHA-256"),
        ("changed-source-bytes", "archive SHA-256"),
        ("late-clock-fraction", "timestamp"),
    ],
)
def test_audit_fails_closed_when_inputs_or_identity_proof_differ(tmp_path, case, change, message):
    job, result, owner = case
    proof = result["metadata"]["provenance"]
    if change == "old-job":
        job["job_schema_version"] = "0.1"
    if change == "old-result":
        result["result_schema_version"] = "0.2"
    if change == "different-attempt":
        proof["attempt"] = 2
    if change == "different-owner-pin":
        owner["ref"] = "other-registry.json"
    if change == "different-registry-in-result":
        proof["identity_registry"]["sha256"] = "0" * 64
    if change == "reversed-result-bindings":
        proof["security_bindings"].reverse()
    if change == "reversed-job-bindings":
        job["security_bindings"].reverse()
    if change == "different-snapshot-id":
        result["batch"]["portfolio_snapshot_id"] = "00000000-0000-0000-0000-000000000009"
    if change == "different-snapshot-time":
        result["batch"]["as_of"] = "2026-11-18T09:59:00Z"
    if change == "future-decision":
        result["batch"]["created_at"] = "2026-11-18T10:03:00Z"
    if change == "changed-report":
        result["reports"]["TSLA"]["market_report"] += " changed"
    if change == "changed-projection":
        proof["projected_snapshot_sha256"] = "0" * 64
    if change == "changed-source-bytes":
        (tmp_path / job["data_manifest"]["ref"]).write_bytes(b"{}")
    if change == "late-clock-fraction":
        result["batch"]["as_of"] = "2026-11-18T10:00:00.000000001Z"
    receipt = row(job, result)
    if change == "different-job-id":
        receipt["job_id"] = "00000000-0000-0000-0000-000000000009"
    if change == "changed-result-hash":
        receipt["result_sha256"] = "0" * 64
    if change == "malformed-base64":
        receipt["request_base64"] += "!"
    raw = export(receipt, receipt) if change == "duplicate" else export(receipt)
    with pytest.raises(ValueError, match=message):
        verify(raw, tmp_path, owner)
