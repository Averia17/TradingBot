"""Verify the isolated Compose project and print its local image manifest."""

import argparse
import hashlib
import json
import os
import socket
import subprocess
import tempfile
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener
from uuid import uuid4

ROOT = Path(__file__).resolve().parents[2]
HTTP = build_opener(ProxyHandler({}))
JOBS = "/v1/research/jobs"


def checked(condition, message):
    if not condition:
        raise RuntimeError(message)


def request(origin, path, expected, body=None):
    payload = json.dumps(body).encode() if body is not None else None
    req = Request(origin + path, payload, {"Content-Type": "application/json"})
    try:
        with HTTP.open(req, timeout=3) as response:
            status, raw = response.status, response.read()
    except HTTPError as exc:
        with exc:
            status, raw = exc.code, exc.read()
    statuses = expected if isinstance(expected, tuple) else (expected,)
    checked(status in statuses, f"{path} returned HTTP {status}, expected {expected}")
    return json.loads(raw) if raw else None


def enqueue(origin, label, archive_root=None):
    portfolio = json.loads((ROOT / "contracts/valid-portfolio.json").read_text(encoding="utf-8"))
    portfolio.update(
        snapshot_id=str(uuid4()), as_of=(datetime.now(UTC) - timedelta(seconds=5)).isoformat()
    )
    manifest = {"ref": f"offline/smoke/{label}", "sha256": "a" * 64}
    if archive_root is not None:
        observed = portfolio["as_of"]
        archive = {
            "schema_version": "0.1",
            "source_mode": "synthetic_fixture",
            "records": [
                {
                    "kind": "bar",
                    "provider": "compose-paper",
                    "record_id": "pinned-mark",
                    "symbol": "TSLA",
                    "revision": 1,
                    "event_time": observed,
                    "available_at": observed,
                    "ingested_at": observed,
                    "payload": {"close": "102.12345678"},
                }
            ],
        }
        raw = json.dumps(archive).encode()
        path = archive_root / "paper-smoke.json"
        path.write_bytes(raw)
        path.chmod(0o644)
        manifest = {"ref": path.name, "sha256": hashlib.sha256(raw).hexdigest()}
    job = {
        "job_schema_version": "0.1",
        "job_id": str(uuid4()),
        "portfolio": portfolio,
        "requested_symbols": ["TSLA"],
        "max_llm_calls": 20,
        "data_manifest": manifest,
        "policy_version": "packaging-smoke-v1",
    }
    request(origin, JOBS, 201, job)
    return job


def completed_result(origin, job):
    path = f"{JOBS}/{job['job_id']}/result"
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        try:
            with HTTP.open(origin + path, timeout=3) as response:
                receipt = json.load(response)
                break
        except HTTPError as exc:
            exc.close()
            if exc.code != 404:
                raise
        except (TimeoutError, URLError):
            pass
        time.sleep(0.25)
    else:
        raise RuntimeError(f"timed out waiting for research job {job['job_id']}")
    result = receipt["result"]
    checked(result["result_schema_version"] == "0.2", "unexpected result version")
    checked(
        result["batch"]["portfolio_snapshot_id"] == job["portfolio"]["snapshot_id"],
        "snapshot mismatch",
    )
    checked(
        result["metadata"]["source_mode"] == "tradingagents_offline", "unexpected execution source"
    )
    checked(0 < result["metadata"]["llm_calls"] <= job["max_llm_calls"], "invalid model call count")
    checked(result["metadata"]["usd_cost"] is None, "unexpected billing evidence")
    return receipt


