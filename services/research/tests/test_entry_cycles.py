import hashlib
import json
import os
import socket
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import UTC, datetime, timedelta
from pathlib import Path
from threading import Barrier
from uuid import uuid4

import pytest
from test_worker_postgres import build_go_command, process, request, wait_for_resource

from tradingbot.entry_cycles import ENTRY_CYCLE_PLAN, StagedEntry, _persist, poll_cycles, run_cycles
from tradingbot.snapshots import ArchiveReference, read_pinned_json

ROOT = Path(__file__).resolve().parents[3]
ARCHIVE = ROOT / "services/research/examples/selection"
ACCOUNT = "11111111-1111-4111-8111-111111111111"
AT = datetime(2026, 11, 18, 10, 0, 1, tzinfo=UTC)


def test_concurrent_entry_publishers_keep_first_journal_and_remove_drafts(tmp_path):
    path = tmp_path / "journal" / "occurrence.json"
    start = Barrier(2)

    def publish(admission):
        staged = StagedEntry(
            schema_version="0.1",
            owner_binding_sha256="1" * 64,
            admission_sha256=hashlib.sha256(admission.encode()).hexdigest(),
            admission=admission,
            selection="{}",
        )
        start.wait(timeout=5)
        return _persist(path, staged)

    with ThreadPoolExecutor(max_workers=2) as executor:
        first, second = executor.map(publish, ('{"attempt":1}', '{"attempt":2}'))
    assert first == second == StagedEntry.model_validate_json(path.read_bytes())
    assert list(path.parent.iterdir()) == [path]


def put(root, name, value):
    raw = json.dumps(value).encode()
    (root / name).write_bytes(raw)
    return {"ref": name, "sha256": hashlib.sha256(raw).hexdigest()}


def fixture(root):
    expected = json.loads((ROOT / "contracts/valid-paper-entry-schedule.json").read_bytes())
    vetted = expected["snapshot_context"]
    selection = json.loads((ARCHIVE / "request.json").read_bytes())
    (root / "market.json").write_bytes((ARCHIVE / "market.json").read_bytes())
    for field in ("job_id", "policy_version", "portfolio"):
        selection.pop(field)
    plan = {
        "schema_version": "0.1",
        "account_id": ACCOUNT,
        "schedule_id": "opening-opportunity",
        "policy": put(root, "risk.json", vetted["policy"]),
        "occurrences": [
            {
                "scheduled_at": AT.isoformat(),
                "max_lateness_seconds": 30,
                "marks": put(root, "marks.json", vetted["marks"]),
                "screening": put(root, "screening.json", selection),
            },
            {
                "scheduled_at": (AT + timedelta(minutes=1)).isoformat(),
                "max_lateness_seconds": 30,
                "marks": put(root, "later-marks.json", vetted["marks"]),
                "screening": put(root, "later-screening.json", selection),
            },
        ],
    }
    portfolio = {
        "snapshot_id": "f72108f4-756a-8e8b-931e-891cb579a95c",
        "as_of": AT.isoformat(),
        "currency": "USD",
        "cash_asset": "USD",
        "cash_usd": "1000",
        "reserved_cash_usd": "0",
        "max_position_pct": "0.2500",
        "positions": [],
    }
    return put(root, "plan.json", plan), plan, {"snapshot_context": vetted, "portfolio": portfolio}


def identity_fixture(root):
    _, plan, bundle = fixture(root)
    screening = json.loads((ARCHIVE / "identity-request.json").read_bytes())
    for field in ("job_id", "policy_version", "portfolio", "identity_registry"):
        screening.pop(field)
    for field in ("remaining_analysis_slots", "top_k"):
        screening[field] = 1
    screening["candidates"] = [item for item in screening["candidates"] if item["symbol"] == "TSLA"]
    for name in ("identity-market.json", "identity-registry.json"):
        (root / name).write_bytes((ARCHIVE / name).read_bytes())
    tape = json.loads((ROOT / "contracts/valid-paper-identity-quote-tape.json").read_bytes())
    registry_sha = hashlib.sha256((root / "identity-registry.json").read_bytes()).hexdigest()
    plan.update(
        schema_version="0.2",
        identity_registry={"ref": "identity-registry.json", "sha256": registry_sha},
        quote_tape=put(root, "quote-tape.json", bind_tape(tape, ACCOUNT, registry_sha)),
    )
    for occurrence in plan["occurrences"]:
        occurrence["screening"] = put(root, "identity-screening.json", screening)
    bundle["portfolio"]["as_of"] = "2026-11-18T10:00:01Z"
    return put(root, "identity-plan.json", plan), plan, bundle


