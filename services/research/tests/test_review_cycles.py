import json
import sqlite3
import subprocess
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from copy import deepcopy
from datetime import datetime, timedelta
from pathlib import Path
from threading import Barrier

import pytest
from test_prepare_identity_review import AS_OF, ORDER, pin
from test_prepare_identity_review import paperwork as paperwork

from tradingbot.review_cycles import ReviewCyclePlan, run_cycles
from tradingbot.snapshots import read_pinned_json

ROOT = Path(__file__).resolve().parents[3]
AT = datetime.fromisoformat(AS_OF)
JOB = "99999999-9999-4999-8999-999999999999"


def put(path, document):
    path.write_text(json.dumps(document), encoding="utf-8")
    return pin(path)


def make_plan(paperwork):
    options, _, _ = paperwork
    window = {
        "opening_order_id": options["order"],
        "as_of": AS_OF,
        "release_at": AS_OF,
        "max_lateness_seconds": 5,
        "data_manifest": options["data_manifest"],
        "marks": pin(options["marks"]),
        "max_llm_calls": 20,
    }
    plan = {
        "schema_version": "0.1",
        "account_id": options["account"],
        "schedule_id": "holding-followups",
        "policy": pin(options["policy"]),
        "identity_registry": options["identity_registry"],
        "quote_tape": options["quote_tape"],
        "occurrences": [window],
    }
    return options["archive_root"], plan


def materialize(root, plan, invoke, *, now=AT, pin_sha=None):
    reference = put(root / "review-plan.json", plan)
    return run_cycles(
        root,
        reference,
        root / "journal",
        invoke,
        trusted_identity_registry_sha256=pin_sha or plan["identity_registry"]["sha256"],
        clock=lambda: now,
    )["occurrences"]


def read_receipts(args, paperwork):
    _, view, snapshot = paperwork
    cutoff = args[args.index("--as-of") + 1]
    if args[0] == "review":
        return deepcopy(view) | {
            "as_of": cutoff,
            "opening_order_id": args[args.index("--order") + 1],
        }
    output = deepcopy(snapshot)
    output["portfolio"]["as_of"] = cutoff
    return output


def go_ack(status="queued", *, created=False):
    receipt = {"review_schedule_schema_version": "0.2", "status": status, "created": created}
    if status not in {"not_due", "missed"}:
        receipt.update(job_id=JOB, created_at=AS_OF)
    return receipt


def test_lost_ack_retries_sqlite_durable_bytes_even_after_window_closes(paperwork):
    root, plan = make_plan(paperwork)
    calls = []
    published = []

    def invoke(*args):
        calls.append(args[0])
        if args[0] != "schedule-review":
            return read_receipts(args, paperwork)
        raw = Path(args[args.index("--config") + 1]).read_bytes()
        with closing(sqlite3.connect(root / "journal/reviews.sqlite3")) as reader:
            assert reader.execute("SELECT request FROM reviews WHERE position=0").fetchone() == (
                raw,
            )
        assert args[args.index("--max-research-age") + 1] == "5000000us"
        assert (
            args[args.index("--identity-registry-sha256") + 1]
            == plan["identity_registry"]["sha256"]
        )
        assert "--roll-forward" in args
        published.append(raw)
        if len(published) == 1:
            raise subprocess.TimeoutExpired("schedule-review", 45)
        return go_ack()

    assert materialize(root, plan, invoke)[0]["status"] == "pending_go"
    recovered = materialize(root, plan, invoke, now=AT + timedelta(seconds=20))[0]
    assert (
        recovered["status"] == "queued" and not recovered["created"] and recovered["job_id"] == JOB
    )
    assert calls == ["review", "snapshot", "schedule-review", "schedule-review"]
    assert len(published) == 2 and published[0] == published[1]
    assert materialize(root, plan, invoke, now=AT + timedelta(seconds=30))[0] == recovered
    assert len(published) == 2


