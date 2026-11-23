"""Opt-in cross-process verification against a dedicated, disposable PostgreSQL database."""

import base64
import hashlib
import json
import os
import socket
import subprocess
import sys
import time
from contextlib import closing, contextmanager
from datetime import UTC, datetime, timedelta
from http.client import HTTPConnection
from pathlib import Path
from uuid import uuid4

import pytest

from tradingbot.paper_memory import build_contexts
from tradingbot.security_identity import load_identity_snapshot
from tradingbot.selection import plan_research
from tradingbot.snapshots import load_snapshot

ROOT = Path(__file__).resolve().parents[3]
DATABASE_URL = os.environ.get("TRADINGBOT_E2E_DATABASE_URL")
CONNECT = socket.socket.connect
pytestmark = pytest.mark.skipif(
    not DATABASE_URL, reason="TRADINGBOT_E2E_DATABASE_URL must point to a disposable database"
)


class LoopbackConnection(HTTPConnection):
    def connect(self):
        # The global test guard still blocks DNS, proxies and external destinations.
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        CONNECT(self.sock, ("127.0.0.1", self.port))


def request(port, path, body=None):
    # Let Control return its own three-second database timeout to the caller.
    with closing(LoopbackConnection("127.0.0.1", port, timeout=5)) as conn:
        if body is None:
            conn.request("GET", path)
        else:
            conn.request("POST", path, json.dumps(body), {"Content-Type": "application/json"})
        response = conn.getresponse()
        return response.status, json.loads(response.read())


@contextmanager
def process(command, env=None):
    with subprocess.Popen(command, cwd=ROOT / "services/research", env=env) as child:
        try:
            yield child
        finally:
            if child.poll() is None:
                child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)