def bind_tape(tape, account_id, registry_sha):
    tape.update(account_id=account_id, identity_registry_sha256=registry_sha)
    for observation in tape["observations"]:
        observation["quote"].update(
            identity_registry_sha256=registry_sha, security_id="fixture-current"
        )
    return tape


def test_bound_owner_plan_submits_exact_stable_job_and_retries_without_new_snapshot(tmp_path):
    plan_ref, plan, bundle = identity_fixture(tmp_path)
    registry_sha = plan["identity_registry"]["sha256"]
    journal = tmp_path / "journal"
    submissions = []

    def invoke(*args):
        if args[0] == "snapshot":
            assert not submissions
            return bundle
        assert args[0] == "schedule-entry"
        assert args[args.index("--identity-registry-sha256") + 1] == registry_sha
        config = Path(args[args.index("--config") + 1]).read_bytes()
        raw = Path(args[args.index("--selection") + 1]).read_bytes()
        admission, selection = json.loads(config), json.loads(raw)
        assert admission["entry_schedule_schema_version"] == "0.2"
        assert admission["identity_binding"] == {
            "identity_registry": plan["identity_registry"],
            "quote_tape": plan["quote_tape"],
        }
        assert admission["selection_sha256"] == hashlib.sha256(raw).hexdigest()
        assert selection["selection_schema_version"] == "0.2"
        assert selection["job"]["job_schema_version"] == "0.2"
        assert selection["job"]["identity_registry"] == plan["identity_registry"]
        assert selection["job"]["portfolio"] == bundle["portfolio"]
        assert selection["job"]["security_bindings"]
        submissions.append((config, raw))
        return {
            "entry_schedule_schema_version": "0.2",
            "status": "queued",
            "created": len(submissions) == 1,
            "job_id": ACCOUNT,
            "created_at": AT.isoformat(),
        }

    for now in (AT, AT + timedelta(minutes=2)):
        report = run_cycles(
            tmp_path,
            plan_ref,
            journal,
            invoke,
            clock=lambda now=now: now,
            trusted_identity_registry_sha256=registry_sha,
        )
        assert report["occurrences"][0]["status"] == "queued"
        if now == AT:
            (tmp_path / plan["identity_registry"]["ref"]).unlink()
            (tmp_path / plan["quote_tape"]["ref"]).unlink()
    assert len(submissions) == 2 and submissions[0] == submissions[1]


@pytest.mark.parametrize(
    "mutation",
    ["owner-pin", "tape-sha", "tape-account", "tape-registry", "quote-registry", "screening", "screening-pin"],
)
def test_bound_owner_plan_rejects_untrusted_inputs_before_go(tmp_path, mutation):
    _, plan, _ = identity_fixture(tmp_path)
    registry_sha = plan["identity_registry"]["sha256"]
    if mutation == "owner-pin":
        registry_sha = "a" * 64
    if mutation == "tape-sha":
        (tmp_path / plan["quote_tape"]["ref"]).write_bytes(b"{}")
    if mutation in {"tape-account", "tape-registry", "quote-registry"}:
        tape = json.loads((tmp_path / plan["quote_tape"]["ref"]).read_bytes())
        if mutation == "tape-account":
            tape["account_id"] = str(uuid4())
        elif mutation == "tape-registry":
            tape["identity_registry_sha256"] = "b" * 64
        else:
            tape["observations"][0]["quote"]["identity_registry_sha256"] = "b" * 64
        plan["quote_tape"] = put(tmp_path, "quote-tape.json", tape)
    if mutation in {"screening", "screening-pin"}:
        occurrence = plan["occurrences"][0]
        screening = json.loads((tmp_path / occurrence["screening"]["ref"]).read_bytes())
        if mutation == "screening":
            screening["schema_version"] = "0.2"
        else:
            screening["identity_registry"] = plan["identity_registry"] | {"sha256": "b" * 64}
        occurrence["screening"] = put(tmp_path, "identity-screening.json", screening)
    ref = put(tmp_path, "identity-plan.json", plan)

    def unexpected(*_):
        pytest.fail("an untrusted owner plan must reject before invoking Go")

    with pytest.raises(ValueError):
        run_cycles(
            tmp_path,
            ref,
            tmp_path / "journal",
            unexpected,
            clock=lambda: AT,
            trusted_identity_registry_sha256=registry_sha,
        )


