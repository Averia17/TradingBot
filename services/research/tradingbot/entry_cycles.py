"""Stage local paper-entry occurrences and submit them to the Go scheduler."""

import argparse
import hashlib
import json
import os
import subprocess
import tempfile
import time
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Annotated, Literal
from uuid import NAMESPACE_URL, uuid5

from pydantic import Field, TypeAdapter, ValidationError, model_validator

from tradingbot.contracts import Contract, PortfolioSnapshot
from tradingbot.security_identity import load_identity_registry
from tradingbot.selection import plan_research
from tradingbot.snapshots import (
    SHA256,
    ArchiveReference,
    CaptureTime,
    Identifier,
    decode_pinned_json,
    read_pinned_bytes,
    read_pinned_json,
)

MAX_DOCUMENT_BYTES = 256 * 1024
MAX_QUOTE_TAPE_BYTES = 2 * 1024 * 1024
GoRunner = Callable[..., dict]


class Occurrence(Contract):
    scheduled_at: CaptureTime
    max_lateness_seconds: Annotated[int, Field(strict=True, ge=1, le=3600)]
    marks: ArchiveReference
    screening: ArchiveReference


class EntryCyclePlan(Contract):
    schema_version: Literal["0.1"]
    account_id: Annotated[str, Field(pattern=r"^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$")]
    schedule_id: Identifier
    policy: ArchiveReference
    occurrences: Annotated[tuple[Occurrence, ...], Field(min_length=1, max_length=32)]

    @model_validator(mode="after")
    def chronological(self):
        times = [item.scheduled_at for item in self.occurrences]
        if times != sorted(set(times)):
            raise ValueError("occurrences must have unique chronological cutoffs")
        return self


class IdentityEntryCyclePlan(EntryCyclePlan):
    schema_version: Literal["0.2"]
    identity_registry: ArchiveReference
    quote_tape: ArchiveReference


ENTRY_CYCLE_PLAN = TypeAdapter(
    Annotated[EntryCyclePlan | IdentityEntryCyclePlan, Field(discriminator="schema_version")]
)


class StagedEntry(Contract):
    schema_version: Literal["0.1"]
    owner_binding_sha256: SHA256
    admission_sha256: SHA256
    admission: str
    selection: str


class IdentityStagedEntry(StagedEntry):
    schema_version: Literal["0.2"]
    portfolio_sha256: SHA256


_STAGED_ENTRY = TypeAdapter(
    Annotated[StagedEntry | IdentityStagedEntry, Field(discriminator="schema_version")]
)


class EntryReceipt(Contract):
    entry_schedule_schema_version: Literal["0.1", "0.2"]
    status: Literal[
        "not_due", "missed", "not_selected", "queued", "leased", "completed", "failed", "expired"
    ]
    created: Annotated[bool, Field(strict=True)]
    job_id: (
        Annotated[str, Field(pattern=r"^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$")] | None
    ) = None
    created_at: CaptureTime | None = None

    @model_validator(mode="after")
    def coherent(self):
        transient = self.status in {"not_due", "missed", "not_selected"}
        if (
            (transient and (self.job_id is not None or self.created_at is not None))
            or (not transient and (self.job_id is None or self.created_at is None))
            or (self.created and self.status != "queued")
        ):
            raise ValueError("Go entry receipt has an inconsistent acceptance state")
        return self


def _encode(value) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def _sha(raw: str) -> str:
    return hashlib.sha256(raw.encode()).hexdigest()


def _read_journal(path: Path) -> StagedEntry:
    with path.open("rb") as stream:
        raw = stream.read(MAX_DOCUMENT_BYTES * 3 + 1)
    if len(raw) > MAX_DOCUMENT_BYTES * 3:
        raise ValueError("entry journal exceeds the document limit")
    return _STAGED_ENTRY.validate_python(decode_pinned_json(raw))


