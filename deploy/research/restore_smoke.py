"""Drill a local PostgreSQL restore, optionally with a paper scheduler journal."""

import argparse
import hashlib
import json
import os
import sqlite3
import subprocess
import tarfile
import tempfile
import time
from contextlib import closing, contextmanager
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
    write_paper_entry_plan,
)

POSTGRES_IMAGE = (
    "postgres:17-alpine@sha256:d3244e8ce3f165ee3c93d9c4a4b1d0fe48b3a89e2287b717fdcbbde92d405065"
)
DATABASE = "tradingbot"
DUMP_PATH = "/tmp/tradingbot-restore-drill.dump"
COMPOSE_ROLE = "research_control"
COMPOSE_START = ("up", "-d", "--wait", "--wait-timeout", "60", "--no-build", "--pull", "never")
COMPOSE_RUN = ("run", "--rm", "-T", "--no-deps", "--pull", "never")


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
def trading_api(binary, dsn):
    address = f"127.0.0.1:{free_port()}"
    origin = "http://" + address
    env = os.environ | {"TRADINGBOT_OFFLINE_TEST_DATABASE_URL": dsn}
    child = subprocess.Popen([str(binary), "--listen", address], env=env)
    try:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            checked(child.poll() is None, "Trading API exited before readiness")
            try:
                status = request(origin, "/readyz", 200)
            except (TimeoutError, URLError):
                time.sleep(0.1)
                continue
            checked(status["storage_enabled"], "Trading API did not enable storage")
            break
        else:
            raise RuntimeError("Trading API did not become ready")
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


def budget_command(binary, dsn, *arguments):
    completed = subprocess.run(
        [str(binary), *arguments],
        env=os.environ | {"TRADINGBOT_DATABASE_URL": dsn},
        stdout=subprocess.PIPE,
        check=True,
        timeout=30,
    )
    return json.loads(completed.stdout)


def drill(binary, budget, archive):
    prefix = "tradingbot-restore-" + uuid4().hex[:10]
    source, target = prefix + "-source", prefix + "-target"
    with postgres(source) as dsn:
        with trading_api(binary, dsn) as origin:
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
            day = docker(
                "exec", source, "psql", "-XAt", "-U", "postgres", "-d", DATABASE, "-c",
                "SELECT to_char(granted_at AT TIME ZONE 'UTC','YYYY-MM-DD') "
                "FROM research_call_permits",
                capture=True, timeout=10,
            ).strip()
            daily = budget_command(budget, dsn, "set", "--day", day, "--max-calls", "1")
            checked(
                daily["status"] == "exhausted"
                and daily["recorded_permits"] == 1
                and daily["remaining_permits"] == 0,
                "daily budget did not include the committed permit",
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
        checked(
            budget_command(budget, dsn, "show", "--day", day) == daily,
            "restored daily budget changed",
        )
        with trading_api(binary, dsn) as origin:
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
            request(
                origin, permit_path, 422,
                {
                    "worker_id": retry["worker_id"],
                    "lease_token": retry["lease_token"],
                    "call_id": str(uuid4()),
                },
            )
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
        "restored_daily_budget": daily,
        "recovered_research_attempt": recovered["attempt"],
    }


@contextmanager
def scheduler_project(project, archive_root, service, **settings):
    port = str(free_port())
    env = (
        os.environ
        | {
            "TRADINGBOT_CONTROL_PORT": port,
            "TRADINGBOT_PAPER_ARCHIVE_DIR": str(archive_root),
        }
        | settings
    )
    # The review drill uses a one-shot paper entry to create its opening.
    profiles = (service,) if service == "paper-entry" else (service, "paper-entry")
    profile_args = [arg for profile in profiles for arg in ("--profile", profile)]
    images_only = archive_root.parent / "images-only.compose.yaml"
    # The scheduler build also uses the Trading API image.
    images_only.write_text(
        "services:\n"
        + "".join(f"  {name}:\n    build: !reset null\n" for name in ("trading-api", "accounts", *profiles)),
        encoding="utf-8",
    )

    def compose(*args, **kwargs):
        return docker(
            "compose",
            "-p",
            project,
            "-f",
            "deploy/research/compose.yaml",
            "-f",
            str(images_only),
            *profile_args,
            *args,
            env=env,
            timeout=90,
            **kwargs,
        )

    try:
        yield compose, f"http://127.0.0.1:{port}"
    finally:
        compose("down", "--volumes")