@pytest.mark.parametrize("field", ["portfolio", "security_bindings"])
def test_bound_staging_rejects_rehashed_snapshot_or_security_changes(tmp_path, field):
    ref, plan, bundle = identity_fixture(tmp_path)
    registry_sha = plan["identity_registry"]["sha256"]
    journal = tmp_path / "journal"

    def invoke(*args):
        if args[0] == "snapshot":
            return bundle
        return {"entry_schedule_schema_version": "0.2", "status": "not_selected", "created": False}

    run_cycles(
        tmp_path,
        ref,
        journal,
        invoke,
        clock=lambda: AT,
        trusted_identity_registry_sha256=registry_sha,
    )
    journal_file = next(journal.glob("*.json"))
    staged = json.loads(journal_file.read_bytes())
    selection = json.loads(staged["selection"])
    if field == "portfolio":
        selection["job"]["portfolio"]["cash_usd"] = "999"
    else:
        selection["job"]["security_bindings"][0]["security_id"] = "other-issuer"
    staged["selection"] = json.dumps(selection)
    admission = json.loads(staged["admission"])
    admission["selection_sha256"] = hashlib.sha256(staged["selection"].encode()).hexdigest()
    staged["admission"] = json.dumps(admission)
    staged["admission_sha256"] = hashlib.sha256(staged["admission"].encode()).hexdigest()
    journal_file.write_text(json.dumps(staged), encoding="utf-8")

    def unexpected(*_):
        pytest.fail("a modified stable binding or original snapshot must not reach Go")

    with pytest.raises(ValueError, match="stable identity or Go snapshot"):
        run_cycles(
            tmp_path,
            ref,
            journal,
            unexpected,
            clock=lambda: AT,
            trusted_identity_registry_sha256=registry_sha,
        )


def test_owner_plan_schema_and_examples_use_pinned_files():
    published = json.loads((ROOT / "contracts/paper-entry-cycle-plan.schema.json").read_bytes())
    assert published == ENTRY_CYCLE_PLAN.json_schema(mode="serialization")
    for filename in (
        "valid-paper-entry-cycle-plan.json",
        "valid-paper-identity-entry-cycle-plan.json",
    ):
        raw = json.loads((ROOT / "contracts" / filename).read_bytes())
        plan = ENTRY_CYCLE_PLAN.validate_python(raw)
        read_pinned_json(ROOT, plan.policy)
        for occurrence in plan.occurrences:
            read_pinned_json(ROOT, occurrence.marks)
            screening = read_pinned_json(ROOT, occurrence.screening)
            read_pinned_json(ROOT, ArchiveReference.model_validate(screening["data_manifest"]))
        if raw["schema_version"] == "0.2":
            read_pinned_json(ROOT, plan.identity_registry)
            tape = read_pinned_json(ROOT, plan.quote_tape)
            assert tape["account_id"] == plan.account_id
            assert tape["identity_registry_sha256"] == plan.identity_registry.sha256


def test_repeat_submits_exact_staged_bytes_without_new_snapshot(tmp_path):
    plan_ref, _, bundle = fixture(tmp_path)
    calls = []

    def invoke(*args):
        if args[0] == "snapshot":
            assert not calls
            return bundle
        assert args[0] == "schedule-entry"
        config = Path(args[args.index("--config") + 1]).read_bytes()
        selection = Path(args[args.index("--selection") + 1]).read_bytes()
        assert json.loads(config)["selection_sha256"] == hashlib.sha256(selection).hexdigest()
        calls.append((config, selection))
        return {
            "entry_schedule_schema_version": "0.1",
            "status": "queued",
            "created": len(calls) == 1,
            "job_id": ACCOUNT,
            "created_at": AT.isoformat(),
        }

    def drive(now):
        return run_cycles(tmp_path, plan_ref, tmp_path / "journal", invoke, clock=lambda: now)

    assert [row["status"] for row in drive(AT)["occurrences"]] == ["queued", "not_due"]
    replay = drive(AT + timedelta(minutes=2))["occurrences"]
    assert [row["status"] for row in replay] == ["queued", "missed"]
    assert replay[0]["created"] is False
    assert len(calls) == 2 and calls[0] == calls[1]


def test_plan_digest_mismatch_runs_no_go_tool(tmp_path):
    plan_ref, _, _ = fixture(tmp_path)
    plan_ref["sha256"] = "a" * 64

    def unexpected(*_):
        pytest.fail("an invalid plan cannot start the Go runner")

    with pytest.raises(ValueError, match="SHA-256"):
        run_cycles(tmp_path, plan_ref, tmp_path / "journal", unexpected, clock=lambda: AT)