def _persist(directory: Path, path: Path, staged: StagedEntry) -> StagedEntry:
    directory.mkdir(parents=True, exist_ok=True)
    # Publish a fully written file without replacing an existing occurrence.
    # Concurrent runners use the winner's bytes before sending anything to Go.
    with tempfile.TemporaryDirectory(dir=directory) as temporary:
        draft = Path(temporary) / "stage.json"
        with draft.open("wb") as stream:
            stream.write(staged.model_dump_json().encode())
            stream.flush()
            os.fsync(stream.fileno())
        try:
            os.link(draft, path)
        except FileExistsError:
            return _read_journal(path)
    return staged


def _verify_identity_inputs(root: Path, plan: IdentityEntryCyclePlan) -> None:
    load_identity_registry(root, plan.identity_registry)
    raw = read_pinned_bytes(root, plan.quote_tape)
    if len(raw) > MAX_QUOTE_TAPE_BYTES:
        raise ValueError("owner quote tape exceeds Go's tape limit")
    tape = decode_pinned_json(raw)
    if (
        not isinstance(tape, dict)
        or tape.keys()
        != {
            "paper_quote_tape_schema_version",
            "source_mode",
            "account_id",
            "identity_registry_sha256",
            "observations",
        }
        or tape["paper_quote_tape_schema_version"] != "0.3"
        or tape["source_mode"] != "synthetic_fixture"
        or tape["account_id"] != plan.account_id
        or tape["identity_registry_sha256"] != plan.identity_registry.sha256
        or not isinstance(tape["observations"], list)
        or len(tape["observations"]) > 2048
    ):
        raise ValueError("owner quote tape must bind the same account and registry in protocol 0.3")


def _stage(
    root: Path,
    invoke: GoRunner,
    account_id: str,
    identity: str,
    policy,
    marks,
    screening,
    admission: dict,
    binding: str,
) -> StagedEntry:
    with tempfile.TemporaryDirectory() as temporary:
        policy_path = Path(temporary) / "policy.json"
        marks_path = Path(temporary) / "marks.json"
        policy_path.write_bytes(_encode(policy).encode())
        marks_path.write_bytes(_encode(marks).encode())
        bundle = invoke(
            "snapshot",
            "--account",
            account_id,
            "--policy",
            str(policy_path),
            "--marks",
            str(marks_path),
            "--as-of",
            admission["scheduled_at"],
        )
    wire_portfolio = bundle.get("portfolio")
    portfolio = PortfolioSnapshot.model_validate(wire_portfolio)
    if portfolio.as_of != datetime.fromisoformat(admission["scheduled_at"]):
        raise ValueError("Go returned a different portfolio cutoff")
    context = bundle.get("snapshot_context")
    if not isinstance(context, dict) or context.get("account_id") != account_id:
        raise ValueError("Go returned a different portfolio account")
    identity_binding = admission.get("identity_binding")
    request = screening | {
        "job_id": identity,
        "policy_version": policy["policy_version"],
        "portfolio": portfolio.model_dump(mode="json"),
    }
    if identity_binding is None:
        selected = plan_research(root, request)
    else:
        if (
            wire_portfolio.keys() != PortfolioSnapshot.model_fields.keys()
            or type(context.get("snapshot_version")) is not int
            or context["snapshot_version"] < 1
        ):
            raise ValueError("Go returned an incomplete portfolio snapshot")
        selected = plan_research(
            root,
            request,
            trusted_identity_registry=identity_binding["identity_registry"],
        )
        if selected["job"] is not None:
            # Preserve Go's original amount and timestamp encodings in the receipt.
            selected["job"]["portfolio"] = wire_portfolio
    selection = _encode(selected)
    raw = _encode(
        admission
        | {
            "snapshot_context": {
                "account_id": account_id,
                "snapshot_version": context.get("snapshot_version"),
                "policy": policy,
                "marks": marks,
            },
            "selection_sha256": _sha(selection),
        }
    )
    if max(len(raw.encode()), len(selection.encode())) > MAX_DOCUMENT_BYTES:
        raise ValueError("entry admission exceeds Go's document limit")
    fields = dict(
        owner_binding_sha256=binding,
        admission_sha256=_sha(raw),
        admission=raw,
        selection=selection,
    )
    if identity_binding is not None:
        return IdentityStagedEntry(
            schema_version="0.2", portfolio_sha256=_sha(_encode(wire_portfolio)), **fields
        )
    return StagedEntry(schema_version="0.1", **fields)