SCHEDULER_DATABASES = (("postgres", COMPOSE_ROLE, DATABASE), ("accounts-postgres", "accounts", "accounts"))


def compose_sql(compose, query, database=SCHEDULER_DATABASES[0]):
    container, user, name = database
    return compose(
        "exec",
        "-T",
        container,
        "psql",
        "-XAt",
        "-v",
        "ON_ERROR_STOP=1",
        "-U",
        user,
        "-d",
        name,
        "-c",
        query,
        capture=True,
    ).strip()


def scheduler_state(compose):
    jobs = json.loads(
        compose_sql(
            compose,
            """
        SELECT json_build_object(
            'jobs', (SELECT count(*) FROM research_jobs),
            'queued_events', (SELECT count(*) FROM research_outbox
                WHERE event_type = 'research_job.queued'),
            'permits', (SELECT count(*) FROM research_call_permits),
            'attempts', (SELECT count(*) FROM research_job_attempts),
            'results', (SELECT count(*) FROM research_job_results),
            'dispatch', (SELECT coalesce(json_agg(json_build_object(
                'job_id', j.id, 'status', j.status, 'created_at', j.created_at,
                'request', convert_from(j.request, 'UTF8'),
                'job_sha256', encode(j.request_sha256, 'hex'),
                'snapshot', convert_from(s.payload, 'UTF8'),
                'snapshot_sha256', encode(s.payload_sha256, 'hex'),
                'account_id', d.account_id,
                'dispatch_request', convert_from(d.request, 'UTF8'),
                'dispatch_sha256', encode(d.request_sha256, 'hex'),
                'dispatch_status', d.finished_status, 'dispatch_at', d.finished_at
            ) ORDER BY j.created_at, j.id), '[]')
            FROM research_paper_dispatch d JOIN research_jobs j ON j.id = d.job_id
            JOIN research_snapshots s ON s.id = j.snapshot_id)
        )
    """,
        )
    )

    accounts = json.loads(compose_sql(compose, """
        SELECT json_build_object(
            'orders', (SELECT count(*) FROM paper_orders),
            'fills', (SELECT count(*) FROM paper_fills),
            'paper_events', (SELECT count(*) FROM paper_events)
        )
    """, SCHEDULER_DATABASES[1]))
    return jobs | accounts


def compose_clock(compose):
    return datetime.fromisoformat(compose_sql(compose, "SELECT clock_timestamp()::text"))


def wait_until(compose, target):
    delta = (target - compose_clock(compose)).total_seconds()
    checked(delta < 60, "restore fixture unexpectedly far from its cutoff")
    if delta > 0:
        time.sleep(delta + 0.1)
    checked(compose_clock(compose) > target, "PostgreSQL has not passed the expected cutoff")


def journal_transfer(project, archive, service, *, restore=False):
    volume = docker(
        "volume",
        "ls",
        "--quiet",
        "--filter",
        f"label=com.docker.compose.project={project}",
        "--filter",
        f"label=com.docker.compose.volume={service}",
        capture=True,
        timeout=10,
    ).splitlines()
    checked(len(volume) == 1, "expected exactly one disposable scheduler journal volume")
    journal_mount = f"type=volume,source={volume[0]},target=/journal"
    backup_mount = f"type=bind,source={archive.parent},target=/backup"
    docker(
        "run",
        "--rm",
        "--network",
        "none",
        "--pull=never",
        "--entrypoint",
        "tar",
        "--mount",
        journal_mount if restore else journal_mount + ",readonly",
        "--mount",
        backup_mount + ",readonly" if restore else backup_mount,
        POSTGRES_IMAGE,
        "-xf" if restore else "-cf",
        f"/backup/{archive.name}",
        "-C",
        "/journal",
        *(() if restore else (".",)),
        timeout=30,
    )


def scheduler_dump_files(dump):
    return (
        (SCHEDULER_DATABASES[0], dump),
        (SCHEDULER_DATABASES[1], dump.with_name(dump.stem + "-accounts" + dump.suffix)),
    )