def test_foreground_replays_an_uncertain_submission_and_keeps_other_occurrences_running(tmp_path):
    plan_ref, plan, bundle = fixture(tmp_path)
    first, second = [row["scheduled_at"] for row in plan["occurrences"]]
    now = AT - timedelta(seconds=1)
    ticks = iter(
        [(1, AT), (2, AT + timedelta(minutes=1)), (2, AT + timedelta(minutes=1, seconds=1))]
    )
    attempts = {first: [], second: []}
    reports = []

    def wait(seconds):
        nonlocal now
        expected, now = next(ticks)
        assert seconds == expected

    def invoke(*args):
        if args[0] == "snapshot":
            scheduled = args[args.index("--as-of") + 1]
            return bundle | {"portfolio": bundle["portfolio"] | {"as_of": scheduled}}
        config = Path(args[args.index("--config") + 1]).read_bytes()
        scheduled = json.loads(config)["scheduled_at"]
        attempts[scheduled].append(config)
        if scheduled == first and len(attempts[first]) <= 2:
            raise subprocess.TimeoutExpired("paper", 45)
        return {
            "entry_schedule_schema_version": "0.1",
            "status": "queued",
            "created": len(attempts[scheduled]) == 1,
            "job_id": ACCOUNT,
            "created_at": scheduled,
        }

    poll_cycles(
        tmp_path,
        plan_ref,
        tmp_path / "journal",
        invoke,
        2,
        reports.append,
        clock=lambda: now,
        wait=wait,
    )

    assert [[item["status"] for item in report["occurrences"]] for report in reports] == [
        ["not_due", "not_due"],
        ["pending_go", "not_due"],
        ["pending_go", "queued"],
        ["queued", "queued"],
    ]
    assert len(attempts[first]) == 3 and len(set(attempts[first])) == 1
    assert len(attempts[second]) == 1


def test_changed_journal_never_submits_again(tmp_path):
    plan_ref, _, bundle = fixture(tmp_path)
    journal = tmp_path / "journal"

    def invoke(*args):
        if args[0] == "snapshot":
            return bundle
        return {"entry_schedule_schema_version": "0.1", "status": "not_selected", "created": False}

    run_cycles(tmp_path, plan_ref, journal, invoke, clock=lambda: AT)
    path = next(journal.glob("*.json"))
    staged = json.loads(path.read_bytes())
    staged["admission"] += " "
    path.write_text(json.dumps(staged), encoding="utf-8")

    def unexpected(*_):
        pytest.fail("a changed admitted document cannot reach Go")

    with pytest.raises(ValueError, match="admission SHA-256"):
        run_cycles(tmp_path, plan_ref, journal, unexpected, clock=lambda: AT)


def test_rehashed_journal_cannot_change_the_pinned_market_archive(tmp_path):
    plan_ref, _, bundle = fixture(tmp_path)
    journal = tmp_path / "journal"

    def invoke(*args):
        if args[0] == "snapshot":
            return bundle
        return {"entry_schedule_schema_version": "0.1", "status": "not_selected", "created": False}

    run_cycles(tmp_path, plan_ref, journal, invoke, clock=lambda: AT)
    path = next(journal.glob("*.json"))
    staged = json.loads(path.read_bytes())
    selection = json.loads(staged["selection"])
    selection["job"]["data_manifest"]["sha256"] = "a" * 64
    staged["selection"] = json.dumps(selection)
    admission = json.loads(staged["admission"])
    admission["selection_sha256"] = hashlib.sha256(staged["selection"].encode()).hexdigest()
    staged["admission"] = json.dumps(admission)
    staged["admission_sha256"] = hashlib.sha256(staged["admission"].encode()).hexdigest()
    path.write_text(json.dumps(staged), encoding="utf-8")

    def unexpected(*_):
        pytest.fail("a changed archive cannot reach Go")

    with pytest.raises(ValueError, match="pinned screening inputs"):
        run_cycles(tmp_path, plan_ref, journal, unexpected, clock=lambda: AT)