def _submit(
    root: Path,
    invoke: GoRunner,
    staged: StagedEntry,
    binding: str,
    admission: dict,
    identity: str,
    account_id: str,
    policy,
    marks,
    screening,
) -> dict:
    version = admission["entry_schedule_schema_version"]
    if (
        staged.schema_version != version
        or staged.owner_binding_sha256 != binding
        or staged.admission_sha256 != _sha(staged.admission)
    ):
        raise ValueError("entry journal owner binding or admission SHA-256 differs")
    document = decode_pinned_json(staged.admission.encode())
    context = document.get("snapshot_context") if isinstance(document, dict) else None
    if (
        not isinstance(context, dict)
        or context.get("account_id") != account_id
        or context.get("policy") != policy
        or context.get("marks") != marks
        or any(document.get(key) != value for key, value in admission.items())
        or document.get("selection_sha256") != _sha(staged.selection)
    ):
        raise ValueError("entry journal does not match its pinned inputs")
    selected = decode_pinned_json(staged.selection.encode())
    if (
        not isinstance(selected, dict)
        or selected.get("selection_schema_version") != version
        or "job" not in selected
    ):
        raise ValueError("entry journal has no screening receipt")
    job = selected["job"]
    if job is not None:
        eligible = {item["symbol"] for item in screening["candidates"] if item["allowed"] is True}
        if (
            not isinstance(job, dict)
            or job.get("data_manifest") != screening["data_manifest"]
            or job.get("policy_version") != policy["policy_version"]
            or not isinstance(job.get("requested_symbols"), list)
            or not set(job["requested_symbols"]) <= eligible
        ):
            raise ValueError("entry journal job differs from pinned screening inputs")
        if isinstance(staged, IdentityStagedEntry):
            candidate_ids = {
                item["symbol"]: item["security_id"]
                for item in screening["candidates"]
                if item["allowed"] is True
            }
            expected_bindings = [
                {"symbol": symbol, "security_id": candidate_ids[symbol]}
                for symbol in job["requested_symbols"]
            ]
            if (
                job.get("job_schema_version") != "0.2"
                or job.get("job_id") != identity
                or job.get("identity_registry")
                != admission["identity_binding"]["identity_registry"]
                or not expected_bindings
                or len({item["security_id"] for item in expected_bindings})
                != len(expected_bindings)
                or job.get("security_bindings") != expected_bindings
                or _sha(_encode(job.get("portfolio"))) != staged.portfolio_sha256
            ):
                raise ValueError(
                    "entry journal job differs from its stable identity or Go snapshot"
                )
    with tempfile.TemporaryDirectory() as temporary:
        config = Path(temporary) / "config.json"
        selection = Path(temporary) / "selection.json"
        config.write_bytes(staged.admission.encode())
        selection.write_bytes(staged.selection.encode())
        command = [
            "schedule-entry",
            "--config",
            str(config),
            "--selection",
            str(selection),
            "--archive-root",
            str(root),
        ]
        if version == "0.2":
            command.extend(
                [
                    "--identity-registry-sha256",
                    admission["identity_binding"]["identity_registry"]["sha256"],
                ]
            )
        receipt = invoke(*command)
    if EntryReceipt.model_validate(receipt).entry_schedule_schema_version != version:
        raise ValueError("Go entry receipt uses a different protocol version")
    return receipt


