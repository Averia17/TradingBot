"""Opt-in cross-process verification against a dedicated, disposable PostgreSQL database."""

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
    with closing(LoopbackConnection("127.0.0.1", port, timeout=2)) as conn:
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


def wait_for_resource(port, path, *children):
    deadline = time.monotonic() + 45
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


@pytest.mark.parametrize("archive_mode", [False, True], ids=["fixture", "archive"])
def test_python_graph_result_is_persisted_by_go_control(tmp_path, archive_mode):
    executable = tmp_path / ("control.exe" if os.name == "nt" else "control")
    subprocess.run(
        ["go", "build", "-mod=readonly", "-o", str(executable), "./cmd/control"],
        cwd=ROOT / "services/control",
        check=True,
        timeout=90,
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
    ctl = [str(executable), "-listen", f"127.0.0.1:{port}"]
    env = os.environ | {"TRADINGBOT_DATABASE_URL": DATABASE_URL}
    worker_env = {
        key: value
        for key, value in os.environ.items()
        if key
        not in {
            "TRADINGBOT_DATABASE_URL",
            "TRADINGBOT_TEST_DATABASE_URL",
            "TRADINGBOT_E2E_DATABASE_URL",
        }
    }
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
        archive_bytes = json.dumps(
            {
                "schema_version": "0.1",
                "source_mode": "synthetic_fixture",
                "records": [
                    {
                        "kind": "bar",
                        "provider": "e2e-fixture",
                        "record_id": "snapshot-mark",
                        "symbol": "TSLA",
                        "revision": 1,
                        "event_time": observed,
                        "available_at": observed,
                        "ingested_at": observed,
                        "payload": {"close": "98.12345678"},
                    }
                ],
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
        assert request(port, "/v1/research/jobs", job)[0] == 201
        with process(worker, worker_env) as running:
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
        assert result["batch"]["evidence_hashes"]["TSLA:market_report"] == hashlib.sha256(
            report.encode()
        ).hexdigest()

    with process(ctl, env) as restarted:
        assert wait_for_resource(port, path, restarted) == receipt
