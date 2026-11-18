"""Drill a logical PostgreSQL restore using disposable local research and paper data."""

import hashlib
import json
import os
import subprocess
import tempfile
import time
from contextlib import contextmanager
from datetime import UTC, datetime, timedelta
from pathlib import Path
from urllib.error import URLError
from uuid import uuid4

from smoke import (
    JOBS,
    ROOT,
    check_paper_order,
    checked,
    docker,
    enqueue,
    free_port,
    request,
)

POSTGRES_IMAGE = (
    "postgres:17-alpine@sha256:d3244e8ce3f165ee3c93d9c4a4b1d0fe48b3a89e2287b717fdcbbde92d405065"
)
DATABASE = "tradingbot"
DUMP_PATH = "/tmp/tradingbot-restore-drill.dump"


@contextmanager
def postgres(name):
    docker(
        "run",
        "--detach",
        "--rm",
        "--pull=never",
        "--name",
        name,
        "--publish",
        "127.0.0.1::5432",
        "--env",
        f"POSTGRES_DB={DATABASE}",
        "--env",
        "POSTGRES_HOST_AUTH_METHOD=trust",
        POSTGRES_IMAGE,
        capture=True,
        timeout=30,
    )
    try:
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            ready = subprocess.run(
                [
                    "docker",
                    "exec",
                    name,
                    "pg_isready",
                    "-h",
                    "127.0.0.1",
                    "-U",
                    "postgres",
                    "-d",
                    DATABASE,
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=5,
            )
            if ready.returncode == 0:
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("disposable PostgreSQL did not become ready")
        address = docker("port", name, "5432/tcp", capture=True, timeout=5).strip()
        checked(address.startswith("127.0.0.1:"), "PostgreSQL requires a loopback mapping")
        yield f"postgresql://postgres@{address}/{DATABASE}?sslmode=disable"
    finally:
        docker("stop", "--time", "5", name, capture=True, timeout=15)


@contextmanager
def control(binary, dsn):
    address = f"127.0.0.1:{free_port()}"
    origin = "http://" + address
    env = os.environ | {"TRADINGBOT_DATABASE_URL": dsn}
    child = subprocess.Popen([str(binary), "--listen", address], env=env)
    try:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            checked(child.poll() is None, "Go control exited before readiness")
            try:
                status = request(origin, "/readyz", 200)
            except (TimeoutError, URLError):
                time.sleep(0.1)
                continue
            checked(status["storage_enabled"], "Go control did not enable storage")
            break
        else:
            raise RuntimeError("Go control did not become ready")
        yield origin
    finally:
        child.terminate()
        try:
            child.wait(timeout=10)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)


def claim_request():
    return {"worker_id": "restore-drill", "claim_id": str(uuid4())}


def complete_fixture(origin, job, lease):
    fixture = json.loads((ROOT / "contracts/valid-result-request.json").read_text(encoding="utf-8"))
    fixture.update(worker_id=lease["worker_id"], lease_token=lease["lease_token"])
    batch = fixture["batch"]
    batch.update(
        run_id=str(uuid4()),
        portfolio_snapshot_id=job["portfolio"]["snapshot_id"],
        as_of=job["portfolio"]["as_of"],
        created_at=datetime.now(UTC).isoformat(),
        expires_at=lease["attempt_deadline"],
    )
    batch["intents"] = [intent for intent in batch["intents"] if intent["symbol"] == "TSLA"]
    batch["intents"][0]["decision_id"] = str(uuid4())
    batch["evidence_hashes"] = {
        key: value for key, value in batch["evidence_hashes"].items() if key.startswith("TSLA:")
    }
    fixture["reports"] = {"TSLA": fixture["reports"]["TSLA"]}
    path = f"{JOBS}/{job['job_id']}/complete"
    receipt = request(origin, path, 201, fixture)
    checked(receipt["result"]["metadata"]["llm_calls"] == 0, "fixture recorded model calls")
    return path, fixture, receipt