def dump_scheduler_database(compose, dump):
    compose("stop", "trading-api", "accounts")
    for (container, user, name), target in scheduler_dump_files(dump):
        compose("exec", "-T", container, "pg_dump", "-U", user, "-d", name, "-Fc", "--file", DUMP_PATH)
        compose("cp", f"{container}:{DUMP_PATH}", str(target))


def restore_scheduler_database(compose, dump, service):
    compose(*COMPOSE_START, "postgres", "accounts-postgres")
    for (container, user, name), source in scheduler_dump_files(dump):
        compose("cp", str(source), f"{container}:{DUMP_PATH}")
        compose(
            "exec", "-T", container, "pg_restore", "-U", user, "-d", name,
            "--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges", DUMP_PATH,
        )
    compose(*COMPOSE_START, "trading-api", "accounts")
    # Initialize the empty journal volume under the configured scheduler user.
    compose(*COMPOSE_RUN, "--entrypoint", "/bin/true", service)


def journal_documents(archive):
    with tarfile.open(archive) as contents:
        documents = {}
        for member in contents:
            if member.isdir() and member.name == ".":
                continue
            checked(member.isfile(), "entry journal archive has an unexpected member")
            document = contents.extractfile(member).read()
            staged = json.loads(document)
            raw, selection = staged["admission"], staged["selection"]
            admission = json.loads(raw)
            checked(
                hashlib.sha256(raw.encode()).hexdigest() == staged["admission_sha256"]
                and hashlib.sha256(selection.encode()).hexdigest() == admission["selection_sha256"],
                "staged admission or selection hash differs",
            )
            documents[admission["scheduled_at"]] = (member.name, document)
        checked(len(documents) == 2, "expected two distinct staged occurrence documents")
        return documents


# Deliberately interrupt only the second submission after the real runner
# staged its Go snapshot and pinned screen. The first submission reaches Go.
BLOCK_ENTRY = """
import json
import subprocess
import sys
from pathlib import Path

from tradingbot.entry_cycles import run_cycles

ref, digest, block_at = sys.argv[1:]

def invoke(*args):
    if args[0] == "schedule-entry":
        config = json.loads(Path(args[args.index("--config") + 1]).read_bytes())
        if config["scheduled_at"] == block_at:
            raise subprocess.TimeoutExpired(args[0], 0)
    return json.loads(subprocess.check_output(["/usr/local/bin/paper", *args], timeout=45))

result = run_cycles(
    Path("/archive"), {"ref": ref, "sha256": digest}, Path("/journal"), invoke, settled={},
)
print(json.dumps(result))
"""