@pytest.mark.skipif(
    not os.environ.get("TRADINGBOT_ENTRY_CYCLE_TEST_DATABASE_URL"),
    reason="set TRADINGBOT_ENTRY_CYCLE_TEST_DATABASE_URL to a disposable PostgreSQL database",
)
@pytest.mark.parametrize("stable", [False, True], ids=["legacy", "identity"])
def test_command_schedules_two_occurrences_and_replays_them_on_postgres(tmp_path, stable):
    _, plan, bundle = identity_fixture(tmp_path) if stable else fixture(tmp_path)
    account_id = str(uuid4())
    plan["account_id"] = account_id
    control = build_go_command(tmp_path, "control")
    paper_apply = build_go_command(tmp_path, "paper")
    env = os.environ | {
        "TRADINGBOT_DATABASE_URL": os.environ["TRADINGBOT_ENTRY_CYCLE_TEST_DATABASE_URL"]
    }
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    with process([control, "-listen", f"127.0.0.1:{port}"], env) as running:
        wait_for_resource(port, "/readyz", running)
        assert (
            request(
                port, "/v1/paper/accounts", {"account_id": account_id, "initial_cash_usd": "1000"}
            )[0]
            == 201
        )
        first = datetime.now(UTC) + timedelta(seconds=2)
        market_name = "identity-market.json" if stable else "market.json"
        screening_name = "identity-screening.json" if stable else "screening.json"
        template = json.loads((tmp_path / screening_name).read_bytes())
        market = json.loads((tmp_path / market_name).read_bytes())

        def shift(value):
            return (first + (datetime.fromisoformat(value) - AT)).isoformat()

        for record in market["records"]:
            event_time = record["event_time"]
            for field in ("event_time", "available_at", "ingested_at"):
                record[field] = shift(record[field] if stable else event_time)
            if record["kind"] == "listing":
                record["payload"]["effective_at"] = shift(record["payload"]["effective_at"])
        if stable:
            registry_name = "identity-registry.json"
            registry = json.loads((tmp_path / registry_name).read_bytes())
            for entry in registry["entries"]:
                for field in ("available_at", "effective_at"):
                    entry[field] = shift(entry[field])
            plan["identity_registry"] = put(tmp_path, registry_name, registry)
            env["TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256"] = plan["identity_registry"][
                "sha256"
            ]
            tape = json.loads((tmp_path / "quote-tape.json").read_bytes())
            plan["quote_tape"] = put(
                tmp_path,
                "quote-tape.json",
                bind_tape(tape, account_id, plan["identity_registry"]["sha256"]),
            )
        template["data_manifest"] = put(tmp_path, market_name, market)
        screen = put(tmp_path, screening_name, template)
        marks = bundle["snapshot_context"]["marks"]
        for mark in marks:
            mark["at"] = mark["available_at"] = (first - timedelta(seconds=5)).isoformat()
        observed = put(tmp_path, "marks.json", marks)
        for item, seconds in zip(plan["occurrences"], (0, 0.01), strict=True):
            item.update(
                scheduled_at=(first + timedelta(seconds=seconds)).isoformat(),
                marks=observed,
                screening=screen,
                max_lateness_seconds=45,
            )
        plan["occurrences"].insert(
            0, plan["occurrences"][0] | {"scheduled_at": (first - timedelta(minutes=2)).isoformat()}
        )
        plan_ref = put(tmp_path, "plan.json", plan)
        time.sleep(max(0, (first - datetime.now(UTC)).total_seconds()) + 0.05)
        command = [
            sys.executable,
            "-m",
            "tradingbot.entry_cycles",
            "--root",
            str(tmp_path),
            "--plan-ref",
            "plan.json",
            "--plan-sha256",
            plan_ref["sha256"],
            "--journal",
            str(tmp_path / "journal"),
            "--paper-cli",
            paper_apply,
        ]

        def drive():
            completed = subprocess.run(
                command,
                env=env,
                timeout=45,
                capture_output=True,
                check=False,
            )
            assert completed.returncode == 0, completed.stderr.decode(errors="replace")
            return json.loads(completed.stdout)["occurrences"]

        first_run = drive()
        assert [row["status"] for row in first_run] == ["missed", "queued", "queued"]
        assert [row["created"] for row in first_run] == [False, True, True]
        ids = [row["job_id"] for row in first_run[1:]]
        assert len(set(ids)) == 2
        for job_id in ids:
            status, persisted = request(port, f"/v1/research/jobs/{job_id}")
            assert status == 200
            if stable:
                assert persisted["request"]["job_schema_version"] == "0.2"
                assert persisted["request"]["identity_registry"] == plan["identity_registry"]
                assert persisted["request"]["security_bindings"] == [
                    {"symbol": "TSLA", "security_id": "fixture-current"},
                ]
        if stable:
            (tmp_path / plan["identity_registry"]["ref"]).unlink()
            (tmp_path / plan["quote_tape"]["ref"]).unlink()
        replay = drive()
        assert [row["created"] for row in replay] == [False, False, False]
        assert [row["job_id"] for row in replay[1:]] == ids