def paper_pending(origin, paper_path, primary_fill):
    account = request(origin, paper_path, 200)
    original = request(origin, paper_path + "/orders/" + primary_fill["order_id"], 200)
    fields = (
        "account_id",
        "instrument",
        "side",
        "quantity_raw",
        "gross_usd",
        "max_fee_usd",
    )
    reserve = {key: original[key] for key in fields}
    reserve.update(
        order_id=str(uuid4()),
        decision_id=str(uuid4()),
        quote_id=str(uuid4()),
        snapshot_version=account["version"],
        expires_at=(datetime.now(UTC) + timedelta(minutes=5)).isoformat(),
    )
    order = request(origin, paper_path + "/orders", 201, reserve)
    path = paper_path + "/orders/" + order["order_id"]
    outcome = {
        "account_id": order["account_id"],
        "order_id": order["order_id"],
        "event_id": str(uuid4()),
    }
    unknown = request(origin, path + "/resolve", 200, outcome | {"outcome": "unknown"})
    fill = outcome | {
        "event_id": str(uuid4()),
        "outcome": "filled",
        "gross_usd": "20.42",
        "fee_usd": "0.08",
        "observed_at": order["created_at"],
    }
    return path, unknown, fill


def database_counts(container):
    query = """SELECT json_build_object(
        'accounts', (SELECT count(*) FROM paper_accounts),
        'orders', (SELECT count(*) FROM paper_orders),
        'fills', (SELECT count(*) FROM paper_fills),
        'events', (SELECT count(*) FROM paper_events),
        'paper_outbox', (SELECT count(*) FROM paper_fill_outbox),
        'paper_outbox_pending', (SELECT count(*) FROM paper_fill_outbox WHERE queued_at IS NULL),
        'jobs', (SELECT count(*) FROM research_jobs),
        'attempts', (SELECT count(*) FROM research_job_attempts),
        'permits', (SELECT count(*) FROM research_call_permits),
        'results', (SELECT count(*) FROM research_job_results),
        'completion_events', (SELECT count(*) FROM research_outbox WHERE event_type = 'research_job.completed')
    )"""
    return json.loads(
        docker(
            "exec",
            container,
            "psql",
            "-XAt",
            "--set",
            "ON_ERROR_STOP=1",
            "-U",
            "postgres",
            "-d",
            DATABASE,
            "-c",
            query,
            capture=True,
            timeout=10,
        )
    )