def entry_journal_drill(directory):
    prefix = "tradingbot-entry-restore-" + uuid4().hex[:10]
    source, target = prefix + "-source", prefix + "-target"
    root = directory / "archive"
    root.mkdir(mode=0o755)
    (root / "restore-fault.py").write_text(BLOCK_ENTRY, encoding="utf-8")
    dump = directory / "tradingbot.dump"
    journal = directory / "entry-journal.tar"
    account_id = str(uuid4())
    account_path = "/v1/paper/accounts"
    service, volume = "paper-entry", "paper-entry-journal"

    with scheduler_project(source, root, service) as (compose, origin):
        compose(*COMPOSE_START, "postgres", "accounts", "trading-api")
        account = request(
            origin,
            account_path,
            201,
            {"account_id": account_id, "initial_cash_usd": "1000"},
        )
        first = compose_clock(compose) + timedelta(seconds=2)
        plan, _ = write_paper_entry_plan(root, account_id, first)
        plan_path = root / plan["ref"]
        plan_doc = json.loads(plan_path.read_bytes())
        first_occurrence, second_occurrence = plan_doc["occurrences"]
        first_occurrence["max_lateness_seconds"] = 36
        second_occurrence["max_lateness_seconds"] = 180
        policy_path = root / plan_doc["policy"]["ref"]
        policy = json.loads(policy_path.read_bytes()) | {"max_mark_age_seconds": 240}
        policy_path.write_bytes(json.dumps(policy).encode())
        plan_doc["policy"]["sha256"] = hashlib.sha256(policy_path.read_bytes()).hexdigest()
        plan_path.write_bytes(json.dumps(plan_doc).encode())
        plan_sha = hashlib.sha256(plan_path.read_bytes()).hexdigest()
        arguments = (
            "--root",
            "/archive",
            "--plan-ref",
            plan["ref"],
            "--plan-sha256",
            plan_sha,
            "--journal",
            "/journal",
        )
        first_at = first_occurrence["scheduled_at"]
        second_at = second_occurrence["scheduled_at"]
        def stage_entries():
            return json.loads(
                compose(
                    *COMPOSE_RUN,
                    "--entrypoint",
                    "python",
                    "paper-entry",
                    "/archive/restore-fault.py",
                    plan["ref"],
                    plan_sha,
                    second_at,
                    capture=True,
                )
            )["occurrences"]

        wait_until(compose, datetime.fromisoformat(first_at))
        accepted, staged_only = stage_entries()
        checked(
            accepted["status"] == "queued" and accepted["created"] is True,
            f"source did not accept the first occurrence: {accepted['status']}",
        )
        if staged_only["status"] == "not_due":
            wait_until(compose, datetime.fromisoformat(second_at))
            _, staged_only = stage_entries()
        checked(
            staged_only["status"] == "pending_go" and staged_only.get("error") == "TimeoutExpired",
            f"source did not interrupt the second occurrence: {staged_only['status']}",
        )
        before = scheduler_state(compose)
        checked(
            before["jobs"] == before["queued_events"] == len(before["dispatch"]) == 1
            and before["permits"] == before["orders"] == 0,
            "source must have one queued entry and one unsubmitted staged entry",
        )
        dump_scheduler_database(compose, dump)
        journal_transfer(source, journal, volume)
        documents = journal_documents(journal)
        checked(set(documents) == {first_at, second_at}, "archived entry cutoffs differ")

    with scheduler_project(target, root, service) as (compose, origin):
        restore_scheduler_database(compose, dump, service)
        journal_transfer(target, journal, volume, restore=True)
        checked(scheduler_state(compose) == before, "restored job or dispatch binding differs")
        checked(request(origin, f"{account_path}/{account_id}", 200) == account, "account differs")
        wait_until(
            compose,
            datetime.fromisoformat(first_at)
            + timedelta(seconds=first_occurrence["max_lateness_seconds"]),
        )

        def resume():
            result = json.loads(
                compose(
                    *COMPOSE_RUN,
                    "paper-entry",
                    *arguments,
                    "--paper-cli",
                    "/usr/local/bin/paper",
                    capture=True,
                )
            )
            checked(result["owner_plan_sha256"] == plan_sha, "restored owner plan changed")
            return result["occurrences"]

        original, recovered = resume()
        existing = before["dispatch"][0]
        checked(
            original["status"] == recovered["status"] == "queued"
            and original["created"] is False
            and recovered["created"] is True
            and original["job_id"] == existing["job_id"]
            and datetime.fromisoformat(original["created_at"])
            == datetime.fromisoformat(existing["created_at"])
            and recovered["job_id"] != original["job_id"],
            "restore did not replay the accepted job and admit the staged job",
        )
        after = scheduler_state(compose)
        restored_job = after["dispatch"][-1]
        staged = json.loads(documents[second_at][1])
        selected = json.loads(staged["selection"])["job"]
        restored_request = json.loads(restored_job["request"])
        checked(
            after["jobs"] == after["queued_events"] == len(after["dispatch"]) == 2
            and after["permits"] == after["orders"] == 0
            and after["dispatch"][0] == existing
            and restored_job["job_id"] == recovered["job_id"]
            and restored_job["account_id"] == account_id
            and all(
                restored_request[key] == selected[key]
                for key in (
                    "data_manifest",
                    "requested_symbols",
                    "policy_version",
                    "max_llm_calls",
                )
            )
            and restored_request["portfolio"]["snapshot_id"]
            == selected["portfolio"]["snapshot_id"],
            "recovery duplicated a durable effect or changed the staged binding",
        )
        checked(
            resume() == [original, recovered | {"created": False}]
            and scheduler_state(compose) == after,
            "second recovery attempt changed the durable jobs",
        )
        restored_tar = directory / "restored-entry-journal.tar"
        journal_transfer(target, restored_tar, volume)
        checked(journal_documents(restored_tar) == documents, "recovery changed journal bytes")
    return {
        "source_mode": "synthetic_fixture",
        "owner_plan_sha256": plan_sha,
        "backup_sha256": hashlib.sha256(dump.read_bytes()).hexdigest(),
        "entry_journal_sha256": hashlib.sha256(journal.read_bytes()).hexdigest(),
        "staged_entries": len(documents),
        "replayed_job_id": original["job_id"],
        "recovered_job_id": recovered["job_id"],
        "durable_jobs_before": before["jobs"],
        "durable_jobs_after": after["jobs"],
    }


