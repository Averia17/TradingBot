"""Seed an actual stable paper opening for the isolated review restore drill."""

import hashlib
import json
import time
from datetime import datetime, timedelta
from uuid import uuid4

from smoke import JOBS, ROOT, checked, request

SELECTION = ROOT / "services/research/examples/selection"
ENTRY = ROOT / "services/research/examples/entry-cycles"
SYMBOL = "TSLA"
SECURITY = "fixture-current"
EVENT = {"kind": "news", "provider": "selection-fixture", "record_id": "restore-catalyst"}


def _load(path):
    return json.loads(path.read_bytes())


def _encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def _pin(root, name, document):
    raw = _encode(document)
    path = root / f"review-restore-{name}.json"
    path.write_bytes(raw)
    path.chmod(0o644)
    return {"ref": path.name, "sha256": hashlib.sha256(raw).hexdigest()}


def _registry():
    registry = _load(SELECTION / "identity-registry.json")
    (entry,) = (row for row in registry["entries"] if row["security_id"] == SECURITY)
    # The sample issuer is synthetic and this drill can run at any local date.
    entry.update(available_at="2000-01-01T00:00:00Z", effective_at="2000-01-01T00:00:00Z")
    return registry | {"entries": [entry]}


def review_registry_sha256():
    """Return the deterministic synthetic registry pin before Control starts."""
    return hashlib.sha256(_encode(_registry())).hexdigest()


def _market(cutoff):
    sample = datetime.fromisoformat(
        _load(SELECTION / "identity-request.json")["portfolio"]["as_of"]
    )
    market = _load(SELECTION / "identity-market.json")
    market["records"] = [
        row for row in market["records"] if row["payload"].get("security_id") == SECURITY
    ]
    shift = cutoff - sample
    for row in market["records"]:
        for key in ("event_time", "available_at", "ingested_at"):
            row[key] = (datetime.fromisoformat(row[key]) + shift).isoformat()
        if row["kind"] == "listing":
            row["payload"]["effective_at"] = row["event_time"]
    return market


def _marks(cutoff):
    observed = (cutoff - timedelta(seconds=1)).isoformat()
    return [
        mark | {"at": observed, "available_at": observed} for mark in _load(ENTRY / "marks.json")
    ]


def _opening_tape(account_id, registry_sha, start):
    tape = _load(ENTRY / "identity-tape.json")
    quote = tape["observations"][0]["quote"] | {"identity_registry_sha256": registry_sha}
    observations = []
    # Admission and completion take a variable number of Docker roundtrips.
    # The exact firm observation must follow durable completion.
    for offset in range(2, 121, 2):
        observed = start + timedelta(seconds=offset)
        observations.append(
            {
                "firmness": "firm",
                "available_at": (observed + timedelta(milliseconds=1)).isoformat(),
                "quote": quote
                | {
                    "observed_at": observed.isoformat(),
                    "expires_at": (observed + timedelta(seconds=90)).isoformat(),
                },
            }
        )
    return tape | {
        "account_id": account_id,
        "identity_registry_sha256": registry_sha,
        "observations": observations,
    }


def _queue_opening(compose, root, account_id, policy, registry, tape, start):
    market = _pin(root, "entry-market", _market(start))
    screening = _load(ENTRY / "identity-screening.json") | {"data_manifest": market}
    plan = _pin(
        root,
        "entry-plan",
        {
            "schema_version": "0.2",
            "account_id": account_id,
            "schedule_id": "review-restore-opening",
            "policy": policy,
            "identity_registry": registry,
            "quote_tape": tape,
            "occurrences": [
                {
                    "scheduled_at": start.isoformat(),
                    "max_lateness_seconds": 60,
                    "marks": _pin(root, "entry-marks", _marks(start)),
                    "screening": _pin(root, "entry-screening", screening),
                }
            ],
        },
    )
    output = compose(
        "run",
        "--rm",
        "--no-deps",
        "-T",
        "--pull",
        "never",
        "paper-entry",
        "--root",
        "/archive",
        "--plan-ref",
        plan["ref"],
        "--plan-sha256",
        plan["sha256"],
        "--journal",
        "/tmp/entry-fixture-journal",
        "--paper-cli",
        "/usr/local/bin/paper",
        "--identity-registry-sha256",
        registry["sha256"],
        capture=True,
    )
    (result,) = json.loads(output)["occurrences"]
    checked(
        result["status"] == "queued" and result["created"],
        "stable restore opening was not admitted",
    )
    return result["job_id"]