def wait_for_resource(port, path, *children, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for child in children:
            assert child.poll() is None, f"service exited with {child.returncode}"
        try:
            status, body = request(port, path)
        except OSError:
            status = None
        if status == 200:
            return body
        time.sleep(0.05)
    pytest.fail(f"timed out waiting for {path}")


def test_bound_research_reuses_selected_issuer_through_real_postgres(tmp_path):
    examples = ROOT / "services/research/examples/selection"
    request_doc = json.loads((examples / "identity-request.json").read_bytes())
    cutoff = datetime.now(UTC) - timedelta(seconds=10)
    shift = cutoff - datetime.fromisoformat(request_doc["portfolio"]["as_of"])

    def rebase(value):
        if isinstance(value, list):
            return [rebase(item) for item in value]
        if isinstance(value, dict):
            return {
                key: (
                    (datetime.fromisoformat(item) + shift).isoformat()
                    if key in {"event_time", "available_at", "ingested_at", "effective_at", "expires_at"}
                    and isinstance(item, str)
                    else rebase(item)
                )
                for key, item in value.items()
            }
        return value

    for field in ("data_manifest", "identity_registry"):
        ref = request_doc[field]["ref"]
        raw = json.dumps(rebase(json.loads((examples / ref).read_bytes()))).encode()
        (tmp_path / ref).write_bytes(raw)
        request_doc[field]["sha256"] = hashlib.sha256(raw).hexdigest()

    request_doc["job_id"] = str(uuid4())
    request_doc["portfolio"].update(snapshot_id=str(uuid4()), as_of=cutoff.isoformat())
    registry = request_doc["identity_registry"]
    selected = plan_research(tmp_path, request_doc, trusted_identity_registry=registry)
    job = selected["job"]
    assert job["job_schema_version"] == "0.2"
    trading_api = build_go_command(tmp_path, "trading-api")
    env = offline_test_environment(DATABASE_URL) | {
        "TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256": registry["sha256"],
    }
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    with process([trading_api, "--listen", f"127.0.0.1:{port}"], env) as control_process:
        wait_for_resource(port, "/readyz", control_process)
        assert request(port, "/v1/research/jobs", job)[0] == 201
        with closing(LoopbackConnection("127.0.0.1", port, timeout=5)) as connection:
            connection.request(
                "POST", "/v1/research/jobs/claim",
                json.dumps({"worker_id": "fixture-legacy", "claim_id": str(uuid4())}),
                {"Content-Type": "application/json"},
            )
            assert connection.getresponse().status == 204
        route = f"/v1/research/jobs/{job['job_id']}"
        command = [
            sys.executable, "-m", "tradingbot", "worker",
            "--control-url", f"http://127.0.0.1:{port}", "--archive-root", str(tmp_path),
            "--identity-registry-ref", registry["ref"],
            "--identity-registry-sha256", registry["sha256"],
        ]
        with process(command, research_environment()) as running:
            receipt = wait_for_resource(port, route + "/result", control_process, running)
        assert request(port, route + "/result") == (200, receipt)
        saved_job = request(port, route)[1]["request"]

    result = receipt["result"]
    assert result["result_schema_version"] == "0.3"
    assert result["metadata"]["source_mode"] == "tradingagents_offline"
    assert 0 < result["metadata"]["llm_calls"] <= job["max_llm_calls"]
    assert result["metadata"]["usd_cost"] is None
    proof = result["metadata"]["provenance"]
    assert proof["identity_registry"] == saved_job["identity_registry"] == registry
    assert proof["security_bindings"] == saved_job["security_bindings"] == job["security_bindings"]
    independently_projected = load_identity_snapshot(
        tmp_path, saved_job["data_manifest"], registry, cutoff, saved_job["security_bindings"]
    )
    assert proof["projected_snapshot_sha256"] == selected["snapshot_sha256"] == independently_projected.sha256
    assert all(intent["action"] == "HOLD" for intent in result["batch"]["intents"])
    assert "fixture-previous" not in json.dumps(result["reports"])

    exporter = build_go_command(tmp_path, "research-replay-export")
    exported = subprocess.check_output([exporter, "--job", saved_job["job_id"]], env=env, timeout=20)
    export_path = tmp_path / "identity-export.json"
    export_path.write_bytes(exported)
    independently_audited = json.loads(subprocess.check_output(
        [
            sys.executable, "-m", "tradingbot", "verify-identity-export",
            "--input", str(export_path), "--input-sha256", hashlib.sha256(exported).hexdigest(),
            "--archive-root", str(tmp_path), "--identity-registry-ref", registry["ref"],
            "--identity-registry-sha256", registry["sha256"],
        ],
        cwd=ROOT / "services/research", env=research_environment(), timeout=20,
    ))
    assert independently_audited["status"] == "verified"
    assert independently_audited["jobs"][0]["projected_snapshot_sha256"] == selected["snapshot_sha256"]


def build_go_command(tmp_path, name):
    executable = tmp_path / (name + ".exe" if os.name == "nt" else name)
    subprocess.run(
        ["go", "build", "-mod=readonly", "-o", str(executable), f"./cmd/{name}"],
        cwd=ROOT / "services/trading-api",
        check=True,
        timeout=90,
    )
    return str(executable)


def paper_command(executable, policy, env, *arguments):
    result = subprocess.run(
        [executable, *arguments, "--policy", str(policy)],
        env=env,
        stdout=subprocess.PIPE,
        check=True,
        timeout=40,
    )
    return json.loads(result.stdout)


def research_environment():
    storage_and_internal_variables = {
        "TRADINGBOT_DATABASE_URL",
        "TRADINGBOT_TEST_DATABASE_URL",
        "TRADINGBOT_E2E_DATABASE_URL",
        "TRADINGBOT_OFFLINE_TEST_DATABASE_URL",
        "TRADINGBOT_ACCOUNTS_DATABASE_URL",
        "TRADINGBOT_ACCOUNTS_URL",
        "TRADINGBOT_TRADING_API_URL",
        "TRADINGBOT_PAPER_INTERNAL_TOKEN",
        "TRADINGBOT_PAPER_INTERNAL_TOKEN_FILE",
    }
    return {key: value for key, value in os.environ.items() if key not in storage_and_internal_variables}


def offline_test_environment(dsn):
    return research_environment() | {
        "TRADINGBOT_DATABASE_URL": dsn,
        "TRADINGBOT_OFFLINE_TEST_DATABASE_URL": dsn,
    }


def test_worker_crash_preserves_observed_calls_and_keeps_orphan_budget_charged(tmp_path):
    executable = build_go_command(tmp_path, "trading-api")
    exporter = build_go_command(tmp_path, "research-usage-export")
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    origin = f"http://127.0.0.1:{port}"
    command = [executable, "-listen", f"127.0.0.1:{port}"]
    env = offline_test_environment(DATABASE_URL)
    book = json.loads((ROOT / "contracts/valid-portfolio.json").read_text(encoding="utf-8"))
    book.update(
        snapshot_id=str(uuid4()),
        as_of=(datetime.now(UTC) - timedelta(seconds=5)).isoformat(),
    )
    job_id = str(uuid4())
    job = {
        "job_schema_version": "0.1",
        "job_id": job_id,
        "portfolio": book,
        "requested_symbols": ["TSLA"],
        "max_llm_calls": 14,
        "data_manifest": {"ref": "offline/graph", "sha256": "a" * 64},
        "policy_version": "offline-policy",
    }

    def usage_archive():
        return json.loads(
            subprocess.check_output([exporter, "--job", job_id], env=env, timeout=10)
        )

    marker = tmp_path / "blocked-run"
    pause_second_permit = """
import sys
from pathlib import Path
from threading import Event, Lock

from tradingbot.worker import _Worker, run_worker

authorize_call = _Worker.authorize_call
confirmed_permits = []
admission = Lock()

def pause(self, lease, watchdog, run_id):
    with admission:
        authorize_call(self, lease, watchdog, run_id)
        confirmed_permits.append(run_id)
        if len(confirmed_permits) == 2:
            marker = Path(sys.argv[2])
            temporary = marker.with_suffix(".tmp")
            temporary.write_text(str(run_id), encoding="utf-8")
            temporary.replace(marker)
            Event().wait()

_Worker.authorize_call = pause
run_worker(sys.argv[1], "pytest-crashed-worker")
"""
    with process(command, env) as control:
        wait_for_resource(port, "/readyz", control)
        assert request(port, "/v1/research/jobs", job)[0] == 201
        with process(
            [sys.executable, "-c", pause_second_permit, origin, str(marker)], research_environment()
        ) as crashing:
            pause_deadline = time.monotonic() + 30
            while not marker.is_file() and time.monotonic() < pause_deadline:
                assert crashing.poll() is None, "scripted crash probe exited early"
                time.sleep(0.025)
            assert marker.is_file(), "second invocation was not granted a permit"
            orphan_id = marker.read_text(encoding="utf-8")
            early = usage_archive()
            while (
                not any(item["callback_payload"] for item in early["permits"])
                and time.monotonic() < pause_deadline
            ):
                time.sleep(0.025)
                early = usage_archive()
            crashing.kill()
            crashing.wait(timeout=5)
        assert usage_archive() == early
        assert early["job_id"] == job_id and early["job_status"] == "leased"
        assert early["legacy_budget_unknown"] is False
        assert early["permit_history_known"] is True
        assert len(early["permits"]) == 2
        (observed,) = [item for item in early["permits"] if item["call_id"] != orphan_id]
        (orphan,) = [item for item in early["permits"] if item["call_id"] == orphan_id]
        assert observed["attempt"] == orphan["attempt"] == 1
        assert orphan["callback_payload"] is orphan["callback_sha256"] is orphan["recorded_at"] is None

    recovery_worker = [
        sys.executable, "-m", "tradingbot", "worker", "--control-url", origin,
        "--worker-id", "pytest-recovered-worker", "--poll-interval", "0.05",
    ]
    with process(command, env) as control:
        wait_for_resource(port, "/readyz", control)
        with process(recovery_worker, research_environment()) as recovering:
            finished = wait_for_resource(
                port, f"/v1/research/jobs/{job_id}/result", control, recovering, timeout=90
            )
        exported = usage_archive()
    assert exported["job_status"] == "completed"
    assert exported["permit_history_known"] is True and exported["attempts"] == 2
    assert finished["attempt"] == 2 and finished["result"]["metadata"]["llm_calls"] == 12
    assert len(exported["permits"]) == 14
    assert [item for item in exported["permits"] if item["attempt"] == 1] == early["permits"]
    callbacks = []
    for permit in exported["permits"]:
        if permit["call_id"] == orphan_id:
            assert permit == orphan
            continue
        payload = base64.b64decode(permit["callback_payload"], validate=True)
        assert hashlib.sha256(payload).hexdigest() == permit["callback_sha256"]
        call = json.loads(payload)
        assert call["run_id"] == permit["call_id"] and call["status"] == "completed"
        assert call["model"] == "scripted-offline-v1" and call["provider"] == "offline"
        assert call["usage_source"] == "unavailable" and call["input_tokens"] is None
        assert permit["recorded_at"] is not None
        if permit["attempt"] == 2:
            callbacks.append(call)
    assert len({call["run_id"] for call in callbacks}) == len(callbacks) == 12
    assert sum(call["role"] == "portfolio_allocator" for call in callbacks) == 1


def settle_paper_round_trip(port, account_id, instrument):
    orders = f"/v1/paper/accounts/{account_id}/orders"
    for version, side, gross in ((1, "BUY", "10.00000001"), (3, "SELL", "8.00000001")):
        order_id = str(uuid4())
        code, order = request(
            port,
            orders,
            {
                "account_id": account_id,
                "order_id": order_id,
                "decision_id": str(uuid4()),
                "quote_id": str(uuid4()),
                "snapshot_version": version,
                "instrument": instrument,
                "side": side,
                "quantity_raw": "1",
                "gross_usd": gross,
                "max_fee_usd": "0.00000002",
                "expires_at": (datetime.now(UTC) + timedelta(minutes=1)).isoformat(),
            },
        )
        assert code == 201 and order["status"] == "reserved"
        code, settled = request(
            port,
            f"{orders}/{order_id}/resolve",
            {
                "account_id": account_id,
                "order_id": order_id,
                "event_id": str(uuid4()),
                "outcome": "filled",
                "observed_at": order["created_at"],
                "gross_usd": gross,
                "fee_usd": "0.00000002",
            },
        )
        assert code == 200 and settled["status"] == "filled"
    return settled["fill"]["event_id"]


@pytest.mark.parametrize("archive_mode", [False, True], ids=["fixture", "archive"])
def test_python_graph_result_is_persisted_by_go_control(tmp_path, archive_mode):
    executable = build_go_command(tmp_path, "trading-api")
    if archive_mode:
        paper_apply = build_go_command(tmp_path, "paper")
        paper_memory = build_go_command(tmp_path, "paper-memory")
        policy_path = ROOT / "contracts/valid-paper-policy.json"
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        instrument = next(
            rule["instrument"] for rule in policy["instruments"] if rule["symbol"] == "TSLA"
        )
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    book = json.loads((ROOT / "contracts/valid-portfolio.json").read_text(encoding="utf-8"))
    book.update(
        snapshot_id=str(uuid4()),
        as_of=(datetime.now(UTC) - timedelta(seconds=5)).isoformat(),
    )
    job_id = str(uuid4())
    job = {
        "job_schema_version": "0.1",
        "job_id": job_id,
        "portfolio": book,
        "requested_symbols": ["TSLA"],
        "max_llm_calls": 32,
        "data_manifest": {"ref": "offline/graph", "sha256": "a" * 64},
        "policy_version": "offline-policy",
    }
    path = f"/v1/research/jobs/{job_id}/result"
    ctl = [executable, "-listen", f"127.0.0.1:{port}"]
    env = offline_test_environment(DATABASE_URL)
    worker = [
        sys.executable,
        "-m",
        "tradingbot",
        "worker",
        "--control-url",
        f"http://127.0.0.1:{port}",
        "--worker-id",
        "pytest-offline",
    ]
    if archive_mode:
        observed = book["as_of"]
        bars = []
        for age, close in [(120, "96"), (60, "97"), (0, "98.12345678")]:
            at = (datetime.fromisoformat(observed) - timedelta(seconds=age)).isoformat()
            bars.append(
                {
                    "kind": "bar",
                    "provider": "e2e-fixture",
                    "record_id": f"snapshot-mark-{age}",
                    "symbol": "TSLA",
                    "revision": 1,
                    "event_time": at,
                    "available_at": at,
                    "ingested_at": at,
                    "payload": {
                        "close": close,
                        "volume": 5000,
                        "currency": "USD",
                        "price_basis": "raw",
                        "bid": "98.10",
                        "ask": "98.15",
                    },
                }
            )
        archive_bytes = json.dumps(
            {
                "schema_version": "0.1",
                "source_mode": "synthetic_fixture",
                "records": bars,
            }
        ).encode()
        (tmp_path / "market.json").write_bytes(archive_bytes)
        job["data_manifest"] = {
            "ref": "market.json",
            "sha256": hashlib.sha256(archive_bytes).hexdigest(),
        }
        worker.extend(["--archive-root", str(tmp_path)])
    with process(ctl, env) as control:
        wait_for_resource(port, "/readyz", control)
        if archive_mode:
            account_id = str(uuid4())
            account_path = f"/v1/paper/accounts/{account_id}"
            code, account = request(
                port, "/v1/paper/accounts", {"account_id": account_id, "initial_cash_usd": "1000"}
            )
            assert code == 201 and account["version"] == 1
            code, initial_events = request(port, account_path + "/events")
            assert code == 200 and [event["kind"] for event in initial_events] == [
                "paper_account.created"
            ]
            fill_id = settle_paper_round_trip(port, account_id, instrument)
            code, account = request(port, account_path)
            assert code == 200 and account["cash_usd"] == "997.99999996"
            code, expected_events = request(port, account_path + "/events")
            assert code == 200 and len(expected_events) == 5
            # The sell fill was recorded by PostgreSQL. Use that cutoff,
            # allowing a short clock difference with the local Go process.
            cutoff = expected_events[-1]["recorded_at"]
            delay = (datetime.fromisoformat(cutoff) - datetime.now(UTC)).total_seconds()
            assert delay <= 5, "paper database clock is more than five seconds ahead"
            if delay > 0:
                time.sleep(delay)
            memory_path = tmp_path / "paper-market.json"
            manifest = paper_command(
                paper_memory,
                policy_path,
                env,
                "--account",
                account_id,
                "--as-of",
                cutoff,
                "--input",
                str(tmp_path / "market.json"),
                "--input-sha256",
                job["data_manifest"]["sha256"],
                "--output",
                str(memory_path),
            )
            assert manifest == {
                "ref": memory_path.name,
                "sha256": hashlib.sha256(memory_path.read_bytes()).hexdigest(),
            }
            archived = load_snapshot(tmp_path, manifest, datetime.fromisoformat(cutoff))
            context = build_contexts(
                archived, ["TSLA"], policy["policy_version"], include_reflections=True
            )["TSLA"]
            (observation,) = json.loads(context.split("\n", 1)[1])["records"]
            reflection = observation["factual_reflection"]
            (source,) = [item for item in archived.records if item.kind == "paper_outcome"]
            assert reflection["source_fill_event_id"] == fill_id
            assert reflection["source_decision_id"] == observation["payload"]["decision_id"]
            assert reflection["source_record_sha256"] == source.sha256
            assert reflection["realized_outcome"] == "loss"
            assert reflection["pnl_before_exit_fee_usd"] == "-2.00000002"
            assert reflection["recorded_exit_fee_usd"] == "0.00000002"
            assert reflection["exit_fee_erased_positive_pre_fee_pnl"] is False
            marks_path = tmp_path / "marks.json"
            marks_path.write_text(
                json.dumps(
                    [
                        {
                            "instrument": instrument,
                            "price_usd": "98.12345678",
                            "at": observed,
                            "available_at": observed,
                        }
                    ]
                ),
                encoding="utf-8",
            )
            bound = paper_command(
                paper_apply,
                policy_path,
                env,
                "snapshot",
                "--account",
                account_id,
                "--marks",
                str(marks_path),
                "--as-of",
                cutoff,
            )
            book = bound["portfolio"]
            assert book["cash_usd"] == account["cash_usd"] and not book["positions"]
            plan = plan_research(
                tmp_path,
                {
                    "schema_version": "0.1",
                    "job_id": job_id,
                    "portfolio": book,
                    "policy_version": policy["policy_version"],
                    "data_manifest": manifest,
                    "candidates": [{"symbol": "TSLA", "allowed": True}],
                    "top_k": 1,
                    "remaining_analysis_slots": 1,
                    "calls_per_symbol": 31,
                    "max_llm_calls": 32,
                    "roc_periods": 2,
                    "max_bar_age_seconds": 60,
                    "max_bar_gap_seconds": 60,
                    "max_catalyst_age_seconds": 60,
                    "min_bar_turnover_usd": "10000",
                    "max_spread_bps": 25,
                },
            )
            job = plan["job"]
            assert job is not None and job["requested_symbols"] == ["TSLA"]
            raw_selection = json.dumps(plan).encode()
            selection_path = tmp_path / "selection.json"
            selection_path.write_bytes(raw_selection)
            config_path = tmp_path / "entry-schedule.json"
            config_path.write_text(
                json.dumps(
                    {
                        "entry_schedule_schema_version": "0.1",
                        "schedule_id": "pytest-entry",
                        "scheduled_at": cutoff,
                        "max_lateness_seconds": 120,
                        "max_job_llm_calls": 32,
                        "snapshot_context": bound["snapshot_context"],
                        "selection_sha256": hashlib.sha256(raw_selection).hexdigest(),
                    }
                ),
                encoding="utf-8",
            )
            schedule_command = [
                paper_apply, "schedule-entry",
                "--config", str(config_path),
                "--selection", str(selection_path),
                "--archive-root", str(tmp_path),
            ]
            admission = json.loads(subprocess.check_output(schedule_command, env=env, timeout=40))
            assert admission["created"] is True and admission["job_id"] != job_id
            job_id = admission["job_id"]
            path = f"/v1/research/jobs/{job_id}/result"
        else:
            assert request(port, "/v1/research/jobs", job)[0] == 201
        with process(worker, research_environment()) as running:
            receipt = wait_for_resource(port, path, control, running)

    result = receipt["result"]
    assert result["result_schema_version"] == "0.2"
    assert result["batch"]["portfolio_snapshot_id"] == book["snapshot_id"]
    assert set(result["reports"]["TSLA"]) >= {"market_report", "final_trade_decision"}
    metadata = result["metadata"]
    assert metadata["source_mode"] == "tradingagents_offline"
    assert 1 <= metadata["llm_calls"] <= job["max_llm_calls"]
    assert metadata["usd_cost"] is None
    assert metadata["alpha_measured"] is False
    assert metadata["intraday_pit_verified"] is False
    assert metadata["provenance"]["job_id"] == job_id
    assert metadata["provenance"]["attempt"] == receipt["attempt"] == 1
    assert metadata["provenance"]["data_manifest"] == job["data_manifest"]
    assert metadata["provenance"]["policy_version"] == job["policy_version"]
    pin = next(
        repo
        for repo in json.loads(
            (ROOT / "services/research/upstream.lock.json").read_text(encoding="utf-8")
        )["repositories"]
        if repo["distribution"] == "tradingagents"
    )
    assert metadata["provenance"]["upstream_sha"] == pin["source_commit"]
    if archive_mode:
        report = result["reports"]["TSLA"]["market_report"]
        assert "98.12345678" in report and "snapshot_sha256" in report
        assert "paper_simulated_ledger" not in report
        assert (
            result["batch"]["evidence_hashes"]["TSLA:market_report"]
            == hashlib.sha256(report.encode()).hexdigest()
        )
        final = result["reports"]["TSLA"]["final_trade_decision"]
        assert "paper_simulated_ledger" in final
        assert '"net_pnl_usd":"-2.00000004"' in final
        assert fill_id in final
        assert (
            result["batch"]["evidence_hashes"]["TSLA:final_trade_decision"]
            == hashlib.sha256(final.encode()).hexdigest()
        )

    with process(ctl, env) as restarted:
        assert wait_for_resource(port, path, restarted) == receipt
        if archive_mode:
            decisions = [(item["symbol"], item["action"]) for item in result["batch"]["intents"]]
            assert decisions == [("TSLA", "HOLD")]
            repeat = json.loads(subprocess.check_output(schedule_command, env=env, timeout=40))
            assert repeat["created"] is False and repeat["job_id"] == job_id
            dispatched = paper_command(paper_apply, policy_path, env, "orders", "--account", account_id)
            assert dispatched["job_id"] == job_id and dispatched["application"] == {
                "paper_apply_schema_version": "0.1",
                "job_id": job_id,
                "status": "hold",
            }
            assert request(port, account_path) == (200, account)
            assert request(port, account_path + "/events") == (200, expected_events)


def test_archived_catalyst_requeues_an_open_paper_position_for_offline_worker(tmp_path):
    control_exe = build_go_command(tmp_path, "trading-api")
    paper_apply = build_go_command(tmp_path, "paper")
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    def save(name, payload):
        path = tmp_path / name
        data = json.dumps(payload).encode()
        path.write_bytes(data)
        return {"ref": name, "sha256": hashlib.sha256(data).hexdigest()}

    account_id, opening_job_id, decision_id = (str(uuid4()) for _ in range(3))
    account_path = f"/v1/paper/accounts/{account_id}"
    policy_path = ROOT / "contracts/valid-paper-policy.json"
    policy = json.loads(policy_path.read_text(encoding="utf-8"))
    instrument = next(
        rule["instrument"] for rule in policy["instruments"] if rule["symbol"] == "TSLA"
    )
    env = offline_test_environment(DATABASE_URL)
    control = [control_exe, "-listen", f"127.0.0.1:{port}"]
    worker = [
        sys.executable,
        "-m",
        "tradingbot",
        "worker",
        "--control-url",
        f"http://127.0.0.1:{port}",
        "--archive-root",
        str(tmp_path),
    ]

    with process(control, env) as api:
        wait_for_resource(port, "/readyz", api)
        assert (
            request(
                port,
                "/v1/paper/accounts",
                {
                    "account_id": account_id,
                    "initial_cash_usd": "1000",
                },
            )[0]
            == 201
        )
        cutoff = request(port, account_path + "/events")[1][0]["recorded_at"]
        observed = (datetime.fromisoformat(cutoff) - timedelta(seconds=1)).isoformat()
        delay = (datetime.fromisoformat(cutoff) - datetime.now(UTC)).total_seconds()
        assert delay <= 5, "paper database clock is more than five seconds ahead"
        if delay > 0:
            time.sleep(delay)
        save(
            "marks.json",
            [
                {
                    "instrument": instrument,
                    "price_usd": "100",
                    "at": observed,
                    "available_at": observed,
                }
            ],
        )
        opening = paper_command(
            paper_apply,
            policy_path,
            env,
            "snapshot",
            "--account",
            account_id,
            "--marks",
            str(tmp_path / "marks.json"),
            "--as-of",
            cutoff,
        )
        portfolio = opening["portfolio"]
        code, queued = request(
            port,
            "/v1/research/jobs",
            {
                "job_schema_version": "0.1",
                "job_id": opening_job_id,
                "portfolio": portfolio,
                "requested_symbols": ["TSLA"],
                "policy_version": policy["policy_version"],
                "max_llm_calls": 32,
                "data_manifest": {"ref": "synthetic/opening", "sha256": "a" * 64},
            },
        )
        assert code == 201
        code, lease = request(
            port,
            "/v1/research/jobs/claim",
            {
                "worker_id": "opening-fixture",
                "claim_id": str(uuid4()),
            },
        )
        assert code == 200 and lease["job_id"] == opening_job_id
        evidence_id, report = "TSLA:final_trade_decision", "Synthetic opening BUY fixture."
        expiry = (datetime.fromisoformat(queued["created_at"]) + timedelta(minutes=1)).isoformat()
        code, completion = request(
            port,
            f"/v1/research/jobs/{opening_job_id}/complete",
            {
                "result_schema_version": "0.1",
                "worker_id": "opening-fixture",
                "lease_token": lease["lease_token"],
                "batch": {
                    "schema_version": "0.1",
                    "execution_mode": "research",
                    "run_id": str(uuid4()),
                    "portfolio_snapshot_id": portfolio["snapshot_id"],
                    "as_of": portfolio["as_of"],
                    "created_at": queued["created_at"],
                    "expires_at": expiry,
                    "evidence_hashes": {evidence_id: hashlib.sha256(report.encode()).hexdigest()},
                    "intents": [
                        {
                            "decision_id": decision_id,
                            "symbol": "TSLA",
                            "action": "BUY",
                            "notional_usd": "100",
                            "confidence": 0.75,
                            "expected_return_pct": 1.0,
                            "horizon": {"min_hours": 2, "max_hours": 24},
                            "thesis": ["Opening fixture"],
                            "invalidation_conditions": ["Catalyst fixture"],
                            "evidence_ids": [evidence_id],
                            "max_execution_cost_bps": 30,
                        }
                    ],
                },
                "reports": {"TSLA": {"final_trade_decision": report}},
                "metadata": {
                    "source_mode": "synthetic_fixture",
                    "llm_calls": 0,
                    "alpha_measured": False,
                },
            },
        )
        assert code == 201, completion
        quote_at = datetime.fromisoformat(completion["completed_at"]) + timedelta(microseconds=1)
        delay = (quote_at - datetime.now(UTC)).total_seconds()
        assert delay <= 5, "paper database clock is more than five seconds ahead"
        if delay > 0:
            time.sleep(delay)
        save(
            "opening-request.json",
            {
                "paper_apply_schema_version": "0.1",
                "job_id": opening_job_id,
                "snapshot_context": opening["snapshot_context"],
            },
        )
        save(
            "opening-quote.json",
            {
                "paper_quote_schema_version": "0.1",
                "source_mode": "synthetic_fixture",
                "instrument": instrument,
                "side": "BUY",
                "quantity_raw": "100",
                "gross_usd": "100",
                "fee_bps": 0,
                "fixed_fee_usd": "0",
                "observed_at": quote_at.isoformat(),
                "expires_at": expiry,
                "execution": "fill",
            },
        )
        opened = paper_command(
            paper_apply,
            policy_path,
            env,
            "apply",
            "--request",
            str(tmp_path / "opening-request.json"),
            "--quote",
            str(tmp_path / "opening-quote.json"),
        )["execution"]["order"]
        assert opened["status"] == "filled"
        event_at = request(port, account_path + "/events")[1][-1]["recorded_at"]
        review_event = {"kind": "news", "provider": "review-fixture", "record_id": "review-news"}
        known = {
            "symbol": "TSLA",
            "revision": 1,
            "event_time": event_at,
            "available_at": event_at,
            "ingested_at": event_at,
        }
        archive = save(
            "review-archive.json",
            {
                "schema_version": "0.1",
                "source_mode": "synthetic_fixture",
                "records": [
                    review_event | known | {"payload": {"headline": "Paper review catalyst."}},
                    known
                    | {
                        "kind": "bar",
                        "provider": "review-fixture",
                        "record_id": "mark",
                        "payload": {"close": "100"},
                    },
                ],
            },
        )
        save(
            "review-schedule.json",
            {
                "review_schedule_schema_version": "0.1",
                "opening_order_id": opened["order_id"],
                "snapshot_context": {
                    "account_id": account_id,
                    "policy": policy,
                    "marks": [
                        {
                            "instrument": instrument,
                            "price_usd": "100",
                            "at": event_at,
                            "available_at": event_at,
                        }
                    ],
                },
                "max_llm_calls": 32,
                "data_manifest": archive,
                "event": review_event,
            },
        )

        def schedule():
            command = subprocess.run(
                [
                    paper_apply,
                    "schedule-review",
                    "--config",
                    str(tmp_path / "review-schedule.json"),
                    "--archive-root",
                    str(tmp_path),
                ],
                env=env,
                stdout=subprocess.PIPE,
                check=True,
                timeout=40,
            )
            return json.loads(command.stdout)

        first = schedule()
        review_id = first["job_id"]
        assert first["created"] is True and review_id == first["trigger"]["trigger_id"]
        assert first["snapshot"]["portfolio"]["positions"][0]["quantity"] == "1.00000000"
        with process(worker, research_environment()) as running:
            path = f"/v1/research/jobs/{review_id}/result"
            receipt = wait_for_resource(port, path, api, running)
        assert receipt["result"]["metadata"]["provenance"]["data_manifest"] == archive
        assert receipt["result"]["batch"]["intents"][0]["action"] == "HOLD"

    with process(control, env) as restarted:
        assert wait_for_resource(port, path, restarted) == receipt
        replayed = schedule()
        assert replayed["job_id"] == review_id and replayed["created"] is False
        assert replayed["snapshot"] == first["snapshot"]
        _, account_before_hold = request(port, account_path)

        def dispatch():
            run = subprocess.run(
                [paper_apply, "orders", "--account", account_id, "--policy", str(policy_path)],
                env=env,
                stdout=subprocess.PIPE,
                check=True,
                timeout=40,
            )
            return run.stdout

        result = json.loads(dispatch())
        assert result["job_id"] == review_id
        assert result["status"] == "hold" and result["application"]["status"] == "hold"
        assert not dispatch().strip()
        assert request(port, account_path) == (200, account_before_hold)