def test_go_confirms_missed_when_staged_bytes_were_never_admitted(paperwork):
    root, plan = make_plan(paperwork)
    attempted = []

    def invoke(*args):
        if args[0] != "schedule-review":
            return read_receipts(args, paperwork)
        attempted.append(Path(args[args.index("--config") + 1]).read_bytes())
        if len(attempted) == 1:
            raise subprocess.CalledProcessError(1, "schedule-review", stderr="private details")
        return go_ack("missed")

    pending = materialize(root, plan, invoke)[0]
    assert (pending["status"], pending["error"]) == ("pending_go", "CalledProcessError")
    now = AT + timedelta(seconds=9)
    missed = materialize(root, plan, invoke, now=now)[0]
    assert missed["status"] == "missed" and not missed["created"] and "job_id" not in missed
    assert materialize(root, plan, invoke, now=now)[0] == missed
    assert len(attempted) == 2 and attempted[0] == attempted[1]


def test_concurrent_runners_send_the_single_durable_request(paperwork):
    root, plan = make_plan(paperwork)
    reference = put(root / "review-plan.json", plan)
    preparations = Barrier(2)
    handoffs = Barrier(2)

    def run():
        def invoke(*args):
            if args[0] == "snapshot":
                preparations.wait(timeout=5)
            if args[0] != "schedule-review":
                return read_receipts(args, paperwork)
            raw = Path(args[args.index("--config") + 1]).read_bytes()
            with closing(sqlite3.connect(root / "journal/reviews.sqlite3")) as reader:
                assert reader.execute(
                    "SELECT request FROM reviews WHERE position=0"
                ).fetchall() == [(raw,)]
            handoffs.wait(timeout=5)
            return go_ack()

        return run_cycles(
            root,
            reference,
            root / "journal",
            invoke,
            trusted_identity_registry_sha256=plan["identity_registry"]["sha256"],
            clock=lambda: AT,
        )

    with ThreadPoolExecutor(max_workers=2) as pool:
        first, second = list(pool.map(lambda _: run(), range(2)))
    assert first == second
    assert first["occurrences"][0]["job_id"] == JOB


def test_not_due_is_terminal_and_later_window_uses_a_new_frozen_cutoff(paperwork):
    root, plan = make_plan(paperwork)
    next_at = AT + timedelta(seconds=1)
    release = AT + timedelta(seconds=3)
    catalyst = {"kind": "report", "provider": "selection-fixture", "record_id": "filing-1"}
    plan["occurrences"].append(
        plan["occurrences"][0]
        | {
            "as_of": next_at.isoformat(),
            "release_at": release.isoformat(),
            "event": catalyst,
        }
    )
    called = []

    def invoke(*args):
        if args[0] != "schedule-review":
            return read_receipts(args, paperwork)
        called.append(json.loads(Path(args[args.index("--config") + 1]).read_bytes()))
        if len(called) == 2:
            assert args[args.index("--max-research-age") + 1] == "7000000us"
            return go_ack("already_scheduled")
        return go_ack("not_due")

    assert [row["status"] for row in materialize(root, plan, invoke)] == ["not_due", "not_released"]
    assert [row["status"] for row in materialize(root, plan, invoke, now=release)] == [
        "not_due",
        "already_scheduled",
    ]
    assert len(called) == 2 and "event" not in called[0] and called[1]["event"] == catalyst
    assert datetime.fromisoformat(called[1]["portfolio"]["as_of"]) == next_at
    assert (
        called[0]["identity_binding"]["projected_snapshot_sha256"]
        != (called[1]["identity_binding"]["projected_snapshot_sha256"])
    )


def test_expired_and_closed_windows_do_not_submit_new_jobs(paperwork):
    root, plan = make_plan(paperwork)
    repeated = plan["occurrences"][0]
    cutoff = AT + timedelta(seconds=8)
    next_order = "22222222-2222-4222-8222-222222222223"
    plan["occurrences"].append(
        repeated
        | {
            "opening_order_id": next_order,
            "as_of": cutoff.isoformat(),
            "release_at": cutoff.isoformat(),
        }
    )

    def invoke(*args):
        assert args[0] == "review"
        result = read_receipts(args, paperwork)
        assert result["opening_order_id"] == next_order
        return result | {"state": "closed"}

    assert [row["status"] for row in materialize(root, plan, invoke, now=cutoff)] == [
        "missed",
        "not_open",
    ]