# Drop the successful Go response before the Python runner stores it. The
# same probe checks the first request sent by the restored runner.
REVIEW_ACK_PROBE = """
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path

from tradingbot import review_cycles

expected = sys.argv.pop(1)
run = subprocess.run

def observe(command, **options):
    global expected
    try:
        reply = run(command, **options)
    except subprocess.CalledProcessError as exc:
        raise RuntimeError(exc.stderr.decode().strip()) from None
    if command[1] == "schedule-review" and expected:
        path = Path(command[command.index("--config") + 1])
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        if expected == "drop":
            print(json.dumps({"request_sha256": digest, "receipt": json.loads(reply.stdout)}), flush=True)
            os._exit(70)
        if digest != expected:
            raise RuntimeError("first restored submission changed its frozen request bytes")
        expected = ""
    return reply

review_cycles.subprocess.run = observe
status = review_cycles.main()
if expected:
    raise RuntimeError("runner did not submit the expected review request")
raise SystemExit(status)
"""


def review_journal_rows(archive):
    snapshot = archive.with_suffix(".sqlite3")
    with tarfile.open(archive) as contents:
        files = [member for member in contents if member.name != "."]
        checked(
            len(files) == 1
            and files[0].isfile()
            and files[0].name.removeprefix("./") == "reviews.sqlite3",
            "expected one quiesced review SQLite database in the journal backup",
        )
        with contents.extractfile(files[0]) as database:
            snapshot.write_bytes(database.read())
    with closing(sqlite3.connect(snapshot.as_uri() + "?mode=ro", uri=True)) as database:
        checked(
            database.execute("PRAGMA integrity_check").fetchone() == ("ok",),
            "review journal failed SQLite integrity check",
        )
        identity = database.execute("SELECT identity FROM owner_plan WHERE singleton=1").fetchone()
        rows = database.execute(
            "SELECT position, request, request_sha256, receipt, receipt_sha256 "
            "FROM reviews ORDER BY position"
        ).fetchall()
    checked(identity is not None, "review journal lost its owner plan pin")
    for _, request_bytes, request_hash, receipt_bytes, receipt_hash in rows:
        for blob, digest in ((request_bytes, request_hash), (receipt_bytes, receipt_hash)):
            checked(
                (blob is None and digest is None)
                or (blob is not None and hashlib.sha256(blob).hexdigest() == digest),
                "review journal checksum differs",
            )
    return identity[0], rows


