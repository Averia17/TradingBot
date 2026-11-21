"""Prepare and hand off frozen offline review windows to Go."""

import argparse
import hashlib
import json
import sqlite3
import subprocess
import tempfile
import time
from collections.abc import Callable
from contextlib import closing
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Annotated, Literal, Self

from pydantic import Field, JsonValue, TypeAdapter, model_validator

from tradingbot.contracts import Contract
from tradingbot.paper_cli import (
    MAX_DOCUMENT_BYTES,
    AdmissionReceipt,
    AdmissionState,
    AdmissionStop,
    AdmissionWindow,
    GoRunner,
    admit_occurrence,
    execute_cycle_command,
    invoke_paper_cli,
    parse_cycle_args,
    poll_admissions,
)
from tradingbot.paper_memory import LedgerID
from tradingbot.prepare_identity_review import prepare_identity_review
from tradingbot.snapshots import (
    ArchiveReference,
    CaptureTime,
    Identifier,
    decode_pinned_json,
    read_pinned_bytes,
    read_pinned_json,
)

_CaptureTime = TypeAdapter(CaptureTime)
_DB = "reviews.sqlite3"
_SCHEMA = """
CREATE TABLE IF NOT EXISTS owner_plan (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    identity BLOB NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS reviews (
    position INTEGER PRIMARY KEY CHECK (position BETWEEN 0 AND 63),
    request BLOB,
    request_sha256 TEXT,
    receipt BLOB,
    receipt_sha256 TEXT,
    CHECK ((request IS NULL) = (request_sha256 IS NULL)),
    CHECK ((receipt IS NULL) = (receipt_sha256 IS NULL)),
    CHECK (request IS NOT NULL OR receipt IS NOT NULL)
) STRICT;
"""


class ReviewEvent(Contract):
    kind: Literal["report", "news", "price", "liquidity", "policy", "review"]
    provider: Identifier
    record_id: Identifier


class ReviewWindow(Contract):
    opening_order_id: LedgerID
    as_of: CaptureTime
    release_at: CaptureTime
    max_lateness_seconds: Annotated[int, Field(strict=True, ge=1, le=3600)]
    data_manifest: ArchiveReference
    marks: ArchiveReference
    max_llm_calls: Annotated[int, Field(strict=True, ge=1, le=64)]
    event: ReviewEvent | None = None

    @property
    def deadline(self) -> datetime:
        return self.release_at + timedelta(seconds=self.max_lateness_seconds)

    @model_validator(mode="after")
    def ordered_times(self) -> Self:
        if self.as_of > self.release_at or self.deadline - self.as_of > timedelta(hours=1):
            raise ValueError(
                "review cutoff must precede release and be at most one hour from deadline"
            )
        return self


class ReviewCyclePlan(Contract):
    schema_version: Literal["0.1"]
    account_id: LedgerID
    schedule_id: Identifier
    policy: ArchiveReference
    identity_registry: ArchiveReference
    quote_tape: ArchiveReference
    occurrences: Annotated[tuple[ReviewWindow, ...], Field(min_length=1, max_length=64)]

    @model_validator(mode="after")
    def ordered_windows(self) -> Self:
        windows = [(row.release_at, row.as_of, row.opening_order_id) for row in self.occurrences]
        if windows != sorted(windows) or len(
            {(cutoff, order) for _, cutoff, order in windows}
        ) != len(windows):
            raise ValueError(
                "review windows must be sorted and unique per opening order and cutoff"
            )
        for row in self.occurrences:
            pins = (
                self.policy,
                self.identity_registry,
                self.quote_tape,
                row.data_manifest,
                row.marks,
            )
            if len({pin.ref for pin in pins}) != 5 or len({pin.sha256 for pin in pins}) != 5:
                raise ValueError("each review window requires independent input pins")
        return self


class _Receipt(AdmissionReceipt):
    unadmitted = frozenset({"not_due", "missed"})
    review_schedule_schema_version: Literal["0.2"]
    status: Literal[
        "not_due",
        "missed",
        "queued",
        "leased",
        "completed",
        "failed",
        "expired",
        "already_scheduled",
    ]
    trigger: dict[str, JsonValue] | None = None
    snapshot: dict[str, JsonValue] | None = None

    @model_validator(mode="after")
    def durable_receipt(self) -> Self:
        if self.status in self.unadmitted and (
            self.trigger is not None or self.snapshot is not None
        ):
            raise ValueError("unadmitted review cannot include a trigger or snapshot")
        if self.trigger is not None and self.trigger.get("trigger_id") != self.job_id:
            raise ValueError("review trigger does not identify the acknowledged job")
        return self


class _NotOpen(Exception):
    pass