def run_cycles(
    root: Path,
    plan_ref: ArchiveReference | dict,
    journal: Path,
    invoke: GoRunner,
    *,
    clock: Callable[[], datetime] = lambda: datetime.now(UTC),
    settled: dict[str, dict] | None = None,
    trusted_identity_registry_sha256: str | None = None,
) -> dict:
    reference = ArchiveReference.model_validate(plan_ref)
    plan = ENTRY_CYCLE_PLAN.validate_python(read_pinned_json(root, reference))
    identity_options = {}
    if isinstance(plan, IdentityEntryCyclePlan):
        if trusted_identity_registry_sha256 != plan.identity_registry.sha256:
            raise ValueError("bound entry cycle requires the matching owner registry SHA-256")
        identity_options["identity_binding"] = {
            "identity_registry": plan.identity_registry.model_dump(mode="json"),
            "quote_tape": plan.quote_tape.model_dump(mode="json"),
        }
    policy = read_pinned_json(root, plan.policy)
    if not isinstance(policy, dict) or not isinstance(policy.get("policy_version"), str):
        raise ValueError("owner policy requires a policy version")
    outcomes = []
    identity_inputs_checked = False
    for occurrence in plan.occurrences:
        scheduled = occurrence.scheduled_at.isoformat()
        if settled is not None and scheduled in settled:
            outcomes.append(settled[scheduled])
            continue
        identity = str(
            uuid5(NAMESPACE_URL, _encode([plan.account_id, plan.schedule_id, scheduled]))
        )
        path = journal / f"{identity}.json"
        now = clock()
        if not path.exists() and (
            now < occurrence.scheduled_at
            or now > occurrence.scheduled_at + timedelta(seconds=occurrence.max_lateness_seconds)
        ):
            outcomes.append(
                {
                    "scheduled_at": scheduled,
                    "status": "not_due" if now < occurrence.scheduled_at else "missed",
                    "created": False,
                }
            )
            continue
        screening = read_pinned_json(root, occurrence.screening)
        marks = read_pinned_json(root, occurrence.marks)
        if (
            not isinstance(screening, dict)
            or {"job_id", "policy_version", "portfolio"} & screening.keys()
        ):
            raise ValueError("screening must omit runner-owned job and portfolio fields")
        if isinstance(plan, IdentityEntryCyclePlan):
            identity_registry = identity_options["identity_binding"]["identity_registry"]
            if (
                screening.get("schema_version") != "0.3"
                or screening.get("identity_registry", identity_registry) != identity_registry
            ):
                raise ValueError(
                    "bound entry requires screening version 0.3 and the exact owner registry pin"
                )
            screening = screening | {"identity_registry": identity_registry}
        elif screening.get("schema_version") == "0.3":
            raise ValueError("identity screening requires owner entry plan version 0.2")
        admission = {
            "entry_schedule_schema_version": plan.schema_version,
            "schedule_id": plan.schedule_id,
            "scheduled_at": scheduled,
            "max_lateness_seconds": occurrence.max_lateness_seconds,
            "max_job_llm_calls": screening.get("max_llm_calls"),
            **identity_options,
        }
        binding = _sha(
            _encode(
                {
                    "account_id": plan.account_id,
                    "schedule_id": plan.schedule_id,
                    "policy": plan.policy.model_dump(mode="json"),
                    "occurrence": occurrence.model_dump(mode="json"),
                    **identity_options,
                }
            )
        )
        try:
            try:
                staged = _read_journal(path)
            except FileNotFoundError:
                if isinstance(plan, IdentityEntryCyclePlan) and not identity_inputs_checked:
                    _verify_identity_inputs(root, plan)
                    identity_inputs_checked = True
                staged = _persist(
                    journal,
                    path,
                    _stage(
                        root,
                        invoke,
                        plan.account_id,
                        identity,
                        policy,
                        marks,
                        screening,
                        admission,
                        binding,
                    ),
                )
            outcome = _submit(
                root,
                invoke,
                staged,
                binding,
                admission,
                identity,
                plan.account_id,
                policy,
                marks,
                screening,
            )
        except subprocess.SubprocessError as exc:
            if settled is None:
                raise
            outcome = {"status": "pending_go", "created": False, "error": type(exc).__name__}
        outcomes.append(outcome | {"scheduled_at": scheduled})
    return {
        "entry_cycle_report_schema_version": plan.schema_version,
        "owner_plan_sha256": reference.sha256,
        "occurrences": outcomes,
    }


