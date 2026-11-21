"""Stage local paper-entry occurrences and submit them to the Go scheduler."""

import argparse
import hashlib
import json
import os
import subprocess
import tempfile
import time
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from functools import cached_property, partial
from pathlib import Path
from typing import Annotated, Literal
from uuid import NAMESPACE_URL, uuid5

from pydantic import Field, JsonValue, TypeAdapter, ValidationError, model_validator

from tradingbot.contracts import Contract, PortfolioSnapshot
from tradingbot.paper_cli import (
    MAX_DOCUMENT_BYTES,
    AdmissionReceipt,
    AdmissionState,
    AdmissionStop,
    AdmissionWindow,
    GoRunner,
    admit_occurrence,
    check_owner_quote_tape,
    execute_cycle_command,
    invoke_paper_cli,
    parse_cycle_args,
    poll_admissions,
    read_paper_snapshot,
)
from tradingbot.security_identity import load_identity_registry
from tradingbot.selection import plan_research
from tradingbot.snapshots import (
    SHA256,
    ArchiveReference,
    CaptureTime,
    Identifier,
    decode_pinned_json,
    read_pinned_json,
)


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


class EntryReceipt(AdmissionReceipt):
    unadmitted = frozenset({"not_due", "missed", "not_selected"})
    entry_schedule_schema_version: Literal["0.1", "0.2"]
    status: Literal[
        "not_due", "missed", "not_selected", "queued", "leased", "completed", "failed", "expired"
    ]


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


def _persist(path: Path, staged: StagedEntry) -> StagedEntry:
    directory = path.parent
    directory.mkdir(parents=True, exist_ok=True)
    # Publish a fully written file without replacing an existing occurrence.
    # Concurrent runners use the winner's bytes before sending anything to Go.
    with tempfile.TemporaryDirectory(dir=directory) as temporary:
        draft = Path(temporary) / "stage.json"
        with draft.open("wb") as stream:
            stream.write(staged.model_dump_json().encode())
            stream.flush()
            os.fsync(stream.fileno())
        # Windows rename refuses an existing destination. POSIX rename would replace it.
        publish = os.rename if os.name == "nt" else os.link
        try:
            publish(draft, path)
        except FileExistsError:
            return _read_journal(path)
    return staged


@dataclass(frozen=True)
class _Journal:
    path: Path

    def load(self) -> AdmissionState[StagedEntry]:
        try:
            return AdmissionState(request=_read_journal(self.path))
        except FileNotFoundError:
            return AdmissionState()

    def stage(self, request: StagedEntry) -> AdmissionState[StagedEntry]:
        return AdmissionState(request=_persist(self.path, request))

    def finish(self, receipt: dict, *, submitted: bool) -> AdmissionState[StagedEntry]:
        # Entry reports query Go for the current status on each one-shot run.
        return AdmissionState(receipt=receipt)


