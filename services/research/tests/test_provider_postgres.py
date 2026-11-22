"""Opt-in paper round trip through scheduled provider research and real Control."""

import json
import os
import re
import socket
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from datetime import UTC, datetime, timedelta
from threading import Event
from urllib.parse import parse_qs, urlsplit
from uuid import uuid4

import pytest
from langchain_core.runnables import RunnableLambda
from test_entry_cycles import AT, bind_tape, identity_fixture, put
from test_provider_worker import CONFIG, PROPOSALS, MockProvider
from test_worker_postgres import (
    ROOT,
    LoopbackConnection,
    build_go_command,
    process,
    request,
    wait_for_resource,
)

from tradingbot import entry_cycles, review_cycles, worker
from tradingbot.adapters import prepared_provider
from tradingbot.contracts import IntentProposals

DATABASE_URL = os.environ.get("TRADINGBOT_PROVIDER_TEST_DATABASE_URL")
pytestmark = pytest.mark.skipif(
    not DATABASE_URL, reason="set TRADINGBOT_PROVIDER_TEST_DATABASE_URL to a disposable PostgreSQL database"
)


class ExitProvider(MockProvider):
    def with_structured_output(self, schema, **kwargs):
        assert schema is IntentProposals
        sale = PROPOSALS["intents"][0] | {
            "action": "SELL", "notional_usd": "110", "expected_return_pct": -5.0,
            "thesis": ["The issuer-specific update changes the forward thesis."],
            "evidence_ids": ["TSLA:news_report"],
        }
        return self | RunnableLambda(lambda _: IntentProposals.model_validate({"intents": [sale]}))


def setup_entry(root, account, scheduled):
    _, plan, bundle = identity_fixture(root)
    plan["account_id"] = account

    def shift(value):
        return (scheduled + (datetime.fromisoformat(value) - AT)).isoformat()

    registry_name = plan["identity_registry"]["ref"]
    registry = json.loads((root / registry_name).read_bytes())
    for item in registry["entries"]:
        for field in ("effective_at", "available_at"):
            item[field] = shift(item[field])
    plan["identity_registry"] = put(root, registry_name, registry)

    screening_name = plan["occurrences"][0]["screening"]["ref"]
    screening = json.loads((root / screening_name).read_bytes())
    archive_name = screening["data_manifest"]["ref"]
    archive = json.loads((root / archive_name).read_bytes())
    archive["source_mode"] = "local_archive"
    for row in archive["records"]:
        for field in ("event_time", "available_at", "ingested_at"):
            row[field] = shift(row[field])
        if row["kind"] == "listing":
            row["payload"]["effective_at"] = shift(row["payload"]["effective_at"])
    filing = next(row for row in archive["records"] if row["kind"] == "filing")
    archive["records"].append(filing | {
        "kind": "news", "record_id": "pre-entry-news",
        "payload": {"security_id": "fixture-current", "headline": "Fixture opening catalyst"},
    })
    screening.update(
        analysis_mode="prepared_provider", max_llm_calls=1,
        data_manifest=put(root, archive_name, archive),
    )
    marks = bundle["snapshot_context"]["marks"]
    for mark in marks:
        mark["at"] = mark["available_at"] = (scheduled - timedelta(seconds=5)).isoformat()
        if mark["instrument"]["instrument_id"] == "fixture:TSLA":
            mark["price_usd"] = "100"
    plan["occurrences"] = [{
        "scheduled_at": scheduled.isoformat(), "max_lateness_seconds": 120,
        "marks": put(root, "entry-marks.json", marks),
        "screening": put(root, screening_name, screening),
    }]

    tape_name = plan["quote_tape"]["ref"]
    tape = bind_tape(json.loads((root / tape_name).read_bytes()), account,
                     plan["identity_registry"]["sha256"])
    original = tape["observations"][0]["quote"]
    # The owner pins the simulated tape before either decision. Availability
    # must follow each durable completion, even on a slower test host.
    tape["observations"] = [
        {
            "available_at": (scheduled + timedelta(seconds=seconds, milliseconds=25)).isoformat(),
            "firmness": "firm",
            "quote": original | {
                "observed_at": (scheduled + timedelta(seconds=seconds)).isoformat(),
                "expires_at": (scheduled + timedelta(seconds=seconds + 20)).isoformat(),
                "side": side, "gross_usd": gross, "quantity_raw": "100",
            },
        }
        for seconds in range(1, 151)
        for side, gross in (("BUY", "100"), ("SELL", "115"))
    ]
    plan["quote_tape"] = put(root, tape_name, tape)
    return put(root, "provider-entry.json", plan), plan, archive, marks


