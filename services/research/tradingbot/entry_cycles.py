"""Stage local paper-entry occurrences and submit them to the Go scheduler."""

import argparse
import hashlib
import json
import os
import subprocess
import tempfile
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Annotated, Literal
from uuid import NAMESPACE_URL, uuid5

from pydantic import Field, ValidationError, model_validator

from tradingbot.contracts import Contract, PortfolioSnapshot
from tradingbot.selection import plan_research
from tradingbot.snapshots import (
    SHA256,
    ArchiveReference,
    CaptureTime,
    Identifier,
    decode_pinned_json,
    read_pinned_json,
)

MAX_DOCUMENT_BYTES = 256 * 1024
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


class StagedEntry(Contract):
    schema_version: Literal["0.1"]
    owner_binding_sha256: SHA256
    admission_sha256: SHA256
    admission: str
    selection: str


class EntryReceipt(Contract):
    entry_schedule_schema_version: Literal["0.1"]
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
    return StagedEntry.model_validate(decode_pinned_json(raw))


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
    portfolio = PortfolioSnapshot.model_validate(bundle.get("portfolio"))
    if portfolio.as_of != datetime.fromisoformat(admission["scheduled_at"]):
        raise ValueError("Go returned a different portfolio cutoff")
    context = bundle.get("snapshot_context")
    if not isinstance(context, dict) or context.get("account_id") != account_id:
        raise ValueError("Go returned a different portfolio account")
    selected = plan_research(
        root,
        screening
        | {
            "job_id": identity,
            "policy_version": policy["policy_version"],
            "portfolio": portfolio.model_dump(mode="json"),
        },
    )
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
    return StagedEntry(
        schema_version="0.1",
        owner_binding_sha256=binding,
        admission_sha256=_sha(raw),
        admission=raw,
        selection=selection,
    )


def _submit(
    root: Path,
    invoke: GoRunner,
    staged: StagedEntry,
    binding: str,
    admission: dict,
    account_id: str,
    policy,
    marks,
    screening,
) -> dict:
    if staged.owner_binding_sha256 != binding or staged.admission_sha256 != _sha(staged.admission):
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
    if not isinstance(selected, dict) or "job" not in selected:
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
    with tempfile.TemporaryDirectory() as temporary:
        config = Path(temporary) / "config.json"
        selection = Path(temporary) / "selection.json"
        config.write_bytes(staged.admission.encode())
        selection.write_bytes(staged.selection.encode())
        receipt = invoke(
            "schedule-entry",
            "--config",
            str(config),
            "--selection",
            str(selection),
            "--archive-root",
            str(root),
        )
    EntryReceipt.model_validate(receipt)
    return receipt


def run_cycles(
    root: Path,
    plan_ref: ArchiveReference | dict,
    journal: Path,
    invoke: GoRunner,
    *,
    clock: Callable[[], datetime] = lambda: datetime.now(UTC),
) -> dict:
    reference = ArchiveReference.model_validate(plan_ref)
    plan = EntryCyclePlan.model_validate(read_pinned_json(root, reference))
    policy = read_pinned_json(root, plan.policy)
    if not isinstance(policy, dict) or not isinstance(policy.get("policy_version"), str):
        raise ValueError("owner policy requires a policy version")
    outcomes = []
    for occurrence in plan.occurrences:
        scheduled = occurrence.scheduled_at.isoformat()
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
        admission = {
            "entry_schedule_schema_version": "0.1",
            "schedule_id": plan.schedule_id,
            "scheduled_at": scheduled,
            "max_lateness_seconds": occurrence.max_lateness_seconds,
            "max_job_llm_calls": screening.get("max_llm_calls"),
        }
        binding = _sha(
            _encode(
                {
                    "account_id": plan.account_id,
                    "schedule_id": plan.schedule_id,
                    "policy": plan.policy.model_dump(mode="json"),
                    "occurrence": occurrence.model_dump(mode="json"),
                }
            )
        )
        try:
            staged = _read_journal(path)
        except FileNotFoundError:
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
            root, invoke, staged, binding, admission, plan.account_id, policy, marks, screening
        )
        outcomes.append(outcome | {"scheduled_at": scheduled})
    return {
        "entry_cycle_report_schema_version": "0.1",
        "owner_plan_sha256": reference.sha256,
        "occurrences": outcomes,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", action="store_true", help="Print the owner-plan schema")
    parser.add_argument("--root", type=Path)
    parser.add_argument("--plan-ref")
    parser.add_argument("--plan-sha256")
    parser.add_argument("--journal", type=Path)
    parser.add_argument("--paper-apply", type=Path)
    args = parser.parse_args()
    if args.schema:
        print(json.dumps(EntryCyclePlan.model_json_schema(mode="serialization"), indent=2))
        return 0
    if not all((args.root, args.plan_ref, args.plan_sha256, args.journal, args.paper_apply)):
        parser.error("--root, --plan-ref, --plan-sha256, --journal, and --paper-apply are required")

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

    try:
        executable = args.paper_apply.resolve(strict=True)
        result = run_cycles(
            args.root.resolve(strict=True),
            {"ref": args.plan_ref, "sha256": args.plan_sha256},
            args.journal,
            invoke,
        )
    except ValidationError:
        parser.exit(2, "entry cycles rejected: invalid plan, journal or snapshot\n")
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        parser.exit(2, f"entry cycles rejected ({type(exc).__name__})\n")
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
