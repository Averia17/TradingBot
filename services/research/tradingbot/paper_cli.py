"""Local paper command boundary for research and cycle runners."""

import argparse
import json
import os
import subprocess
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Annotated, ClassVar, Literal, Protocol, Self

from pydantic import Field, JsonValue, model_validator

from tradingbot.contracts import Contract
from tradingbot.paper_memory import LedgerID
from tradingbot.snapshots import (
    SHA256,
    ArchiveReference,
    CaptureTime,
    decode_pinned_json,
    read_pinned_bytes,
)

MAX_DOCUMENT_BYTES = 256 * 1024
MAX_QUOTE_TAPE_BYTES = 2 * 1024 * 1024
GoRunner = Callable[..., dict]


class AdmissionReceipt(Contract):
    unadmitted: ClassVar[frozenset[str]]
    status: str
    created: Annotated[bool, Field(strict=True)]
    job_id: LedgerID | None = None
    created_at: CaptureTime | None = None

    @model_validator(mode="after")
    def validate_acceptance(self) -> Self:
        has_identity = self.job_id is not None and self.created_at is not None
        if self.status in self.unadmitted:
            if self.created or self.job_id is not None or self.created_at is not None:
                raise ValueError("unadmitted occurrence cannot claim a durable job")
        elif not has_identity or (self.created and self.status != "queued"):
            raise ValueError("accepted occurrence lacks a consistent Go job receipt")
        return self


class _QuoteTape(Contract):
    paper_quote_tape_schema_version: Literal["0.3"]
    source_mode: Literal["synthetic_fixture"]
    account_id: LedgerID
    identity_registry_sha256: SHA256
    observations: Annotated[list[dict[str, JsonValue]], Field(max_length=2048)]


def check_owner_quote_tape(
    root: Path, pin: ArchiveReference, account: str, registry: ArchiveReference
) -> None:
    raw = read_pinned_bytes(root, pin)
    if len(raw) > MAX_QUOTE_TAPE_BYTES:
        raise ValueError("paper quote tape exceeds the document limit")
    tape = _QuoteTape.model_validate(decode_pinned_json(raw))
    if tape.account_id != account or tape.identity_registry_sha256 != registry.sha256:
        raise ValueError("paper quote tape differs from the owner account or registry")
    for observation in tape.observations:
        quote = observation.get("quote")
        if (
            not isinstance(quote, dict)
            or quote.get("paper_quote_schema_version") != "0.3"
            or quote.get("identity_registry_sha256") != registry.sha256
        ):
            raise ValueError("paper quote differs from its tape's identity registry")


def read_paper_snapshot(
    invoke: GoRunner, *, account: str, as_of: str, policy: bytes, marks: bytes
) -> dict:
    """Pass the checked policy and marks bytes to the read-only Go command."""
    with tempfile.TemporaryDirectory() as directory:
        folder = Path(directory)
        policy_path, marks_path = folder / "policy.json", folder / "marks.json"
        policy_path.write_bytes(policy)
        marks_path.write_bytes(marks)
        return invoke(
            "snapshot", "--account", account, "--policy", str(policy_path),
            "--marks", str(marks_path), "--as-of", as_of,
        )


@dataclass(frozen=True)
class AdmissionWindow:
    release_at: datetime
    deadline: datetime
    waiting_status: str
    check_prepared_deadline: bool = False


@dataclass(frozen=True)
class AdmissionState[Request]:
    request: Request | None = None
    receipt: dict | None = None


class AdmissionJournal[Request](Protocol):
    def load(self) -> AdmissionState[Request]: ...

    def stage(self, request: Request) -> AdmissionState[Request]: ...

    def finish(self, receipt: dict, *, submitted: bool) -> AdmissionState[Request]: ...


@dataclass(frozen=True)
class AdmissionStop:
    status: str
    error: Exception | None = None
    terminal: bool = False

    def report(self) -> dict:
        report = {"status": self.status, "created": False}
        if self.error is not None:
            report["error"] = type(self.error).__name__
        return report