def setup_review(root, owner, order_id, cutoff, archive, marks):
    event = {"kind": "news", "provider": "selection-fixture", "record_id": "held-catalyst"}
    captured = cutoff.isoformat()
    records = archive["records"] + [event | {
        "symbol": "TSLA", "revision": 1,
        "event_time": captured, "available_at": captured, "ingested_at": captured,
        "payload": {"security_id": "fixture-current", "headline": "Fixture issuer exit catalyst"},
    }]
    for mark in marks:
        mark["at"] = mark["available_at"] = captured
        if mark["instrument"]["instrument_id"] == "fixture:TSLA":
            mark["price_usd"] = "110"
    return put(root, "provider-review.json", {
        "schema_version": "0.1", "account_id": owner["account_id"], "schedule_id": "catalog-review",
        **{key: owner[key] for key in ("policy", "identity_registry", "quote_tape")},
        "occurrences": [{
            "opening_order_id": order_id, "as_of": captured, "release_at": captured,
            "max_lateness_seconds": 120, "max_llm_calls": 1, "event": event,
            "data_manifest": put(root, "review-market.json", archive | {"records": records}),
            "marks": put(root, "review-marks.json", marks),
        }],
    })


def command(executable, env, *arguments):
    output = subprocess.check_output([executable, *arguments], env=env, timeout=40)
    return [json.loads(row) for row in output.splitlines()]