def drill(binary, archive):
    prefix = "tradingbot-restore-" + uuid4().hex[:10]
    source, target = prefix + "-source", prefix + "-target"
    with postgres(source) as dsn:
        with control(binary, dsn) as origin:
            paper_path, _ = check_paper_order(origin)
            events_path = paper_path + "/events"
            events = request(origin, events_path, 200)
            first_fill = events[-1]["payload"]
            first_order_path = paper_path + "/orders/" + first_fill["order_id"]
            first_order = request(origin, first_order_path, 200)
            pending_path, unknown, next_fill = paper_pending(origin, paper_path, first_fill)
            before_account = request(origin, paper_path, 200)
            checked(
                before_account["reserved_cash_usd"] == "21",
                "wrong UNKNOWN cash reservation",
            )
            before_events = request(origin, events_path, 200)

            completed = enqueue(origin, "before-restore")
            lease = request(origin, JOBS + "/claim", 200, claim_request())
            checked(
                lease["job_id"] == completed["job_id"],
                "claimed the wrong completed fixture",
            )
            completion_path, fixture, receipt = complete_fixture(origin, completed, lease)
            pending = enqueue(origin, "resume-after-restore")
            lease = request(origin, JOBS + "/claim", 200, claim_request())
            checked(
                lease["job_id"] == pending["job_id"],
                "claimed the wrong pending fixture",
            )
            pending_job_path = f"{JOBS}/{pending['job_id']}"
            call = {
                "worker_id": lease["worker_id"],
                "lease_token": lease["lease_token"],
                "call_id": str(uuid4()),
            }
            permit_path = pending_job_path + "/authorize-call"
            permit = request(origin, permit_path, 201, call)
            checked(
                permit["remaining_llm_calls"] == pending["max_llm_calls"] - 1,
                "wrong permit budget",
            )
        before = database_counts(source)
        checked(
            before
            == {
                "accounts": 1,
                "orders": 2,
                "fills": 1,
                "events": 6,
                "paper_outbox": 1,
                "paper_outbox_pending": 1,
                "jobs": 2,
                "attempts": 2,
                "permits": 1,
                "results": 1,
                "completion_events": 1,
            },
            "source fixture lacks the expected durable history",
        )
        docker(
            "exec",
            source,
            "pg_dump",
            "-U",
            "postgres",
            "-d",
            DATABASE,
            "-Fc",
            "--file",
            DUMP_PATH,
            timeout=30,
        )
        docker("cp", f"{source}:{DUMP_PATH}", str(archive), timeout=15)

    with postgres(target) as dsn:
        docker("cp", str(archive), f"{target}:{DUMP_PATH}", timeout=15)
        docker(
            "exec",
            target,
            "pg_restore",
            "-U",
            "postgres",
            "-d",
            DATABASE,
            "--single-transaction",
            "--exit-on-error",
            "--no-owner",
            "--no-privileges",
            DUMP_PATH,
            timeout=30,
        )
        checked(database_counts(target) == before, "restored row counts changed")
        with control(binary, dsn) as origin:
            checked(
                request(origin, paper_path, 200) == before_account,
                "restored account changed",
            )
            checked(
                request(origin, events_path, 200) == before_events,
                "restored paper history changed",
            )
            checked(
                request(origin, pending_path, 200) == unknown,
                "restored UNKNOWN order changed",
            )
            checked(
                request(origin, first_order_path, 200) == first_order,
                "restored fill changed",
            )
            checked(
                request(origin, first_order_path + "/resolve", 200, first_fill) == first_order,
                "past fill reapplied",
            )
            result_path = f"{JOBS}/{completed['job_id']}/result"
            checked(request(origin, result_path, 200) == receipt, "restored result changed")
            checked(
                request(origin, completion_path, 200, fixture) == receipt,
                "completion replay changed",
            )
            request(origin, JOBS, 200, completed)
            request(origin, pending_job_path + "/result", 404)
            active_lease = datetime.now(UTC) + timedelta(seconds=2) < datetime.fromisoformat(
                lease["lease_expires_at"]
            )
            if active_lease:
                checked(
                    request(origin, permit_path, 200, call) == permit,
                    "restored permit changed",
                )
                request(origin, JOBS + "/claim", 204, claim_request())

            settled = request(origin, pending_path + "/resolve", 200, next_fill)
            checked(
                request(origin, pending_path + "/resolve", 200, next_fill) == settled,
                "recovery fill repeated",
            )
            account = request(origin, paper_path, 200)
            checked(
                account["cash_usd"] == "959"
                and account["reserved_cash_usd"] == "0"
                and account["positions"][0]["quantity_raw"] == "250"
                and account["version"] == before_account["version"] + 1,
                "paper reconciliation did not settle exactly once",
            )

            # PostgreSQL's original lease still governs eligibility after restore.
            claim = claim_request()
            deadline = time.monotonic() + 70
            while time.monotonic() < deadline:
                retry = request(origin, JOBS + "/claim", (200, 204), claim)
                if retry is not None:
                    break
                time.sleep(0.25)
            else:
                raise RuntimeError("pending job did not resume after its original lease")
            checked(
                retry["job_id"] == pending["job_id"] and retry["attempt"] == 2,
                "wrong recovery attempt",
            )
            request(origin, permit_path, 409, call)
            recovery_path, recovery_fixture, recovered = complete_fixture(origin, pending, retry)
            checked(
                request(origin, recovery_path, 200, recovery_fixture) == recovered,
                "recovery completion repeated",
            )
            request(origin, JOBS + "/claim", 204, claim_request())

        after = database_counts(target)
        expected = before | {
            key: before[key] + 1
            for key in (
                "fills",
                "events",
                "paper_outbox",
                "paper_outbox_pending",
                "attempts",
                "results",
                "completion_events",
            )
        }
        checked(after == expected, "recovery duplicated or lost durable effects")

    return {
        "backup_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
        "backup_bytes": archive.stat().st_size,
        "restored_counts": before,
        "resumed_counts": after,
        "active_permit_replayed": active_lease,
        "recovered_research_attempt": recovered["attempt"],
    }


def main():
    with tempfile.TemporaryDirectory(prefix="tradingbot-restore-") as workspace:
        directory = Path(workspace)
        binary = directory / ("control.exe" if os.name == "nt" else "control")
        subprocess.run(
            [
                "go",
                "-C",
                "services/control",
                "build",
                "-mod=readonly",
                "-o",
                str(binary),
                "./cmd/control",
            ],
            cwd=ROOT,
            env=os.environ | {"GOPROXY": "off", "GOTOOLCHAIN": "local"},
            check=True,
            timeout=90,
        )
        print(json.dumps(drill(binary, directory / "tradingbot.dump"), indent=2))


if __name__ == "__main__":
    main()