def _encode(value: dict) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def _sha(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _open_journal(connection: sqlite3.Connection, identity: bytes) -> None:
    connection.execute("PRAGMA synchronous=FULL")
    connection.executescript(_SCHEMA)
    with connection:
        connection.execute("INSERT OR IGNORE INTO owner_plan VALUES (1, ?)", (identity,))
        stored = connection.execute("SELECT identity FROM owner_plan WHERE singleton=1").fetchone()
        if stored != (identity,):
            raise ValueError("review journal belongs to a different immutable owner plan")


@dataclass(frozen=True)
class _Journal:
    connection: sqlite3.Connection
    position: int

    def load(self) -> AdmissionState[bytes]:
        row = self.connection.execute(
            "SELECT request, request_sha256, receipt, receipt_sha256 FROM reviews WHERE position=?",
            (self.position,),
        ).fetchone()
        if row is None:
            return AdmissionState()
        request, request_hash, receipt, receipt_hash = row
        for data, digest in ((request, request_hash), (receipt, receipt_hash)):
            if data is not None and (len(data) > MAX_DOCUMENT_BYTES or _sha(data) != digest):
                raise ValueError("review journal payload checksum or size differs")
        if receipt is None:
            return AdmissionState(request=request)
        outcome = decode_pinned_json(receipt)
        if not isinstance(outcome, dict):
            raise ValueError("review journal receipt must be a JSON object")
        return AdmissionState(request, outcome)

    def stage(self, request: bytes) -> AdmissionState[bytes]:
        with self.connection:
            self.connection.execute(
                "INSERT OR IGNORE INTO reviews(position,request,request_sha256) VALUES (?,?,?)",
                (self.position, request, _sha(request)),
            )
        # Concurrent runners use the first durable bytes and the same Go identity.
        return self.load()

    def finish(self, receipt: dict, *, submitted: bool) -> AdmissionState[bytes]:
        raw = _encode(receipt)
        with self.connection:
            if submitted:
                self.connection.execute(
                    "UPDATE reviews SET receipt=?, receipt_sha256=? WHERE position=? AND receipt IS NULL",
                    (raw, _sha(raw), self.position),
                )
            else:
                self.connection.execute(
                    "INSERT OR IGNORE INTO reviews(position,receipt,receipt_sha256) VALUES (?,?,?)",
                    (self.position, raw, _sha(raw)),
                )
        return self.load()


def _prepare(root: Path, plan: ReviewCyclePlan, window: ReviewWindow, invoke: GoRunner) -> bytes:
    def read(*arguments):
        output = invoke(*arguments)
        if (
            arguments[0] == "review"
            and output.get("paper_thesis_schema_version") == "0.2"
            and output.get("account_id") == plan.account_id
            and output.get("opening_order_id") == window.opening_order_id
            and output.get("state") in {"pending", "unopened", "closed"}
            and _CaptureTime.validate_python(output.get("as_of")) == window.as_of
        ):
            raise _NotOpen
        return output

    request = prepare_identity_review(
        invoke=read,
        archive_root=root,
        account=plan.account_id,
        order=window.opening_order_id,
        as_of=window.as_of.isoformat(),
        policy=read_pinned_bytes(root, plan.policy),
        marks=read_pinned_bytes(root, window.marks),
        data_manifest=window.data_manifest,
        identity_registry=plan.identity_registry,
        quote_tape=plan.quote_tape,
        max_llm_calls=window.max_llm_calls,
    )
    if window.event is not None:
        request["event"] = window.event.model_dump(mode="json")
    raw = _encode(request)
    if len(raw) > MAX_DOCUMENT_BYTES:
        raise ValueError("prepared review exceeds Go's document limit")
    return raw


def _submit(
    root: Path, plan: ReviewCyclePlan, window: ReviewWindow, raw: bytes, invoke: GoRunner
) -> dict:
    request = decode_pinned_json(raw)
    expected_event = window.event.model_dump(mode="json") if window.event is not None else None
    if (
        not isinstance(request, dict)
        or request.get("review_schedule_schema_version") != "0.2"
        or request.get("opening_order_id") != window.opening_order_id
        or request.get("max_llm_calls") != window.max_llm_calls
        or request.get("data_manifest") != window.data_manifest.model_dump(mode="json")
        or request.get("event") != expected_event
    ):
        raise ValueError("staged review differs from the owner window")
    context, portfolio, binding = (
        request.get(key) for key in ("snapshot_context", "portfolio", "identity_binding")
    )
    if (
        not isinstance(context, dict)
        or context.get("account_id") != plan.account_id
        or not isinstance(portfolio, dict)
        or _CaptureTime.validate_python(portfolio.get("as_of")) != window.as_of
        or not isinstance(binding, dict)
        or binding.get("identity_registry") != plan.identity_registry.model_dump(mode="json")
        or binding.get("quote_tape") != plan.quote_tape.model_dump(mode="json")
    ):
        raise ValueError("staged review lost its exact account, cutoff or owner pins")
    max_age = (window.deadline - window.as_of) // timedelta(microseconds=1)
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "review.json"
        path.write_bytes(raw)
        return invoke(
            "schedule-review",
            "--config",
            str(path),
            "--archive-root",
            str(root),
            "--identity-registry-sha256",
            plan.identity_registry.sha256,
            "--max-research-age",
            f"{max_age}us",
            "--roll-forward",
        )


def _drive_window(
    root: Path,
    journal: _Journal,
    plan: ReviewCyclePlan,
    window: ReviewWindow,
    invoke: GoRunner,
    clock: Callable[[], datetime],
) -> dict:
    def prepare() -> bytes | AdmissionStop:
        try:
            return _prepare(root, plan, window, invoke)
        except _NotOpen:
            return AdmissionStop("not_open", terminal=True)
        except ValueError as exc:
            return AdmissionStop("refused", exc, terminal=True)
        except (OSError, subprocess.SubprocessError) as exc:
            return AdmissionStop("pending_prepare", exc)

    def submit(raw: bytes) -> dict | AdmissionStop:
        try:
            response = _submit(root, plan, window, raw, invoke)
        except (OSError, subprocess.SubprocessError) as exc:
            return AdmissionStop("pending_go", exc)
        try:
            receipt = _Receipt.model_validate(response)
        except ValueError as exc:
            return AdmissionStop("pending_go", exc)
        return receipt.model_dump(
            mode="json", include={"status", "created", "job_id", "created_at"}, exclude_none=True
        )

    return admit_occurrence(
        AdmissionWindow(window.release_at, window.deadline, "not_released", check_prepared_deadline=True),
        journal,
        prepare,
        submit,
        clock,
    )


def run_cycles(
    root: Path,
    plan_ref: ArchiveReference | dict,
    journal: Path,
    invoke: GoRunner,
    *,
    trusted_identity_registry_sha256: str | None,
    clock: Callable[[], datetime] = lambda: datetime.now(UTC),
) -> dict:
    pin = ArchiveReference.model_validate(plan_ref)
    plan = ReviewCyclePlan.model_validate(read_pinned_json(root, pin))
    if trusted_identity_registry_sha256 != plan.identity_registry.sha256:
        raise ValueError("review cycles require the matching independent owner registry SHA-256")
    identity = _encode(
        {"account_id": plan.account_id, "schedule_id": plan.schedule_id, "plan": pin.model_dump()}
    )
    journal.mkdir(parents=True, exist_ok=True)
    with closing(sqlite3.connect(journal / _DB, timeout=5)) as connection:
        _open_journal(connection, identity)
        outcomes = [
            {
                "opening_order_id": window.opening_order_id,
                "as_of": window.as_of.isoformat(),
                "release_at": window.release_at.isoformat(),
            }
            | _drive_window(root, _Journal(connection, position), plan, window, invoke, clock)
            for position, window in enumerate(plan.occurrences)
        ]
    return {
        "review_cycle_report_schema_version": "0.1",
        "owner_plan_sha256": pin.sha256,
        "occurrences": outcomes,
    }


def poll_cycles(
    root: Path,
    reference: ArchiveReference,
    journal: Path,
    invoke: GoRunner,
    interval: int,
    emit: Callable[[dict], None],
    *,
    trusted_identity_registry_sha256: str | None,
) -> None:
    poll_admissions(
        lambda: run_cycles(
            root,
            reference,
            journal,
            invoke,
            trusted_identity_registry_sha256=trusted_identity_registry_sha256,
        ),
        emit,
        interval,
        waiting_status="not_released",
        release_field="release_at",
        retry_statuses={"pending_prepare", "pending_go"},
        clock=lambda: datetime.now(UTC),
        wait=time.sleep,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", action="store_true", help="Print the owner review plan schema")
    args = parse_cycle_args(parser)
    if args.schema:
        print(json.dumps(ReviewCyclePlan.model_json_schema(), indent=2))
        return 0

    def invoke(*arguments: str) -> dict:
        try:
            return invoke_paper_cli(binary, *arguments)
        except ValueError:
            raise subprocess.SubprocessError("Go review receipt is invalid") from None

    try:
        binary = args.paper_cli.resolve(strict=True)
        if not binary.is_file():
            raise ValueError("paper must be an explicit local binary")
        execute_cycle_command(args, invoke, run_cycles, poll_cycles)
    except (ValueError, OSError, OverflowError, sqlite3.Error) as exc:
        parser.exit(2, f"review cycles rejected ({type(exc).__name__})\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