def test_scheduled_provider_buy_and_catalyst_sell_apply_to_real_paper_ledger(tmp_path, monkeypatch):
    database = urlsplit(DATABASE_URL)
    if database.hostname != "127.0.0.1":
        pytest.fail("provider integration requires an opt-in local disposable PostgreSQL database")
    notifications = os.environ.get("TRADINGBOT_TEST_PROVIDER_NOTIFICATIONS") == "1"
    if notifications:
        paths = parse_qs(database.query).get("search_path", [])
        if database.port is None or len(paths) != 1 or not re.fullmatch(r"provider_paper_[a-z0-9]+", paths[0]):
            pytest.fail("provider notification integration requires a dedicated loopback provider_paper schema")
    with ThreadPoolExecutor(max_workers=3) as executor:
        control, paper, pin = executor.map(
            lambda name: build_go_command(tmp_path, name), ("control", "paper", "paper-quote-pin")
        )
    account_id = str(uuid4())
    scheduled = datetime.now(UTC) + timedelta(seconds=2)
    entry_ref, owner, archive, marks = setup_entry(tmp_path, account_id, scheduled)
    registry, tape = owner["identity_registry"], owner["quote_tape"]
    env = os.environ | {
        "TRADINGBOT_DATABASE_URL": DATABASE_URL,
        "TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256": registry["sha256"],
    }
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    quote_root = tmp_path / "quotes"
    quote_root.mkdir()
    observed = []
    stop = Event()

    def invoke(*arguments):
        result, = command(paper, env, *arguments)
        return result

    def model(config, *, callbacks, timeout):
        assert config["deep_think_llm"] == CONFIG["deep_think_llm"] and timeout > 0
        model_type = ExitProvider if observed else MockProvider
        return model_type(callbacks=callbacks, observe_request=observed.append)

    def settle(job_id, side, gross, running):
        route = f"/v1/research/jobs/{job_id}"
        saved = request(port, route)[1]["request"]
        assert saved["job_schema_version"] == "0.3"
        assert saved["analysis_mode"] == "prepared_provider" and saved["max_llm_calls"] == 1
        assert saved["security_bindings"] == [{"symbol": "TSLA", "security_id": "fixture-current"}]
        receipt = wait_for_resource(port, route + "/result", running, timeout=30)
        result = receipt["result"]
        assert receipt["attempt"] == 1 and result["result_schema_version"] == "0.4"
        assert result["batch"]["intents"][0]["action"] == side
        metadata = result["metadata"]
        assert (metadata["llm_calls"], metadata["input_tokens"], metadata["output_tokens"]) == (1, 80, 240)
        assert metadata["provenance"]["model_id"] == CONFIG["deep_think_llm"]
        assert metadata["provenance"]["data_manifest"] == saved["data_manifest"]
        end = time.monotonic() + 12
        while time.monotonic() < end:
            published, = command(pin, env, "--account", account_id, "--tape",
                                  str(tmp_path / tape["ref"]), "--sha256", tape["sha256"],
                                  "--quote-root", str(quote_root))
            assert published["job_id"] == job_id
            if published["status"] == "pinned":
                break
            assert published["status"] == "awaiting_quote"
            time.sleep(0.1)
        else:
            pytest.fail("no eligible owner-pinned synthetic quote became visible")
        assert datetime.fromisoformat(published["observed_at"]) > datetime.fromisoformat(receipt["completed_at"])
        applied, = command(paper, env, *dispatch)
        assert (applied["job_id"], applied["status"]) == (job_id, "applied")
        execution = applied["application"]["execution"]
        assert execution["security_id"] == "fixture-current"
        assert execution["identity_registry_sha256"] == registry["sha256"]
        assert execution["quote_sha256"] == published["quote"]["sha256"]
        order = execution["order"]
        assert (order["status"], order["side"], order["quantity_raw"]) == ("filled", side, "100")
        fill, = order["fills"]
        assert (fill["gross_usd"], fill["fee_usd"]) == (gross, "0")
        return order["order_id"], fill["event_id"]

    route = f"/v1/paper/accounts/{account_id}"
    dispatch = (
        "dispatch", "--account", account_id, "--policy", str(tmp_path / owner["policy"]["ref"]),
        "--quote-root", str(quote_root), "--archive-root", str(tmp_path),
        "--identity-quote-tape-sha256", tape["sha256"],
    )
    monkeypatch.setattr(worker, "HTTPConnection", LoopbackConnection)
    monkeypatch.setattr(prepared_provider, "_create_model", model)
    with process([control, "-listen", f"127.0.0.1:{port}"], env) as running:
        wait_for_resource(port, "/readyz", running)
        assert request(port, "/v1/paper/accounts", {"account_id": account_id, "initial_cash_usd": "1000"})[0] == 201
        time.sleep(max(0, (scheduled - datetime.now(UTC)).total_seconds()))
        journal = tmp_path / "entry-journal"
        entry, = entry_cycles.run_cycles(
            tmp_path, entry_ref, journal, invoke, trusted_identity_registry_sha256=registry["sha256"],
        )["occurrences"]
        assert entry["status"] == "queued" and entry["created"]
        with closing(LoopbackConnection("127.0.0.1", port, timeout=5)) as connection:
            connection.request("POST", "/v1/research/jobs/claim", json.dumps({
                "worker_id": "ordinary-identity-offline", "claim_id": str(uuid4()),
                "identity_registry_sha256": registry["sha256"],
            }), {"Content-Type": "application/json"})
            assert connection.getresponse().status == 204
        with ThreadPoolExecutor(max_workers=1) as executor:
            running_worker = executor.submit(
                worker.run_worker, f"http://127.0.0.1:{port}", "paper-provider-fixture", stop=stop,
                archive_root=tmp_path, identity_registry=registry, provider_config=CONFIG,
                poll_interval=0.05, heartbeat_interval=1,
            )
            try:
                opening_id, buy_fill = settle(entry["job_id"], "BUY", "100", running)
                opened = request(port, route)[1]
                assert (opened["version"], opened["cash_usd"], opened["reserved_cash_usd"]) == (3, "900", "0")
                assert opened["positions"][0]["quantity_raw"] == "100"
                cutoff = datetime.now(UTC)
                review_ref = setup_review(tmp_path, owner, opening_id, cutoff, archive, marks)
                review, = review_cycles.run_cycles(
                    tmp_path, review_ref, tmp_path / "review-journal", invoke,
                    trusted_identity_registry_sha256=registry["sha256"],
                )["occurrences"]
                assert review["status"] == "queued" and review["created"]
                assert review["job_id"] != entry["job_id"]
                _, sell_fill = settle(review["job_id"], "SELL", "115", running)
                final = request(port, route)[1]
                assert (final["version"], final["cash_usd"], final["reserved_cash_usd"]) == (5, "1015", "0")
                assert not final["positions"]
                assert not command(paper, env, *dispatch)
                assert request(port, route)[1] == final
            finally:
                stop.set()
            running_worker.result(timeout=10)

    buy, sell = observed
    assert not buy["portfolio"]["positions"]
    assert sell["portfolio"]["positions"][0]["instrument_id"] == "fixture:TSLA"
    assert "Fixture issuer exit catalyst" in sell["reports"]["TSLA"]["news_report"]
    if notifications:
        subprocess.run(
            ["go", "test", "-mod=readonly", "-count=1", "-v",
             "-run", "^TestPreparedProviderPaperNotifications$", "./cmd/paper"],
            cwd=ROOT / "services/control", timeout=150, check=True,
            env=env | {
                "TRADINGBOT_TEST_PROVIDER_NOTIFICATION_DSN": DATABASE_URL,
                "TRADINGBOT_TEST_PROVIDER_NOTIFICATION_ACCOUNT": account_id,
                "TRADINGBOT_TEST_PROVIDER_NOTIFICATION_FILLS": json.dumps([buy_fill, sell_fill]),
            },
        )