def check_research_replay(compose, root, job, receipt):
    exported = compose(
        "exec", "-T", "control", "research-replay-export", "--job", job["job_id"], capture=True
    ).encode("utf-8")
    as_of = datetime.fromisoformat(job["portfolio"]["as_of"])
    completed = datetime.fromisoformat(receipt["completed_at"])
    tape = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "starting_cash_usd": job["portfolio"]["cash_usd"],
        "commission_bps": 10,
        "quotes": [
            {
                "symbol": "TSLA",
                "observed_at": timestamp.isoformat(),
                "available_at": timestamp.isoformat(),
                "bid_usd": "102.10",
                "ask_usd": "102.12",
                "bid_size": 100,
                "ask_size": 100,
            }
            for timestamp in (as_of - timedelta(seconds=1), completed + timedelta(seconds=1))
        ],
        "orders": [],
    }
    tape_raw = json.dumps(tape).encode()
    for name, data in (("export.json", exported), ("tape.json", tape_raw)):
        path = root / name
        path.write_bytes(data)
        path.chmod(0o644)
    report = json.loads(
        compose(
            "--profile",
            "evaluation",
            "run",
            "--rm",
            "--pull",
            "never",
            "--volume",
            f"{root}:/study:ro",
            "evaluation",
            "research-replay",
            "/study/export.json",
            "/study/tape.json",
            "--export-sha256",
            hashlib.sha256(exported).hexdigest(),
            "--tape-sha256",
            hashlib.sha256(tape_raw).hexdigest(),
            "--require-snapshot-continuity",
            capture=True,
        )
    )
    checked(report["alpha_measured"] is False, "synthetic replay claimed measured alpha")
    checked(not report["fills"], "offline HOLD produced an execution")
    checked(
        report["intent_pipeline_connected"]
        and report["snapshot_continuity_verified"] is True
        and [entry["job_id"] for entry in report["research_jobs"]] == [job["job_id"]]
        and [(entry["decision_id"], entry["status"]) for entry in report["decisions"]]
        == [(entry["decision_id"], "hold") for entry in receipt["result"]["batch"]["intents"]],
        "durable research job and decisions did not reach the evaluation report",
    )


def check_research_call_usage(compose, root, job, receipt):
    exported = compose(
        "exec", "-T", "control", "research-usage-export", "--job", job["job_id"], capture=True
    ).encode("utf-8")
    permits = json.loads(exported)
    expected = receipt["result"]["metadata"]["llm_calls"]
    checked(
        permits["job_id"] == job["job_id"]
        and permits["job_status"] == "completed"
        and permits["permit_history_known"]
        and not permits["legacy_budget_unknown"]
        and len(permits["permits"]) == expected
        and all(
            row["callback_payload"] and row["attempt"] == receipt["attempt"]
            for row in permits["permits"]
        ),
        "packaged worker did not preserve all admitted call observations",
    )
    rates = (ROOT / "services/evaluation/examples/research-rate-schedule.json").read_bytes()
    for name, raw in (("permits.json", exported), ("rates.json", rates)):
        path = root / name
        path.write_bytes(raw)
        path.chmod(0o644)
    valuation = json.loads(
        compose(
            "--profile", "evaluation", "run", "--rm", "--pull", "never",
            "--volume", f"{root}:/study:ro", "evaluation", "research-costs",
            "--rates", "/study/rates.json", hashlib.sha256(rates).hexdigest(),
            "--permit-archive", "/study/permits.json", hashlib.sha256(exported).hexdigest(),
            capture=True,
        )
    )
    observed = valuation["archives"][0]
    checked(
        observed["capture_status"] == "complete"
        and observed["permit_provenance"]["permits_without_callback"] == 0
        and observed["permit_provenance"]["permit_count"] == expected
        and valuation["estimated_cost_usd"] is None
        and valuation["actual_billed_cost_usd"] is None,
        "evaluation inferred a cost or lost metered calls",
    )
    return expected


def reject_incompatible_result(origin):
    job = enqueue(origin, "incompatible")
    lease = request(
        origin, JOBS + "/claim", 200, {"worker_id": "incompatible", "claim_id": str(uuid4())}
    )
    checked(lease["job_id"] == job["job_id"], "wrong job claimed")
    fixture = json.loads(
        (ROOT / "contracts/valid-offline-result-request.json").read_text(encoding="utf-8")
    )
    fixture.update(
        worker_id="incompatible", lease_token=lease["lease_token"], result_schema_version="99.0"
    )
    job_path = f"{JOBS}/{job['job_id']}"
    request(origin, job_path + "/complete", 422, fixture)
    request(origin, job_path + "/result", 404)