def test_slow_preparation_does_not_submit_after_the_deadline(paperwork):
    root, plan = make_plan(paperwork)
    reference = put(root / "review-plan.json", plan)
    moments = iter([AT, AT + timedelta(seconds=6)])

    def invoke(*args):
        assert args[0] != "schedule-review"
        return read_receipts(args, paperwork)

    report = run_cycles(
        root,
        reference,
        root / "journal",
        invoke,
        trusted_identity_registry_sha256=plan["identity_registry"]["sha256"],
        clock=lambda: next(moments),
    )
    assert report["occurrences"][0]["status"] == "missed"


def test_window_refusal_does_not_block_later_approved_windows(paperwork):
    root, plan = make_plan(paperwork)
    cutoff = AT + timedelta(seconds=1)
    first = plan["occurrences"][0]
    plan["occurrences"] = [
        first | {"marks": dict(first["marks"], sha256="a" * 64)},
        first | {"as_of": cutoff.isoformat(), "release_at": cutoff.isoformat()},
    ]
    commands = []

    def invoke(*args):
        commands.append(args[0])
        if args[0] == "schedule-review":
            return go_ack("not_due")
        return read_receipts(args, paperwork)

    rows = materialize(root, plan, invoke, now=cutoff)
    assert [row["status"] for row in rows] == ["refused", "not_due"]
    assert rows[0]["error"] == "ValueError"
    assert materialize(root, plan, invoke, now=cutoff) == rows
    assert commands == ["review", "snapshot", "schedule-review"]


def test_unproven_go_ack_stays_pending_while_later_windows_progress(paperwork):
    root, plan = make_plan(paperwork)
    cutoff = AT + timedelta(seconds=1)
    plan["occurrences"].append(
        plan["occurrences"][0] | {"as_of": cutoff.isoformat(), "release_at": cutoff.isoformat()}
    )

    def invoke(*args):
        if args[0] != "schedule-review":
            return read_receipts(args, paperwork)
        request = json.loads(Path(args[args.index("--config") + 1]).read_bytes())
        if datetime.fromisoformat(request["portfolio"]["as_of"]) == AT:
            return go_ack("missed") | {"job_id": JOB}
        return go_ack("not_due")

    rows = materialize(root, plan, invoke, now=cutoff)
    assert [row["status"] for row in rows] == ["pending_go", "not_due"]
    assert rows[0]["error"] == "ValidationError"


def test_owner_plan_and_journal_checks_fail_closed(paperwork):
    root, plan = make_plan(paperwork)

    def invoke(*args):
        if args[0] == "schedule-review":
            raise subprocess.TimeoutExpired("schedule-review", 45)
        return read_receipts(args, paperwork)

    materialize(root, plan, invoke)

    def forbidden(*_):
        pytest.fail("an invalid plan or journal cannot invoke Go")

    with pytest.raises(ValueError, match="owner registry SHA-256"):
        materialize(root, plan, forbidden, pin_sha="a" * 64)
    with pytest.raises(ValueError, match="different immutable owner plan"):
        materialize(root, plan | {"schedule_id": "rotated"}, forbidden)
    with pytest.raises(ValueError, match="sorted and unique"):
        materialize(root, plan | {"occurrences": plan["occurrences"] * 2}, forbidden)
    with closing(sqlite3.connect(root / "journal/reviews.sqlite3")) as writer:
        with writer:
            writer.execute("UPDATE reviews SET request=? WHERE position=0", (b"{}",))
    with pytest.raises(ValueError, match="checksum"):
        materialize(root, plan, forbidden)


def test_published_plan_schema_and_example_pins_are_valid():
    schema = ROOT / "contracts/paper-review-cycle-plan.schema.json"
    assert json.loads(schema.read_bytes()) == ReviewCyclePlan.model_json_schema()
    sample = ROOT / "contracts/valid-paper-identity-review-cycle-plan.json"
    plan = ReviewCyclePlan.model_validate_json(sample.read_bytes())
    for reference in (plan.policy, plan.identity_registry, plan.quote_tape):
        read_pinned_json(ROOT, reference)
    for window in plan.occurrences:
        read_pinned_json(ROOT, window.marks)
        read_pinned_json(ROOT, window.data_manifest)
    assert plan.occurrences[0].opening_order_id == ORDER