def poll_cycles(
    root: Path,
    plan_ref: ArchiveReference | dict,
    journal: Path,
    invoke: GoRunner,
    interval_seconds: int,
    emit: Callable[[dict], None],
    *,
    clock: Callable[[], datetime] = lambda: datetime.now(UTC),
    wait: Callable[[float], None] = time.sleep,
    trusted_identity_registry_sha256: str | None = None,
) -> None:
    settled: dict[str, dict] = {}
    previous = None
    while True:
        report = run_cycles(
            root,
            plan_ref,
            journal,
            invoke,
            clock=clock,
            settled=settled,
            trusted_identity_registry_sha256=trusted_identity_registry_sha256,
        )
        if report != previous:
            emit(report)
            previous = report
        settled.update(
            (row["scheduled_at"], row)
            for row in report["occurrences"]
            if row["status"] not in {"not_due", "pending_go"}
        )
        if len(settled) == len(report["occurrences"]):
            return
        due_times = (
            datetime.fromisoformat(row["scheduled_at"])
            for row in report["occurrences"]
            if row["status"] == "not_due"
        )
        now = clock()
        until_due = min(
            ((due - now).total_seconds() for due in due_times if due > now),
            default=interval_seconds,
        )
        wait(min(interval_seconds, until_due))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", action="store_true", help="Print the owner-plan schema")
    parser.add_argument("--root", type=Path)
    parser.add_argument("--plan-ref")
    parser.add_argument("--plan-sha256")
    parser.add_argument("--journal", type=Path)
    parser.add_argument("--paper-apply", type=Path)
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
    if args.schema:
        print(json.dumps(ENTRY_CYCLE_PLAN.json_schema(mode="serialization"), indent=2))
        return 0
    if not all((args.root, args.plan_ref, args.plan_sha256, args.journal, args.paper_apply)):
        parser.error("--root, --plan-ref, --plan-sha256, --journal, and --paper-apply are required")
    if args.poll_interval_seconds is not None and not 1 <= args.poll_interval_seconds <= 60:
        parser.error("--poll-interval-seconds must be between 1 and 60")

    def invoke(*arguments):
        response = subprocess.run(
            [str(executable), *arguments],
            check=True,
            capture_output=True,
            timeout=45,
        )
        if len(response.stdout) > MAX_DOCUMENT_BYTES:
            raise ValueError("Go scheduler receipt exceeds the document limit")
        result = decode_pinned_json(response.stdout)
        if not isinstance(result, dict):
            raise ValueError("Go output must be a receipt object")
        return result

    def emit(report):
        print(json.dumps(report), flush=True)

    try:
        executable = args.paper_apply.resolve(strict=True)
        root = args.root.resolve(strict=True)
        reference = {"ref": args.plan_ref, "sha256": args.plan_sha256}
        if args.poll_interval_seconds is None:
            emit(
                run_cycles(
                    root,
                    reference,
                    args.journal,
                    invoke,
                    trusted_identity_registry_sha256=args.identity_registry_sha256,
                )
            )
        else:
            poll_cycles(
                root,
                reference,
                args.journal,
                invoke,
                args.poll_interval_seconds,
                emit,
                trusted_identity_registry_sha256=args.identity_registry_sha256,
            )
    except ValidationError:
        parser.exit(2, "entry cycles rejected: invalid plan, journal or snapshot\n")
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        parser.exit(2, f"entry cycles rejected ({type(exc).__name__})\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