def admit_occurrence[Request](
    window: AdmissionWindow,
    journal: AdmissionJournal[Request],
    prepare: Callable[[], Request | AdmissionStop],
    submit: Callable[[Request], dict | AdmissionStop],
    clock: Callable[[], datetime],
) -> dict:
    """Send only the winning staged request, even after its original release window closes."""
    state = journal.load()
    if state.request is None and state.receipt is None:
        now = clock()
        if now < window.release_at:
            return AdmissionStop(window.waiting_status).report()
        prepared = (
            AdmissionStop("missed", terminal=True) if now > window.deadline else prepare()
        )
        if (
            not isinstance(prepared, AdmissionStop)
            and window.check_prepared_deadline
            and clock() > window.deadline
        ):
            prepared = AdmissionStop("missed", terminal=True)
        if isinstance(prepared, AdmissionStop):
            if not prepared.terminal:
                return prepared.report()
            state = journal.finish(prepared.report(), submitted=False)
        else:
            state = journal.stage(prepared)
    if state.receipt is not None:
        return state.receipt
    if state.request is None:
        raise ValueError("admission journal lost its staged request")
    receipt = submit(state.request)
    if isinstance(receipt, AdmissionStop):
        return receipt.report()
    state = journal.finish(receipt, submitted=True)
    if state.receipt is None:
        raise ValueError("admission journal lost its acknowledgement")
    return state.receipt


def poll_admissions(
    run: Callable[[], dict],
    emit: Callable[[dict], None],
    interval: int,
    *,
    waiting_status: str,
    release_field: str,
    retry_statuses: set[str],
    clock: Callable[[], datetime],
    wait: Callable[[float], None],
    remember: Callable[[dict], None] | None = None,
) -> None:
    previous = None
    while True:
        report = run()
        if report != previous:
            emit(report)
            previous = report
        pending = []
        for row in report["occurrences"]:
            if row["status"] == waiting_status or row["status"] in retry_statuses:
                pending.append(row)
            elif remember is not None:
                remember(row)
        if not pending:
            return
        now = clock()
        future = (
            datetime.fromisoformat(row[release_field])
            for row in pending
            if row["status"] == waiting_status
        )
        until_release = min(
            ((due - now).total_seconds() for due in future if due > now),
            default=interval,
        )
        wait(min(interval, until_release))


def invoke_paper_cli(binary: Path, *arguments: str, timeout: int = 45) -> dict:
    output = subprocess.run(
        [str(binary), *arguments], check=True, capture_output=True, timeout=timeout
    ).stdout
    if len(output) > MAX_DOCUMENT_BYTES:
        raise ValueError("Go receipt exceeds the document limit")
    receipt = decode_pinned_json(output)
    if not isinstance(receipt, dict):
        raise ValueError("Go receipt must be a JSON object")
    return receipt


def parse_cycle_args(parser: argparse.ArgumentParser) -> argparse.Namespace:
    parser.add_argument("--root", type=Path)
    parser.add_argument("--plan-ref")
    parser.add_argument("--plan-sha256")
    parser.add_argument("--journal", type=Path)
    parser.add_argument("--paper-cli", type=Path)
    parser.add_argument(
        "--identity-registry-sha256",
        default=os.getenv("TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256"),
    )
    parser.add_argument(
        "--poll-interval-seconds",
        type=int,
        help="Wait for future occurrences and retry uncertain Go requests every 1 to 60 seconds",
    )
    args = parser.parse_args()
    if not args.schema:
        if not all((args.root, args.plan_ref, args.plan_sha256, args.journal, args.paper_cli)):
            parser.error("--root, --plan-ref, --plan-sha256, --journal, and --paper-cli are required")
        if args.poll_interval_seconds is not None and not 1 <= args.poll_interval_seconds <= 60:
            parser.error("--poll-interval-seconds must be between 1 and 60")
    return args


def execute_cycle_command(
    args: argparse.Namespace,
    invoke: GoRunner,
    run: Callable[..., dict],
    poll: Callable[..., None],
) -> None:
    root = args.root.resolve(strict=True)
    reference = ArchiveReference(ref=args.plan_ref, sha256=args.plan_sha256)
    options = {"trusted_identity_registry_sha256": args.identity_registry_sha256}

    def emit(report: dict) -> None:
        print(json.dumps(report), flush=True)

    if args.poll_interval_seconds is None:
        emit(run(root, reference, args.journal, invoke, **options))
    else:
        poll(root, reference, args.journal, invoke, args.poll_interval_seconds, emit, **options)
