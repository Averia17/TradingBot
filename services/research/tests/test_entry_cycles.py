import hashlib
import json
import os
import socket
import subprocess
import sys
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path
from uuid import uuid4

import pytest
from test_worker_postgres import build_go_command, process, request, wait_for_resource

from tradingbot.entry_cycles import run_cycles

ROOT = Path(__file__).resolve().parents[3]
ARCHIVE = ROOT / "services/research/examples/selection"
ACCOUNT = "11111111-1111-4111-8111-111111111111"
AT = datetime(2026, 11, 18, 10, 0, 1, tzinfo=UTC)


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
def test_command_schedules_two_occurrences_and_replays_them_on_postgres(tmp_path):
    _, plan, bundle = fixture(tmp_path)
    account_id = str(uuid4())
    plan["account_id"] = account_id
    control = build_go_command(tmp_path, "control")
    paper_apply = build_go_command(tmp_path, "paper-apply")
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
        template = json.loads((tmp_path / "screening.json").read_bytes())
        market = json.loads((tmp_path / "market.json").read_bytes())
        for record in market["records"]:
            delta = AT - datetime.fromisoformat(record["event_time"])
            for field in ("event_time", "available_at", "ingested_at"):
                record[field] = (first - delta).isoformat()
        template["data_manifest"] = put(tmp_path, "market.json", market)
        screen = put(tmp_path, "screening.json", template)
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
            "--paper-apply",
            paper_apply,
        ]

        def drive():
            completed = subprocess.run(
                command,
                env=env,
                timeout=45,
                capture_output=True,
                check=True,
            )
            return json.loads(completed.stdout)["occurrences"]

        first_run = drive()
        assert [row["status"] for row in first_run] == ["missed", "queued", "queued"]
        assert [row["created"] for row in first_run] == [False, True, True]
        ids = [row["job_id"] for row in first_run[1:]]
        assert len(set(ids)) == 2
        for job_id in ids:
            assert request(port, f"/v1/research/jobs/{job_id}")[0] == 200
        replay = drive()
        assert [row["created"] for row in replay] == [False, False, False]
        assert [row["job_id"] for row in replay[1:]] == ids