@dataclass(frozen=True)
class _EntryAdmission:
    root: Path
    plan: EntryCyclePlan
    occurrence: Occurrence
    policy: dict
    invoke: GoRunner

    @property
    def scheduled_at(self) -> str:
        return self.occurrence.scheduled_at.isoformat()

    @cached_property
    def identity(self) -> str:
        name = _encode([self.plan.account_id, self.plan.schedule_id, self.scheduled_at])
        return str(uuid5(NAMESPACE_URL, name))

    @cached_property
    def identity_binding(self) -> dict | None:
        if isinstance(self.plan, IdentityEntryCyclePlan):
            return {
                "identity_registry": self.plan.identity_registry.model_dump(mode="json"),
                "quote_tape": self.plan.quote_tape.model_dump(mode="json"),
            }
        return None

    @cached_property
    def materials(self) -> tuple[dict, JsonValue]:
        screening = read_pinned_json(self.root, self.occurrence.screening)
        marks = read_pinned_json(self.root, self.occurrence.marks)
        if (
            not isinstance(screening, dict)
            or {"job_id", "policy_version", "portfolio"} & screening.keys()
        ):
            raise ValueError("screening must omit runner-owned job and portfolio fields")
        if self.identity_binding is not None:
            registry = self.identity_binding["identity_registry"]
            if screening.get("schema_version") != "0.3" or (
                screening.get("identity_registry", registry) != registry
            ):
                raise ValueError(
                    "bound entry requires screening version 0.3 and the exact owner registry pin"
                )
            screening = screening | {"identity_registry": registry}
        elif screening.get("schema_version") == "0.3":
            raise ValueError("identity screening requires owner entry plan version 0.2")
        return screening, marks

    @cached_property
    def admission(self) -> dict:
        screening, _ = self.materials
        body = {
            "entry_schedule_schema_version": self.plan.schema_version,
            "schedule_id": self.plan.schedule_id,
            "scheduled_at": self.scheduled_at,
            "max_lateness_seconds": self.occurrence.max_lateness_seconds,
            "max_job_llm_calls": screening.get("max_llm_calls"),
        }
        if self.identity_binding is not None:
            body["identity_binding"] = self.identity_binding
        return body

    @cached_property
    def owner_binding_sha256(self) -> str:
        scope = {
            "account_id": self.plan.account_id,
            "schedule_id": self.plan.schedule_id,
            "policy": self.plan.policy.model_dump(mode="json"),
            "occurrence": self.occurrence.model_dump(mode="json"),
        }
        if self.identity_binding is not None:
            scope["identity_binding"] = self.identity_binding
        return _sha(_encode(scope))

    def prepare(self) -> StagedEntry:
        screening, marks = self.materials
        bundle = read_paper_snapshot(
            self.invoke,
            account=self.plan.account_id,
            as_of=self.scheduled_at,
            policy=_encode(self.policy).encode(),
            marks=_encode(marks).encode(),
        )
        wire_portfolio = bundle.get("portfolio")
        portfolio = PortfolioSnapshot.model_validate(wire_portfolio)
        if portfolio.as_of != self.occurrence.scheduled_at:
            raise ValueError("Go returned a different portfolio cutoff")
        context = bundle.get("snapshot_context")
        if not isinstance(context, dict) or context.get("account_id") != self.plan.account_id:
            raise ValueError("Go returned a different portfolio account")
        request = screening | {
            "job_id": self.identity,
            "policy_version": self.policy["policy_version"],
            "portfolio": portfolio.model_dump(mode="json"),
        }
        if self.identity_binding is None:
            selected = plan_research(self.root, request)
        else:
            if (
                wire_portfolio.keys() != PortfolioSnapshot.model_fields.keys()
                or type(context.get("snapshot_version")) is not int
                or context["snapshot_version"] < 1
            ):
                raise ValueError("Go returned an incomplete portfolio snapshot")
            selected = plan_research(
                self.root,
                request,
                trusted_identity_registry=self.identity_binding["identity_registry"],
            )
            if selected["job"] is not None:
                # Preserve Go's original amount and timestamp encodings in the receipt.
                selected["job"]["portfolio"] = wire_portfolio
        selection = _encode(selected)
        raw = _encode(
            self.admission
            | {
                "snapshot_context": {
                    "account_id": self.plan.account_id,
                    "snapshot_version": context.get("snapshot_version"),
                    "policy": self.policy,
                    "marks": marks,
                },
                "selection_sha256": _sha(selection),
            }
        )
        if max(len(raw.encode()), len(selection.encode())) > MAX_DOCUMENT_BYTES:
            raise ValueError("entry admission exceeds Go's document limit")
        fields = dict(
            owner_binding_sha256=self.owner_binding_sha256,
            admission_sha256=_sha(raw),
            admission=raw,
            selection=selection,
        )
        if self.identity_binding is not None:
            return IdentityStagedEntry(
                schema_version="0.2", portfolio_sha256=_sha(_encode(wire_portfolio)), **fields
            )
        return StagedEntry(schema_version="0.1", **fields)

    def submit(self, staged: StagedEntry) -> dict:
        screening, marks = self.materials
        version = self.plan.schema_version
        if (
            staged.schema_version != version
            or staged.owner_binding_sha256 != self.owner_binding_sha256
            or staged.admission_sha256 != _sha(staged.admission)
        ):
            raise ValueError("entry journal owner binding or admission SHA-256 differs")
        document = decode_pinned_json(staged.admission.encode())
        context = document.get("snapshot_context") if isinstance(document, dict) else None
        if (
            not isinstance(context, dict)
            or context.get("account_id") != self.plan.account_id
            or context.get("policy") != self.policy
            or context.get("marks") != marks
            or any(document.get(key) != value for key, value in self.admission.items())
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
            eligible = {
                item["symbol"]: item for item in screening["candidates"] if item["allowed"] is True
            }
            if (
                not isinstance(job, dict)
                or job.get("data_manifest") != screening["data_manifest"]
                or job.get("policy_version") != self.policy["policy_version"]
                or not isinstance(job.get("requested_symbols"), list)
                or any(
                    not isinstance(symbol, str) or symbol not in eligible
                    for symbol in job["requested_symbols"]
                )
            ):
                raise ValueError("entry journal job differs from pinned screening inputs")
            if isinstance(staged, IdentityStagedEntry):
                candidate_ids = {symbol: item["security_id"] for symbol, item in eligible.items()}
                expected_bindings = [
                    {"symbol": symbol, "security_id": candidate_ids[symbol]}
                    for symbol in job["requested_symbols"]
                ]
                if (
                    job.get("job_schema_version") != "0.2"
                    or job.get("job_id") != self.identity
                    or job.get("identity_registry") != self.identity_binding["identity_registry"]
                    or not expected_bindings
                    or len({item["security_id"] for item in expected_bindings})
                    != len(expected_bindings)
                    or job.get("security_bindings") != expected_bindings
                    or _sha(_encode(job.get("portfolio"))) != staged.portfolio_sha256
                ):
                    raise ValueError("entry journal job differs from its stable identity or Go snapshot")
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
                str(self.root),
            ]
            if self.identity_binding is not None:
                command.extend(
                    [
                        "--identity-registry-sha256",
                        self.identity_binding["identity_registry"]["sha256"],
                    ]
                )
            receipt = self.invoke(*command)
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
    if isinstance(plan, IdentityEntryCyclePlan) and (
        trusted_identity_registry_sha256 != plan.identity_registry.sha256
    ):
        raise ValueError("bound entry cycle requires the matching owner registry SHA-256")
    policy = read_pinned_json(root, plan.policy)
    if not isinstance(policy, dict) or not isinstance(policy.get("policy_version"), str):
        raise ValueError("owner policy requires a policy version")

    identity_inputs_checked = False

    def prepare(entry: _EntryAdmission) -> StagedEntry:
        nonlocal identity_inputs_checked
        if isinstance(plan, IdentityEntryCyclePlan) and not identity_inputs_checked:
            load_identity_registry(root, plan.identity_registry)
            check_owner_quote_tape(root, plan.quote_tape, plan.account_id, plan.identity_registry)
            identity_inputs_checked = True
        return entry.prepare()

    outcomes = []
    for occurrence in plan.occurrences:
        entry = _EntryAdmission(root, plan, occurrence, policy, invoke)
        if settled is not None and entry.scheduled_at in settled:
            outcomes.append(settled[entry.scheduled_at])
            continue
        window = AdmissionWindow(
            occurrence.scheduled_at,
            occurrence.scheduled_at + timedelta(seconds=occurrence.max_lateness_seconds),
            "not_due",
        )
        try:
            outcome = admit_occurrence(
                window,
                _Journal(journal / f"{entry.identity}.json"),
                partial(prepare, entry),
                entry.submit,
                clock,
            )
        except subprocess.SubprocessError as exc:
            if settled is None:
                raise
            outcome = AdmissionStop("pending_go", exc).report()
        outcomes.append(outcome | {"scheduled_at": entry.scheduled_at})
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

    def remember(row: dict) -> None:
        settled[row["scheduled_at"]] = row

    poll_admissions(
        lambda: run_cycles(
            root,
            plan_ref,
            journal,
            invoke,
            clock=clock,
            settled=settled,
            trusted_identity_registry_sha256=trusted_identity_registry_sha256,
        ),
        emit,
        interval_seconds,
        waiting_status="not_due",
        release_field="scheduled_at",
        retry_statuses={"pending_go"},
        clock=clock,
        wait=wait,
        remember=remember,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", action="store_true", help="Print the owner-plan schema")
    args = parse_cycle_args(parser)
    if args.schema:
        print(json.dumps(ENTRY_CYCLE_PLAN.json_schema(mode="serialization"), indent=2))
        return 0

    try:
        invoke = partial(invoke_paper_cli, args.paper_cli.resolve(strict=True))
        execute_cycle_command(args, invoke, run_cycles, poll_cycles)
    except ValidationError:
        parser.exit(2, "entry cycles rejected: invalid plan, journal or snapshot\n")
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        parser.exit(2, f"entry cycles rejected ({type(exc).__name__})\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