def _complete_opening(origin, job_id, registry_sha, *, sql, clock):
    lease = request(
        origin,
        f"{JOBS}/claim",
        200,
        {
            "worker_id": "review-restore-fixture",
            "claim_id": str(uuid4()),
            "identity_registry_sha256": registry_sha,
        },
    )
    job = lease["request"]
    checked(job["job_id"] == job_id and job["requested_symbols"] == [SYMBOL], "wrong fixture claim")
    app = json.loads(sql("SELECT convert_from(request, 'UTF-8') FROM research_paper_dispatch"))
    checked(app["job_id"] == job_id, "opening admission did not bind its durable dispatch")
    worker = {key: lease[key] for key in ("worker_id", "lease_token")}
    request(origin, f"{JOBS}/{job_id}/authorize-call", 201, worker | {"call_id": str(uuid4())})
    created = clock()
    fixture = _load(ROOT / "contracts/valid-research-identity-result-request.json") | worker
    batch = fixture["batch"]
    (intent,) = (row for row in batch["intents"] if row["symbol"] == SYMBOL)
    intent.update(
        action="BUY",
        notional_usd="100",
        confidence=0.75,
        expected_return_pct=2,
        horizon={"min_hours": 1, "max_hours": 2},
        decision_id=str(uuid4()),
    )
    batch.update(
        run_id=str(uuid4()),
        portfolio_snapshot_id=job["portfolio"]["snapshot_id"],
        as_of=job["portfolio"]["as_of"],
        created_at=created.isoformat(),
        expires_at=(created + timedelta(minutes=2)).isoformat(),
        intents=[intent],
    )
    evidence = "Synthetic fixture BUY for the review restore drill"
    batch["evidence_hashes"] = {
        f"{SYMBOL}:final_trade_decision": hashlib.sha256(evidence.encode()).hexdigest(),
    }
    intent["evidence_ids"] = list(batch["evidence_hashes"])
    fixture["reports"] = {SYMBOL: {"final_trade_decision": evidence}}
    fixture["metadata"].update(llm_calls=1, missing_usage_calls=1)
    fixture["metadata"]["provenance"].update(
        job_id=job_id,
        attempt=lease["attempt"],
        policy_version=job["policy_version"],
        data_manifest=job["data_manifest"],
        identity_registry=job["identity_registry"],
        security_bindings=job["security_bindings"],
        projected_snapshot_sha256=app["identity_binding"]["projected_snapshot_sha256"],
    )
    receipt = request(origin, f"{JOBS}/{job_id}/complete", 201, fixture)
    checked(
        receipt["job_id"] == job_id and receipt["result"]["metadata"]["usd_cost"] is None,
        "offline fixture completion did not retain its identity",
    )
    return datetime.fromisoformat(receipt["completed_at"])


def _apply_opening(compose, root, account_id, job_id, policy, registry, tape):
    quotes = root / "review-restore-quote-pins"
    quotes.mkdir(mode=0o777)
    quotes.chmod(0o777)

    def control(binary, *arguments, read_only):
        output = compose(
            "run",
            "--rm",
            "--no-deps",
            "-T",
            "--pull",
            "never",
            "--entrypoint",
            f"/usr/local/bin/{binary}",
            "--volume",
            f"{root.as_posix()}:/archive:ro",
            "--volume",
            f"{quotes.as_posix()}:/quotes" + (":ro" if read_only else ""),
            "control",
            *arguments,
            capture=True,
        )
        return json.loads(output)

    quote = control(
        "paper-quote-pin",
        "--account",
        account_id,
        "--tape",
        f"/archive/{tape['ref']}",
        "--sha256",
        tape["sha256"],
        "--quote-root",
        "/quotes",
        read_only=False,
    )
    checked(
        quote["job_id"] == job_id and quote["status"] == "pinned", "firm opening quote not pinned"
    )
    applied = control(
        "paper",
        "dispatch",
        "--account",
        account_id,
        "--policy",
        f"/archive/{policy['ref']}",
        "--quote-root",
        "/quotes",
        "--archive-root",
        "/archive",
        "--identity-registry-sha256",
        registry["sha256"],
        "--identity-quote-tape-sha256",
        tape["sha256"],
        read_only=True,
    )
    checked(
        applied["job_id"] == job_id and applied["status"] == "applied", "opening was not applied"
    )
    execution = applied["application"]["execution"]
    order = execution["order"]
    checked(
        execution["security_id"] == SECURITY and order["status"] == "filled", "wrong stable fill"
    )
    return order["order_id"]


def seed_review_restore(compose, origin, root, account_id, *, sql, clock):
    """Create one durable filled opening and pin two later windows for its same catalyst."""
    account = request(
        origin,
        "/v1/paper/accounts",
        201,
        {"account_id": account_id, "initial_cash_usd": "1000"},
    )
    checked(account["account_id"] == account_id, "wrong fixture account")
    registry = _pin(root, "registry", _registry())
    policy = _pin(
        root,
        "policy",
        _load(ROOT / "contracts/valid-paper-policy.json") | {"max_mark_age_seconds": 600},
    )
    start = clock()
    tape_doc = _opening_tape(account_id, registry["sha256"], start)
    tape = _pin(root, "tape", tape_doc)
    job = _queue_opening(compose, root, account_id, policy, registry, tape, start)
    completed_at = _complete_opening(origin, job, registry["sha256"], sql=sql, clock=clock)
    first_quote = next(
        (
            datetime.fromisoformat(row["available_at"])
            for row in tape_doc["observations"]
            if datetime.fromisoformat(row["quote"]["observed_at"]) > completed_at
        ),
        None,
    )
    checked(first_quote is not None, "fixture did not complete in the firm quote tape window")
    wait = (first_quote - clock()).total_seconds()
    if wait >= 0:
        time.sleep(wait + 0.05)
    order = _apply_opening(compose, root, account_id, job, policy, registry, tape)

    first = clock()
    market = _market(first)
    cutoff = first.isoformat()
    market["records"].append(
        EVENT
        | {
            "symbol": SYMBOL,
            "revision": 1,
            "event_time": cutoff,
            "available_at": cutoff,
            "ingested_at": cutoff,
            "payload": {
                "security_id": SECURITY,
                "headline": "Synthetic held issuer restore catalyst",
            },
        }
    )
    scope = {
        "opening_order_id": order,
        "data_manifest": _pin(root, "market", market),
        "marks": _pin(root, "marks", _marks(first)),
        "max_llm_calls": 3,
        "event": EVENT,
    }
    return _pin(
        root,
        "plan",
        {
            "schema_version": "0.1",
            "account_id": account_id,
            "schedule_id": "review-restore",
            "policy": policy,
            "identity_registry": registry,
            "quote_tape": tape,
            "occurrences": [
                scope
                | {
                    "as_of": at.isoformat(),
                    "release_at": at.isoformat(),
                    "max_lateness_seconds": late,
                }
                for at, late in ((first, 45), (first + timedelta(seconds=1), 180))
            ],
        },
    ), registry["sha256"]