def check_paper_order(origin):
    account_id = str(uuid4())
    account_path = "/v1/paper/accounts/" + account_id
    account = request(
        origin, "/v1/paper/accounts", 201, {"account_id": account_id, "initial_cash_usd": "1000"}
    )
    reservation = {
        "account_id": account_id,
        "order_id": str(uuid4()),
        "decision_id": str(uuid4()),
        "quote_id": str(uuid4()),
        "snapshot_version": account["version"],
        "instrument": {
            "instrument_id": "fixture:TSLAx",
            "registry_version": "fixture-v1",
            "decimals": 2,
        },
        "side": "BUY",
        "quantity_raw": "125",
        "gross_usd": "20.42",
        "max_fee_usd": "0.58",
        "expires_at": (datetime.now(UTC) + timedelta(minutes=2)).isoformat(),
    }
    order = request(origin, account_path + "/orders", 201, reservation)
    resolve_path = account_path + "/orders/" + reservation["order_id"] + "/resolve"
    event = {
        "account_id": account_id,
        "order_id": reservation["order_id"],
        "event_id": str(uuid4()),
    }
    request(origin, resolve_path, 200, event | {"outcome": "unknown"})
    checked(
        request(origin, account_path, 200)["reserved_cash_usd"] == "21", "unknown lost its reserve"
    )
    fill = event | {
        "event_id": str(uuid4()),
        "outcome": "filled",
        "gross_usd": "20.42",
        "fee_usd": "0.08",
        "observed_at": (
            datetime.fromisoformat(order["created_at"]) + timedelta(microseconds=1)
        ).isoformat(),
    }
    settled = request(origin, resolve_path, 200, fill)
    checked(request(origin, resolve_path, 200, fill) == settled, "paper replay changed the result")
    account = request(origin, account_path, 200)
    checked(account["cash_usd"] == "979.5", "imprecise paper cash")
    checked(account["reserved_cash_usd"] == "0", "resolved order retained reserve")
    checked(account["positions"][0]["quantity_raw"] == "125", "wrong paper position")
    return account_path, account