def review_journal_drill(directory):
    from review_restore_fixture import review_registry_sha256, seed_review_restore

    prefix = "tradingbot-review-restore-" + uuid4().hex[:10]
    source, target = prefix + "-source", prefix + "-target"
    root = directory / "archive"
    root.mkdir(mode=0o755)
    (root / "review-ack-probe.py").write_text(REVIEW_ACK_PROBE, encoding="utf-8")
    dump = directory / "tradingbot.dump"
    journal = directory / "review-journal.tar"
    account_id = str(uuid4())
    account_path = f"/v1/paper/accounts/{account_id}"
    service, volume = "paper-review-cycles", "paper-review-journal"
    registry = review_registry_sha256()
    settings = {"TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256": registry}

    def probe(compose, expected):
        return compose(
            *COMPOSE_RUN,
            "--entrypoint",
            "python",
            service,
            "/archive/review-ack-probe.py",
            expected,
            *arguments,
            capture=True,
        )

    with scheduler_project(source, root, service, **settings) as (compose, origin):
        compose(*COMPOSE_START, "postgres", "accounts", "trading-api")
        plan, seed_registry = seed_review_restore(
            compose,
            origin,
            root,
            account_id,
            sql=lambda query: compose_sql(compose, query),
            clock=lambda: compose_clock(compose),
        )
        checked(seed_registry == registry, "seeded review registry differs from the owner pin")
        plan_doc = json.loads((root / plan["ref"]).read_bytes())
        first, second = plan_doc["occurrences"]
        checked(
            first["opening_order_id"] == second["opening_order_id"]
            and first["event"] == second["event"],
            "review windows do not share the same catalyst",
        )
        first_release = datetime.fromisoformat(first["release_at"])
        second_release = datetime.fromisoformat(second["release_at"])
        first_deadline = first_release + timedelta(seconds=first["max_lateness_seconds"])
        arguments = (
            "--root",
            "/archive",
            "--plan-ref",
            plan["ref"],
            "--plan-sha256",
            plan["sha256"],
            "--journal",
            "/journal",
            "--paper-cli",
            "/usr/local/bin/paper",
            "--identity-registry-sha256",
            registry,
        )
        account = request(origin, account_path, 200)
        baseline = scheduler_state(compose)
        checked(
            baseline["orders"]
            == baseline["fills"]
            == baseline["jobs"]
            == baseline["results"]
            == baseline["queued_events"]
            == len(baseline["dispatch"])
            == 1
            and baseline["dispatch"][0]["status"] == "completed"
            and baseline["dispatch"][0]["dispatch_status"] == "applied",
            "review seed must have exactly one completed and filled opening",
        )
        wait_until(compose, first_release)
        try:
            output = probe(compose, "drop")
        except subprocess.CalledProcessError as interrupted:
            if interrupted.returncode != 70:
                raise
            lost = json.loads(interrupted.stdout)
        else:
            raise RuntimeError(f"scheduler exited without losing an acknowledgement: {output}")
        receipt, staged_hash = lost["receipt"], lost["request_sha256"]
        job_id = receipt["job_id"]
        checked(
            receipt["created"]
            and receipt["status"] == "queued"
            and receipt["trigger"]["reason"] == "catalyst"
            and receipt["trigger"]["event_record_id"] == first["event"]["record_id"],
            "lost acknowledgement did not refer to the exact accepted catalyst",
        )
        before = scheduler_state(compose)
        matching = [row for row in before["dispatch"] if row["job_id"] == job_id]
        checked(len(matching) == 1, "acknowledged review did not commit its dispatch")
        (admitted,) = matching
        expected_state = baseline | {
            "jobs": baseline["jobs"] + 1,
            "queued_events": baseline["queued_events"] + 1,
            "dispatch": [*baseline["dispatch"], admitted],
        }
        dispatch = json.loads(admitted["dispatch_request"])
        checked(
            before == expected_state
            and admitted["status"] == "queued"
            and admitted["account_id"] == account_id
            and dispatch["review_origin"]["opening_order_id"] == first["opening_order_id"]
            and dispatch["identity_binding"]["identity_registry"]["sha256"] == registry,
            "lost acknowledgement did not leave exactly one bound review",
        )
        for payload, digest in (
            ("request", "job_sha256"),
            ("snapshot", "snapshot_sha256"),
            ("dispatch_request", "dispatch_sha256"),
        ):
            checked(
                hashlib.sha256(admitted[payload].encode()).hexdigest() == admitted[digest],
                "accepted review payload does not match its PostgreSQL checksum",
            )
        dump_scheduler_database(compose, dump)
        journal_transfer(source, journal, volume)
        identity, staged = review_journal_rows(journal)
        checked(
            json.loads(identity)["plan"] == plan
            and len(staged) == 1
            and staged[0][0] == 0
            and staged[0][2:] == (staged_hash, None, None),
            "backup lost the staged bytes or recorded an acknowledgement too soon",
        )

    with scheduler_project(target, root, service, **settings) as (compose, origin):
        restore_scheduler_database(compose, dump, service)
        journal_transfer(target, journal, volume, restore=True)
        checked(
            scheduler_state(compose) == before, "restored review job, snapshot or dispatch differs"
        )
        checked(request(origin, account_path, 200) == account, "restored review holding differs")
        wait_until(compose, max(first_deadline, second_release))
        report = json.loads(probe(compose, staged_hash))
        original, newer = report["occurrences"]
        checked(report["owner_plan_sha256"] == plan["sha256"], "restored review changed owner plan")
        for row, status in ((original, "queued"), (newer, "already_scheduled")):
            checked(
                row["status"] == status
                and row["created"] is False
                and row["job_id"] == job_id
                and datetime.fromisoformat(row["created_at"])
                == datetime.fromisoformat(admitted["created_at"]),
                "restored window did not acknowledge the original durable review",
            )
        replay = json.loads(compose(*COMPOSE_RUN, service, *arguments, capture=True))
        checked(
            replay == report
            and scheduler_state(compose) == before
            and request(origin, account_path, 200) == account,
            "review retries changed PostgreSQL or returned a duplicate effect",
        )
        restored_tar = directory / "restored-review-journal.tar"
        journal_transfer(target, restored_tar, volume)
        restored_identity, rows = review_journal_rows(restored_tar)
        checked(
            restored_identity == identity
            and len(rows) == 2
            and rows[0][:3] == staged[0][:3]
            and rows[1][0] == 1
            and rows[1][1] is not None
            and rows[1][2] != staged_hash
            and [json.loads(row[3])["job_id"] for row in rows] == [job_id, job_id],
            "restored journal lost frozen bytes or acknowledged the wrong job",
        )
        health = json.loads(
            compose(
                "exec",
                "-T",
                "trading-api",
                "/usr/local/bin/paper-check",
                "--account",
                account_id,
                "--job",
                job_id,
                "--pipeline",
                capture=True,
            )
        )
        pipeline, budget = health["pipeline"], health["research"]
        review = next(stage for stage in pipeline["jobs"] if stage["job_id"] == job_id)
        checked(
            health["status"] == "ok"
            and pipeline["selected_job_registered"] is True
            and review["stage"] == "queued"
            and budget["guaranteed_remaining_llm_calls"] == first["max_llm_calls"]
            and budget["durable_permits"] == 0,
            "paper-check did not verify the restored review queue and budget",
        )
        claim = claim_request() | {"identity_registry_sha256": registry}
        lease = request(origin, f"{JOBS}/claim", 200, claim)
        checked(
            lease["job_id"] == job_id
            and lease["attempt"] == 1
            and lease["request"] == json.loads(admitted["request"])
            and request(origin, f"{JOBS}/claim", 200, claim) == lease,
            "worker could not acquire and replay the first lease of the restored review",
        )
        expected_leased = before | {
            "attempts": before["attempts"] + 1,
            "dispatch": [
                row | {"status": "leased"} if row["job_id"] == job_id else row
                for row in before["dispatch"]
            ],
        }
        checked(
            scheduler_state(compose) == expected_leased,
            "restored worker lease changed state beyond one attempt",
        )

    return {
        "source_mode": "synthetic_fixture",
        "owner_plan_sha256": plan["sha256"],
        "backup_sha256": hashlib.sha256(dump.read_bytes()).hexdigest(),
        "review_journal_sha256": hashlib.sha256(journal.read_bytes()).hexdigest(),
        "staged_request_sha256": staged_hash,
        "review_job_id": job_id,
        "dispatch_sha256": admitted["dispatch_sha256"],
        "window_statuses": [original["status"], newer["status"]],
        "durable_review_jobs": before["jobs"] - baseline["jobs"],
        "restored_pipeline_stage": review["stage"],
        "guaranteed_remaining_llm_calls": budget["guaranteed_remaining_llm_calls"],
        "restored_review_attempt": lease["attempt"],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument(
        "--entry-journal",
        action="store_true",
        help="verify PostgreSQL and the entry journal using cached local Compose images",
    )
    mode.add_argument(
        "--review-journal",
        action="store_true",
        help="verify PostgreSQL and the review journal using cached local Compose images",
    )
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="tradingbot-restore-") as workspace:
        directory = Path(workspace)
        if args.entry_journal:
            print(json.dumps(entry_journal_drill(directory), indent=2))
            return
        if args.review_journal:
            print(json.dumps(review_journal_drill(directory), indent=2))
            return
        extension = ".exe" if os.name == "nt" else ""
        binary = directory / ("trading-api" + extension)
        budget = directory / ("research-budget" + extension)
        subprocess.run(
            [
                "go",
                "-C",
                "services/trading-api",
                "build",
                "-mod=readonly",
                "-o",
                str(directory),
                "./cmd/trading-api",
                "./cmd/research-budget",
            ],
            cwd=ROOT,
            env=os.environ | {"GOPROXY": "off", "GOTOOLCHAIN": "local"},
            check=True,
            timeout=90,
        )
        print(json.dumps(drill(binary, budget, directory / "tradingbot.dump"), indent=2))


if __name__ == "__main__":
    main()
