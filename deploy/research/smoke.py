"""Verify the isolated Compose project and print its local image manifest."""

import argparse
import json
import os
import socket
import subprocess
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
    checked(status == expected, f"{path} returned HTTP {status}, expected {expected}")
    return json.loads(raw) if raw else None


def enqueue(origin, label):
    portfolio = json.loads((ROOT / "contracts/valid-portfolio.json").read_text(encoding="utf-8"))
    portfolio.update(
        snapshot_id=str(uuid4()), as_of=(datetime.now(UTC) - timedelta(seconds=5)).isoformat()
    )
    job = {
        "job_schema_version": "0.1",
        "job_id": str(uuid4()),
        "portfolio": portfolio,
        "requested_symbols": ["TSLA"],
        "max_llm_calls": 20,
        "data_manifest": {"ref": f"offline/smoke/{label}", "sha256": "a" * 64},
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
        compose("up", "-d", "--wait", "--wait-timeout", "60", "--no-build", "postgres", "control")
        capabilities = request(origin, "/v1/capabilities", 200)
        checked(capabilities["research_job_call_budget"], "missing durable budget capability")
        checked(
            capabilities["paper_orders"] and not capabilities["execution"],
            "unexpected execution capabilities",
        )
        paper_path, paper_account = check_paper_order(origin)
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
                },
                indent=2,
            )
        )
    finally:
        compose("--profile", "evaluation", "down", "--volumes")


if __name__ == "__main__":
    main()
