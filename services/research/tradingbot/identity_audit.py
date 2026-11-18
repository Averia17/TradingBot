"""Verify identity-bound research exports without running an analyst or a replay."""

import base64
import binascii
import hashlib
import json
import re
from datetime import datetime
from pathlib import Path
from typing import Annotated, Literal

from pydantic import Field, TypeAdapter

from tradingbot.contracts import (
    DECIMAL_PATTERN,
    Contract,
    DecisionBatch,
    PortfolioSnapshot,
    Symbol,
)
from tradingbot.security_identity import (
    SecurityBinding,
    load_identity_registry,
    project_identity_snapshot,
)
from tradingbot.snapshots import (
    SHA256,
    ArchiveReference,
    CaptureTime,
    Identifier,
    _unique_keys,
    load_snapshot,
)

MAX_IDENTITY_EXPORT_BYTES = 48 << 20
_MAX_PAYLOAD_BYTES = 32 << 20
_MAX_BASE64_CHARS = 4 * ((_MAX_PAYLOAD_BYTES + 2) // 3)
_RECEIPT_TIME = r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?Z$"
_JobID = Annotated[str, Field(pattern=r"^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$")]
_Attempt = Annotated[int, Field(strict=True, ge=1, le=3)]
_CallCount = Annotated[int, Field(strict=True, ge=1, le=64)]
_Usage = Annotated[int, Field(strict=True, ge=0, le=2**64 - 1)]
_Bindings = Annotated[list[SecurityBinding], Field(min_length=1, max_length=100)]
_ReportKey = Literal[
    "market_report",
    "fundamentals_report",
    "news_report",
    "sentiment_report",
    "trader_investment_plan",
    "final_trade_decision",
]
_wire_time = TypeAdapter(CaptureTime)


class _ExportRow(Contract):
    job_id: _JobID
    attempt: _Attempt
    created_at: Annotated[str, Field(pattern=_RECEIPT_TIME)]
    completed_at: Annotated[str, Field(pattern=_RECEIPT_TIME)]
    request_base64: Annotated[str, Field(min_length=4, max_length=_MAX_BASE64_CHARS)]
    request_sha256: SHA256
    result_base64: Annotated[str, Field(min_length=4, max_length=_MAX_BASE64_CHARS)]
    result_sha256: SHA256


class _Export(Contract):
    schema_version: Literal["0.1"]
    jobs: Annotated[list[_ExportRow], Field(min_length=1, max_length=32)]


class _Job(Contract):
    job_schema_version: Literal["0.2"]
    job_id: _JobID
    portfolio: PortfolioSnapshot
    requested_symbols: Annotated[list[Symbol], Field(min_length=1, max_length=100)]
    data_manifest: ArchiveReference
    policy_version: Identifier
    max_llm_calls: _CallCount
    identity_registry: ArchiveReference
    security_bindings: _Bindings


class _Provenance(Contract):
    job_id: _JobID
    attempt: _Attempt
    policy_version: Identifier
    data_manifest: ArchiveReference
    worker_version: Identifier
    upstream_version: Identifier
    upstream_sha: Annotated[str, Field(pattern=r"^[0-9a-f]{40}$")]
    config_sha256: SHA256
    model_id: Literal["scripted-offline-v1"]
    identity_registry: ArchiveReference
    security_bindings: _Bindings
    projected_snapshot_sha256: SHA256


class _Metadata(Contract):
    source_mode: Literal["tradingagents_offline"]
    alpha_measured: Annotated[bool, Field(strict=True)]
    intraday_pit_verified: Annotated[bool, Field(strict=True)]
    llm_calls: _CallCount
    input_tokens: _Usage
    output_tokens: _Usage
    missing_usage_calls: Annotated[int, Field(strict=True, ge=0, le=64)]
    usd_cost: None
    provenance: _Provenance


class _Result(Contract):
    result_schema_version: Literal["0.3"]
    batch: DecisionBatch
    reports: Annotated[dict[Symbol, dict[_ReportKey, str]], Field(min_length=1, max_length=100)]
    metadata: _Metadata


def _reject_constant(_name: str):
    raise ValueError("nonfinite JSON numbers are unsupported")


def _decode(raw: bytes):
    return json.loads(
        raw.decode("utf-8"),
        object_pairs_hook=_unique_keys,
        parse_constant=_reject_constant,
    )


def _payload(encoded: str, expected: str, label: str) -> bytes:
    try:
        raw = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise ValueError(f"invalid research {label} base64") from exc
    if not raw or base64.b64encode(raw).decode("ascii") != encoded:
        raise ValueError(f"noncanonical research {label} base64")
    if hashlib.sha256(raw).hexdigest() != expected:
        raise ValueError(f"research {label} SHA-256 does not match")
    return raw


def _money(value: object) -> None:
    if not isinstance(value, str) or re.fullmatch(DECIMAL_PATTERN, value) is None:
        raise ValueError("stored research money must be an exact decimal string")


def _request(raw: bytes) -> _Job:
    wire = _decode(raw)
    job = _Job.model_validate(wire)
    portfolio = wire["portfolio"]
    if portfolio.keys() != PortfolioSnapshot.model_fields.keys():
        raise ValueError("incomplete stored research portfolio")
    _wire_time.validate_python(portfolio["as_of"])
    for key in ("cash_usd", "reserved_cash_usd", "max_position_pct"):
        _money(portfolio[key])
    for position in portfolio["positions"]:
        _money(position["quantity"])
        _money(position["mark_price_usd"])
    if (
        [binding.symbol for binding in job.security_bindings] != job.requested_symbols
        or len(set(job.requested_symbols)) != len(job.requested_symbols)
        or len({binding.security_id for binding in job.security_bindings})
        != len(job.security_bindings)
    ):
        raise ValueError("security bindings do not match distinct requested symbols in order")
    return job


def _result(raw: bytes) -> _Result:
    wire = _decode(raw)
    result = _Result.model_validate(wire)
    batch = wire["batch"]
    if batch.keys() != DecisionBatch.model_fields.keys():
        raise ValueError("incomplete stored decision batch")
    for field in ("as_of", "created_at", "expires_at"):
        _wire_time.validate_python(batch[field])
    for intent in batch["intents"]:
        _money(intent["notional_usd"])
    return result


def _validate_record(row: _ExportRow, job: _Job, result: _Result, owner: ArchiveReference) -> None:
    if job.job_id != row.job_id:
        raise ValueError("exported research job ID does not match the stored request")
    if job.identity_registry != owner:
        raise ValueError("stored job does not match the owner registry pin")
    metadata = result.metadata
    proof = metadata.provenance
    if (
        proof.job_id != row.job_id
        or proof.attempt != row.attempt
        or proof.policy_version != job.policy_version
        or proof.data_manifest != job.data_manifest
        or proof.identity_registry != owner
        or proof.security_bindings != job.security_bindings
        or metadata.alpha_measured
        or metadata.intraday_pit_verified
        or not metadata.missing_usage_calls <= metadata.llm_calls <= job.max_llm_calls
        or metadata.missing_usage_calls == metadata.llm_calls
        and (metadata.input_tokens or metadata.output_tokens)
    ):
        raise ValueError("stored result provenance differs from the accepted job or attempt")

    batch = result.batch
    if (
        batch.portfolio_snapshot_id != job.portfolio.snapshot_id
        or batch.as_of != job.portfolio.as_of
    ):
        raise ValueError("stored decision batch does not match its portfolio snapshot")
    created, completed = (
        datetime.fromisoformat(row.created_at),
        datetime.fromisoformat(row.completed_at),
    )
    if not (
        batch.as_of <= created <= completed
        and batch.as_of <= batch.created_at <= completed < batch.expires_at
    ):
        raise ValueError("inconsistent research timestamps in stored job or result")
    symbols = set(job.requested_symbols)
    if set(result.reports) != symbols or {intent.symbol for intent in batch.intents} != symbols:
        raise ValueError("stored reports and decisions must cover the exact requested symbols")
    for reports in result.reports.values():
        if not reports.get("final_trade_decision") or any(
            not text.strip() for text in reports.values()
        ):
            raise ValueError("stored result has missing or blank analyst reports")
    hashes = {
        f"{symbol}:{field}": hashlib.sha256(text.encode("utf-8")).hexdigest()
        for symbol, reports in result.reports.items()
        for field, text in reports.items()
    }
    if hashes != batch.evidence_hashes:
        raise ValueError("stored report evidence hashes do not match")


def audit_identity_export(
    raw: bytes,
    expected_sha256: str,
    archive_root: Path,
    owner_registry: ArchiveReference | dict,
) -> dict:
    """Verify a Go export and reproduce each pinned identity projection from local bytes."""
    if not raw or len(raw) > MAX_IDENTITY_EXPORT_BYTES:
        raise ValueError("research identity export exceeds its document limit or is empty")
    actual = hashlib.sha256(raw).hexdigest()
    if actual != expected_sha256:
        raise ValueError("research identity export SHA-256 does not match")
    export = _Export.model_validate(_decode(raw))
    if len({row.job_id for row in export.jobs}) != len(export.jobs):
        raise ValueError("duplicate job in research identity export")
    order = [(datetime.fromisoformat(row.completed_at), row.job_id) for row in export.jobs]
    if order != sorted(order):
        raise ValueError("research identity export jobs are not ordered by completion and job ID")

    owner = ArchiveReference.model_validate(owner_registry)
    registry = None
    jobs = []
    total = 0
    for row in export.jobs:
        request = _payload(row.request_base64, row.request_sha256, "request")
        result = _payload(row.result_base64, row.result_sha256, "result")
        total += len(request) + len(result)
        if total > _MAX_PAYLOAD_BYTES:
            raise ValueError("research identity export exceeds its decoded payload limit")
        job, stored = _request(request), _result(result)
        _validate_record(row, job, stored, owner)
        if registry is None:
            registry = load_identity_registry(archive_root, owner)
        snapshot = load_snapshot(
            archive_root,
            job.data_manifest,
            job.portfolio.as_of,
            visibility="locally_ingested",
        )
        projected = project_identity_snapshot(
            snapshot,
            registry,
            owner,
            job.security_bindings,
            positions=job.portfolio.positions,
        )
        projection_hash = projected.sha256
        if projection_hash != stored.metadata.provenance.projected_snapshot_sha256:
            raise ValueError("projected snapshot SHA-256 does not match the stored result")
        jobs.append(
            row.model_dump(exclude={"request_base64", "result_base64"})
            | {
                "data_manifest": job.data_manifest.model_dump(),
                "security_bindings": [binding.model_dump() for binding in job.security_bindings],
                "projected_snapshot_sha256": projection_hash,
            }
        )
    return {
        "schema_version": "0.1",
        "status": "verified",
        "export_sha256": actual,
        "identity_registry": owner.model_dump(),
        "jobs": jobs,
    }