def free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def docker(*args, env=None, capture=False, **kwargs):
    return subprocess.run(
        ["docker", *args],
        cwd=ROOT,
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE if capture else None,
        **kwargs,
    ).stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--skip-build", action="store_true", help="use the configured local image tags"
    )
    args = parser.parse_args()
    project = f"tradingbot-smoke-{uuid4().hex[:10]}"
    env = os.environ | {"TRADINGBOT_CONTROL_PORT": str(free_port())}
    images = {
        name: env.setdefault(
            f"TRADINGBOT_{name.upper()}_IMAGE",
            f"tradingbot-{name}:local" if args.skip_build else f"tradingbot-{name}:{project}",
        )
        for name in ("control", "research", "evaluation")
    }
    original = images["research"]
    candidate = f"tradingbot-research:{project}-candidate"
    origin = "http://127.0.0.1:" + env["TRADINGBOT_CONTROL_PORT"]

    def compose(*args, **kwargs):
        return docker(
            "compose", "-p", project, "-f", "deploy/research/compose.yaml", *args, env=env, **kwargs
        )

    def image_id(tag):
        return docker("image", "inspect", "--format", "{{.Id}}", tag, capture=True).strip()

    try:
        if not args.skip_build:
            compose("--profile", "evaluation", "build")
        compose("--profile", "paper", "config", "--quiet")
        for binary, arguments in (
            ("paper-apply", ("dispatch", "--help")),
            ("paper-apply", ("schedule-entry", "--help")),
            ("paper-check", ("--help",)),
            ("paper-nav", ("--help",)),
            ("paper-quote-pin", ("--help",)),
            ("paper-quote-study", ("--help",)),
            ("paper-thesis-outcome", ("--help",)),
            ("paper-shadow-outcome", ("--help",)),
            ("research-replay-export", ("--help",)),
            ("research-schedule-coverage", ("--help",)),
            ("research-usage-export", ("--help",)),
            ("research-budget", ("--help",)),
        ):
            docker(
                "run",
                "--rm",
                "--network",
                "none",
                "--entrypoint",
                f"/usr/local/bin/{binary}",
                images["control"],
                *arguments,
            )
        compose("up", "-d", "--wait", "--wait-timeout", "60", "--no-build", "postgres", "control")
        capabilities = request(origin, "/v1/capabilities", 200)
        checked(capabilities["research_job_call_budget"], "missing durable budget capability")
        checked(capabilities["research_call_usage"], "missing durable model usage capability")
        checked(
            capabilities["paper_orders"] and not capabilities["execution"],
            "unexpected execution capabilities",
        )
        paper_path, paper_account = check_paper_order(origin)
        paper_health = json.loads(
            compose(
                "exec", "-T", "control", "paper-check", "--account",
                paper_account["account_id"], "--pipeline", capture=True,
            )
        )
        checked(
            paper_health["status"] == "ok"
            and paper_health["pipeline"]["fill_relay"]["pending_fills"] == 1,
            "paper pipeline check did not report its pending fill",
        )
        daily_budget = json.loads(
            compose("exec", "-T", "control", "research-budget", "show", capture=True)
        )
        checked(
            daily_budget["status"] == "unconfigured" and daily_budget["recorded_permits"] == 0,
            "fresh PostgreSQL reported an unexpected daily budget",
        )
        reject_incompatible_result(origin)
        control_id = compose("ps", "-q", "control", capture=True).strip()
        database_id = compose("ps", "-q", "postgres", capture=True).strip()
        initial_job = enqueue(origin, "before")
        compose("up", "-d", "--no-build", "--no-deps", "research")
        initial_result = completed_result(origin, initial_job)

        # Exercise artifact replacement with a different image identity and the same protocol.
        docker(
            "build",
            "--pull=false",
            "-t",
            candidate,
            "-",
            input=f"FROM {original}\nLABEL org.tradingbot.smoke-candidate={project}\n",
        )
        checked(
            image_id(original) != image_id(candidate), "candidate image identity did not change"
        )
        evaluation_id = image_id(images["evaluation"])
        compose("stop", "research")
        queued = enqueue(origin, "replacement")
        env["TRADINGBOT_RESEARCH_IMAGE"] = candidate
        compose("up", "-d", "--no-build", "--no-deps", "--force-recreate", "research")
        candidate_result = completed_result(origin, queued)
        compose("stop", "research")
        queued_again = enqueue(origin, "rollback")
        env["TRADINGBOT_RESEARCH_IMAGE"] = original
        compose("up", "-d", "--no-build", "--no-deps", "--force-recreate", "research")
        completed_result(origin, queued_again)
        checked(
            compose("ps", "-q", "control", capture=True).strip() == control_id,
            "Control was replaced",
        )
        checked(
            compose("ps", "-q", "postgres", capture=True).strip() == database_id,
            "PostgreSQL was replaced",
        )
        checked(image_id(images["evaluation"]) == evaluation_id, "evaluation artifact changed")
        checked(
            completed_result(origin, initial_job) == initial_result,
            "original result changed after rollback",
        )
        checked(
            completed_result(origin, queued) == candidate_result,
            "candidate result changed after rollback",
        )
        checked(
            request(origin, paper_path, 200) == paper_account,
            "paper account changed after worker rollback",
        )
        compose("stop", "research")
        with tempfile.TemporaryDirectory(prefix="tradingbot-paper-archive-") as scratch:
            archive_root = Path(scratch)
            archive_root.chmod(0o755)
            env["TRADINGBOT_PAPER_ARCHIVE_DIR"] = str(archive_root)
            archive_job = enqueue(origin, "paper-profile", archive_root)
            try:
                compose("--profile", "paper", "up", "-d", "--no-build", "paper-research")
                archive_receipt = completed_result(origin, archive_job)
                archive_result = archive_receipt["result"]
                checked(
                    archive_result["metadata"]["provenance"]["data_manifest"]
                    == archive_job["data_manifest"]
                    and "102.12345678" in archive_result["reports"]["TSLA"]["market_report"],
                    "paper archive input did not reach the isolated worker",
                )
                check_research_replay(compose, archive_root, archive_job, archive_receipt)
                recorded_calls = check_research_call_usage(
                    compose, archive_root, archive_job, archive_receipt
                )
            finally:
                compose("--profile", "paper", "stop", "paper-research")
        compose("--profile", "evaluation", "run", "--rm", "--pull", "never", "evaluation")
        evaluation = json.loads(
            compose(
                "--profile",
                "evaluation",
                "run",
                "--rm",
                "--pull",
                "never",
                "evaluation",
                "doctor",
                capture=True,
            )
        )

        provenance = json.loads(
            docker(
                "run",
                "--rm",
                "--network",
                "none",
                "--entrypoint",
                "cat",
                original,
                "/opt/tradingbot/release-manifest.json",
                capture=True,
            )
        )
        observed = initial_result["result"]["metadata"]["provenance"]
        checked(provenance["upstream_sha"] == observed["upstream_sha"], "upstream commit mismatch")
        checked(
            provenance["worker_version"] == observed["worker_version"], "worker version mismatch"
        )
        checked(
            provenance["research_result_schema_version"]
            in capabilities["research_result_schema_versions"],
            "image result protocol is unsupported",
        )
        print(
            json.dumps(
                {
                    "images": {name: image_id(tag) for name, tag in images.items()},
                    "candidate_research_image": image_id(candidate),
                    "research": provenance,
                    "evaluation": evaluation,
                    "control_capabilities": capabilities,
                    "paper_profile_archive_job_completed": archive_job["job_id"],
                    "paper_profile_call_observations": recorded_calls,
                },
                indent=2,
            )
        )
    finally:
        compose("--profile", "evaluation", "--profile", "paper", "down", "--volumes")


if __name__ == "__main__":
    main()
