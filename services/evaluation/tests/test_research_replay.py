import base64
import json
from hashlib import sha256
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.ablation_evidence import derive_ablation_evidence
from tradingbot_evaluation.research_replay import run_research_replay
from tradingbot_evaluation.statistics import evaluate_statistics, load_protocol, protocol_sha256
from tradingbot_evaluation.walk_forward import run_walk_forward
from tradingbot_evaluation.walk_forward_evidence import derive_walk_forward_evidence


def digest(payload):
    return sha256(payload).hexdigest()


def encode(value):
    return json.dumps(value).encode()


def at(second, fraction=""):
    return f"2026-10-02T13:30:{second:02}{fraction}Z"


def entry(
    number,
    action,
    symbol="TSLA",
    *,
    notional="200",
    cost_limit=100,
    cutoff=10,
    completed=12,
    expiry=None,
    positions=None,
    cash="1000",
):
    job_id = f"00000000-0000-4000-8000-{number:012}"
    snapshot_id = f"10000000-0000-4000-8000-{number:012}"
    manifest = {"ref": "market.json", "sha256": "a" * 64}
    request = {
        "job_schema_version": "0.1",
        "job_id": job_id,
        "portfolio": {
            "snapshot_id": snapshot_id,
            "as_of": at(cutoff),
            "currency": "USD",
            "cash_asset": "USD",
            "cash_usd": cash,
            "reserved_cash_usd": "0",
            "max_position_pct": "1",
            "positions": positions or [],
        },
        "requested_symbols": [symbol],
        "data_manifest": manifest,
        "policy_version": "fixture-v1",
        "max_llm_calls": 5,
    }
    field = "final_trade_decision"
    evidence_id = f"{symbol}:{field}"
    text = "Frozen local synthetic decision"
    batch = {
        "schema_version": "0.1",
        "execution_mode": "research",
        "run_id": f"20000000-0000-4000-8000-{number:012}",
        "portfolio_snapshot_id": snapshot_id,
        "as_of": at(cutoff),
        "created_at": at(completed - 1, ".000000901"),
        "expires_at": expiry or at(40),
        "evidence_hashes": {evidence_id: digest(text.encode())},
        "intents": [
            {
                "symbol": symbol,
                "action": action,
                "notional_usd": notional,
                "confidence": 0.7,
                "expected_return_pct": 4.5,
                "horizon": {"min_hours": 24, "max_hours": 72},
                "thesis": ["Fixture thesis"],
                "invalidation_conditions": ["Fixture expiration"],
                "evidence_ids": [evidence_id],
                "max_execution_cost_bps": cost_limit,
                "decision_id": f"30000000-0000-4000-8000-{number:012}",
            }
        ],
    }
    result = {
        "result_schema_version": "0.2",
        "batch": batch,
        "reports": {symbol: {field: text}},
        "metadata": {
            "source_mode": "tradingagents_offline",
            "alpha_measured": False,
            "intraday_pit_verified": False,
            "llm_calls": 2,
            "input_tokens": 0,
            "output_tokens": 0,
            "missing_usage_calls": 2,
            "usd_cost": None,
            "provenance": {
                "job_id": job_id,
                "attempt": 1,
                "policy_version": "fixture-v1",
                "data_manifest": manifest,
                "worker_version": "0.1.0",
                "upstream_version": "0.7.1",
                "upstream_sha": "b" * 40,
                "config_sha256": "c" * 64,
                "model_id": "scripted-offline-v1",
            },
        },
    }
    item = {
        "job_id": job_id,
        "attempt": 1,
        "created_at": at(cutoff),
        "completed_at": at(completed),
    }
    store(item, "request", request)
    store(item, "result", result)
    return item


def store(item, key, data):
    payload = encode(data)
    item[f"{key}_base64"] = base64.b64encode(payload).decode()
    item[f"{key}_sha256"] = digest(payload)


def load(item, key):
    return json.loads(base64.b64decode(item[f"{key}_base64"]))


def quote(observed, bid, ask, *, available=None, size=1000, symbol="TSLA"):
    return {
        "symbol": symbol,
        "observed_at": at(observed),
        "available_at": at(observed if available is None else available),
        "bid_usd": bid,
        "ask_usd": ask,
        "bid_size": size,
        "ask_size": size,
    }


def tape(quotes=None, **changes):
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "starting_cash_usd": "1000",
        "commission_bps": 10,
        "quotes": quotes
        or [
            quote(10, "99.95", "100.05"),
            quote(11, "99", "100", available=13),
            quote(12, "98", "99", available=14),
            quote(13, "99.95", "100.00", available=15),
            quote(17, "105.00", "105.05"),
        ],
        "orders": [],
    } | changes


def run(entries, replay_tape=None, **changes):
    export_raw = encode({"schema_version": "0.1", "jobs": entries})
    tape_raw = encode(tape() if replay_tape is None else replay_tape)
    hashes = {
        "expected_export_sha256": digest(export_raw),
        "expected_tape_sha256": digest(tape_raw),
    }
    return run_research_replay(export_raw, tape_raw, **(hashes | changes))


def test_uses_one_native_account_with_strictly_post_completion_quote_and_exact_cost():
    enter = entry(1, "BUY")
    exit_ = entry(
        2,
        "SELL",
        notional="210",
        cutoff=17,
        completed=19,
        positions=[
            {
                "symbol": "TSLA",
                "instrument_id": "fixture:TSLA",
                "quantity": "2",
                "mark_price_usd": "105.00",
            }
        ],
    )
    replay_tape = tape()
    replay_tape["quotes"].append(quote(20, "108.00", "108.05"))
    report = run([enter, exit_], replay_tape)
    assert report["intent_pipeline_connected"] is True
    assert report["alpha_measured"] is False
    assert report["snapshot_continuity_verified"] is False
    assert report["external_costs_usd"] is None
    assert [
        (fill["quantity"], fill["price_usd"], fill["commission_usd"]) for fill in report["fills"]
    ] == [("2", "100.00", "0.20"), ("2", "108.00", "0.22")]
    assert report["ending_cash_usd"] == "1015.58"
    assert report["open_positions"] == {}
    assert report["decisions"][0]["execution_quote_available_at"] == at(15)
    assert report["decisions"][0]["status"] == "filled"
    assert report["research_jobs"][0]["result_sha256"] == enter["result_sha256"]


def test_strict_research_replay_sizes_only_from_the_first_reopened_quote():
    replay_tape = tape(
        [
            quote(10, "100.00", "100.00"),
            quote(13, "25.00", "26.00"),
            quote(11, "10.00", "11.00", available=16),
            quote(17, "100.00", "100.00"),
        ],
        execution_calendar={
            "calendar_id": "fixture-research",
            "windows": [
                dict(
                    record_id=record_id,
                    symbol="TSLA",
                    starts_at=at(start),
                    ends_before=at(end),
                    available_at=at(start),
                    status=status,
                )
                for record_id, start, end, status in (
                    ("open", 9, 12, "open"),
                    ("halt", 12, 16, "halted"),
                    ("reopen", 16, 20, "open"),
                )
            ],
        },
    )
    report = run([entry(1, "BUY")], replay_tape, require_snapshot_continuity=True)
    assert report["snapshot_continuity_verified"] is True
    assert report["decisions"][0]["execution_quote_available_at"] == at(17)
    fill = report["fills"][0]
    assert (fill["price_usd"], fill["quantity"], fill["execution_window_id"]) == (
        "100.00",
        "2",
        "reopen",
    )


def continuous_inputs():
    quotes = tape()["quotes"]
    quotes[-1]["ask_usd"] = "105.00"
    quotes.append(quote(20, "108.00", "108.05"))
    return [
        entry(1, "BUY"),
        entry(2, "HOLD", notional="0", cutoff=14, completed=16),
        entry(
            3,
            "SELL",
            notional="210",
            cutoff=17,
            completed=19,
            cash="799.80",
            positions=[
                {
                    "symbol": "TSLA",
                    "instrument_id": "fixture:TSLA",
                    "quantity": "2.00000000",
                    "mark_price_usd": "105.00",
                }
            ],
        ),
    ], tape(quotes)


def test_strict_snapshot_continuity_reuses_state_only_after_quote_availability():
    entries, replay_tape = continuous_inputs()
    report = run(entries, replay_tape, require_snapshot_continuity=True)
    assert report["snapshot_continuity_verified"] is True
    assert [row["snapshot_continuity"]["cash_usd"] for row in report["research_jobs"]] == [
        "1000.00",
        "1000.00",
        "799.80",
    ]
    assert [row["snapshot_continuity"]["positions"] for row in report["research_jobs"]] == [
        {},
        {},
        {"TSLA": "2"},
    ]
    assert report["decisions"][1]["status"] == "hold"
    assert report["ending_cash_usd"] == "1015.58"
    assert report["open_positions"] == {}


def test_snapshot_that_changes_before_execution_cannot_pass_strict_replay():
    entries, replay_tape = continuous_inputs()
    for row in replay_tape["quotes"]:
        row.update(bid_usd="100.00", ask_usd="100.00")
    entries[1] = entry(2, "BUY", cutoff=14, completed=16)
    assert len(run(entries[:2], replay_tape)["fills"]) == 2
    with pytest.raises(ValueError, match="snapshot became stale before execution"):
        run(entries[:2], replay_tape, require_snapshot_continuity=True)


def test_strict_snapshot_counts_only_effective_actions_and_paid_dividends():
    entries, replay_tape = continuous_inputs()
    replay_tape["quotes"][-2].update(bid_usd="52.00", ask_usd="52.00")
    replay_tape["quotes"][-1].update(bid_usd="54.00", ask_usd="54.00")
    replay_tape["stock_splits"] = [
        {
            "action_id": "split",
            "symbol": "TSLA",
            "available_at": at(13),
            "effective_at": at(16),
            "new_shares": 2,
            "old_shares": 1,
        }
    ]
    replay_tape["cash_dividends"] = [
        {
            "action_id": "dividend",
            "symbol": "TSLA",
            "available_at": at(13),
            "entitlement_at": at(16),
            "paid_at": at(21),
            "cash_per_share_usd": "0.25",
        }
    ]
    held = [
        {
            "symbol": "TSLA",
            "instrument_id": "fixture:TSLA",
            "quantity": "4",
            "mark_price_usd": "52.00",
        }
    ]
    before_dividend = entry(
        3,
        "HOLD",
        notional="0",
        cutoff=17,
        completed=19,
        cash="799.80",
        positions=held,
    )
    after_dividend = entry(
        4,
        "HOLD",
        notional="0",
        cutoff=22,
        completed=24,
        cash="800.80",
        positions=[held[0] | {"mark_price_usd": "54.00"}],
    )
    report = run(
        [*entries[:2], before_dividend, after_dividend],
        replay_tape,
        require_snapshot_continuity=True,
    )
    assert report["replay_portfolio_mode"] == "verified_synthetic_cash_account"
    assert report["evaluated_through"] == at(22)
    assert report["simulated_dividend_cash_usd"] == "1.00"
    assert report["ending_cash_usd"] == "800.80"
    assert report["open_positions"] == {"TSLA": "4"}


@pytest.mark.parametrize(
    ("field", "value", "error"),
    [
        ("cash_usd", "800", "cash"),
        ("reserved_cash_usd", "1", "reserved"),
        ("quantity", "1", "positions"),
        ("instrument_id", "token:TSLA", "instrument"),
        ("mark_price_usd", "108.00", "mark"),
    ],
)
def test_strict_snapshot_continuity_rejects_unbacked_portfolio_fields(field, value, error):
    entries, replay_tape = continuous_inputs()
    request = load(entries[-1], "request")
    portfolio = request["portfolio"]
    target = portfolio if field in portfolio else portfolio["positions"][0]
    target[field] = value
    store(entries[-1], "request", request)
    with pytest.raises(ValueError, match=error):
        run(entries, replay_tape, require_snapshot_continuity=True)


def test_holds_cost_rejected_and_expired_decisions_remain_visible():
    entries = [
        entry(1, "HOLD", notional="0"),
        entry(2, "BUY", cost_limit=1),
        entry(3, "BUY", expiry=at(15)),
    ]
    report = run(entries)
    assert report["fills"] == []
    assert report["ending_cash_usd"] == "1000.00"
    assert [decision["status"] for decision in report["decisions"]] == [
        "hold",
        "rejected_execution_cost",
        "expired",
    ]


def test_reference_cutoff_identity_and_first_quote_fail_closed():
    before_first_usable_quote = tape(
        [
            quote(9, "50", "51", available=11),
            quote(13, "99.95", "100", available=15),
        ]
    )
    assert run([entry(1, "BUY")], before_first_usable_quote)["decisions"][0]["status"] == (
        "unavailable_reference_mark"
    )
    tape_with_expensive_first_quote = tape(
        [quote(10, "100", "100"), quote(13, "104", "105"), quote(14, "99.99", "100")]
    )
    assert run([entry(1, "BUY")], tape_with_expensive_first_quote)["decisions"][0]["status"] == (
        "rejected_execution_cost"
    )
    tokens = [
        {"symbol": "TSLA", "instrument_id": "sol:token", "quantity": "2", "mark_price_usd": "100"}
    ]
    report = run([entry(1, "SELL", positions=tokens)])
    assert report["decisions"][0]["status"] == "unsupported_instrument"
    assert report["fills"] == []


def test_pins_both_files_and_rejects_corruption_or_duplicate_coverage():
    buy = entry(1, "BUY")
    with pytest.raises(ValueError, match="export.*SHA-256"):
        run([buy], expected_export_sha256="0" * 64)
    with pytest.raises(ValueError, match="tape.*SHA-256"):
        run([buy], expected_tape_sha256="0" * 64)
    with pytest.raises(ValueError, match="duplicate"):
        run([buy, buy])
    with pytest.raises(ValueError, match="result.*SHA-256"):
        run([buy | {"result_sha256": "a" * 64}])
    with pytest.raises(ValueError, match="empty orders"):
        run(
            [buy],
            tape(
                orders=[
                    dict(
                        intent_id="foreign",
                        symbol="TSLA",
                        side="BUY",
                        quantity=1,
                        ready_at=at(12),
                        expires_at=at(40),
                    )
                ]
            ),
        )


def test_export_rejects_wrong_source_and_cross_document_binding():
    buy = entry(1, "BUY")
    result = load(buy, "result")
    result["metadata"]["source_mode"] = "live"
    store(buy, "result", result)
    with pytest.raises(ValueError):
        run([buy])

    second = entry(2, "BUY")
    result = load(second, "result")
    result["batch"]["portfolio_snapshot_id"] = "00000000-0000-4000-8000-000000000003"
    store(second, "result", result)
    with pytest.raises(ValueError, match="snapshot"):
        run([second])


def test_read_producer_contract_fixture_and_keep_both_intent_ids():
    root = Path(__file__).resolve().parents[3]
    exported = (root / "contracts" / "valid-research-replay-export.json").read_bytes()
    before = "2026-10-04T12:00:00Z"
    after = "2026-10-04T12:02:00Z"
    quotes = [
        quote(10, "100.00", "100") | {"observed_at": before, "available_at": before},
        quote(13, "99.99", "100") | {"observed_at": after, "available_at": after},
    ]
    frozen_tape = encode(tape(quotes))
    kwargs = {
        "expected_export_sha256": digest(exported),
        "expected_tape_sha256": digest(frozen_tape),
    }
    report = run_research_replay(exported, frozen_tape, **kwargs)
    assert report == run_research_replay(exported, frozen_tape, **kwargs)
    assert {item["symbol"]: item["status"] for item in report["decisions"]} == {
        "TSLA": "filled",
        "NVDA": "hold",
    }
    assert report["research_jobs"][0]["research_metadata"]["usd_cost"] is None
    assert report["ending_cash_usd"] == "899.90"
    assert report["ending_bid_mark_nav_usd"] == "999.89"


def test_rejects_submicrosecond_quote_before_it_can_round_into_snapshot():
    too_fine = quote(10, "99.95", "100.05")
    too_fine["available_at"] = at(10, ".000000001")
    quotes = [too_fine, quote(13, "99.95", "100")]
    with pytest.raises(ValueError, match="microsecond"):
        run([entry(1, "BUY")], tape(quotes))


def test_separate_decisions_compete_for_same_synthetic_cash():
    buys = [entry(1, "BUY", notional="800"), entry(2, "BUY", "NVDA", notional="800")]
    quotes = [quote(10, "100", "100", symbol=symbol) for symbol in ("TSLA", "NVDA")] + [
        quote(13, "100", "100", symbol=symbol) for symbol in ("TSLA", "NVDA")
    ]
    report = run(buys, tape(quotes))
    assert len(report["fills"]) == 1
    assert report["ending_cash_usd"] == "199.20"


def walk_forward_inputs():
    entries, replay_tape = continuous_inputs()
    plan = {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "experiment_id": "walk-forward",
        "policy_version": "fixture-v1",
        "max_position_pct": "0.25",
        "max_total_llm_calls": 10,
        "steps": [
            {
                "as_of": at(cutoff),
                "completed_at": at(completion),
                "requested_symbols": ["TSLA"],
                "max_llm_calls": 5,
                "data_manifest": {"ref": "market.json", "sha256": "a" * 64},
            }
            for cutoff, completion in ((10, 12), (17, 19))
        ],
    }
    return plan, replay_tape, [load(entries[i], "result") for i in (0, 2)]


def walk_inference(outputs, seen):
    def infer(payload):
        received = json.loads(payload)
        job = received["job"]
        frozen = outputs[len(seen)]
        portfolio = job["portfolio"]
        seen.append(portfolio)
        frozen["batch"].update(
            portfolio_snapshot_id=portfolio["snapshot_id"],
            as_of=portfolio["as_of"],
            created_at=received["completed_at"],
        )
        frozen["metadata"]["provenance"]["job_id"] = job["job_id"]
        return encode(frozen)

    return infer


def test_walk_forward_sends_new_portfolio_to_offline_inference():
    plan, replay_tape, outputs = walk_forward_inputs()
    seen = []
    plan_raw, tape_raw = encode(plan), encode(replay_tape)
    report = run_walk_forward(
        plan_raw,
        tape_raw,
        infer=walk_inference(outputs, seen),
        expected_plan_sha256=digest(plan_raw),
        expected_tape_sha256=digest(tape_raw),
        include_replay_input=True,
    )
    assert seen[0]["cash_usd"] == "1000.00" and seen[0]["positions"] == []
    assert seen[1]["cash_usd"] == "799.80"
    assert seen[1]["positions"] == [
        {
            "symbol": "TSLA",
            "instrument_id": "fixture:TSLA",
            "quantity": "2",
            "mark_price_usd": "105",
        }
    ]
    assert report["ending_cash_usd"] == "1015.58"
    assert report["llm_calls_used"] == 4
    assert report["snapshot_continuity_verified"] is True
    assert report["clock_source"] == "simulated_plan"
    assert len(report["native_replay_input"]["orders"]) == 2


def test_walk_forward_checks_total_budget_before_inference():
    plan, replay_tape, _ = walk_forward_inputs()
    plan["max_total_llm_calls"] = 9
    plan_raw, tape_raw = encode(plan), encode(replay_tape)

    def unexpected(_):
        pytest.fail("invalid studies must not call research")

    with pytest.raises(ValueError, match="budget"):
        run_walk_forward(
            plan_raw,
            tape_raw,
            infer=unexpected,
            expected_plan_sha256=digest(plan_raw),
            expected_tape_sha256=digest(tape_raw),
        )


def walk_protocol(plan_raw, tape_raw):
    return load_protocol(
        encode(
            {
                "schema_version": "0.2",
                "experiment_id": "walk-forward",
                "configuration_sha256": "b" * 64,
                "source_mode": "synthetic_fixture",
                "model_training_cutoff": None,
                "nav_basis": "execution_net",
                "splits": {
                    "train_start": "2026-10-01",
                    "validation_start": "2026-10-02",
                    "test_start": "2026-10-03",
                    "end_exclusive": "2026-10-05",
                    "embargo_calendar_days": 0,
                },
                "expected_nav_dates": ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"],
                "primary_comparison_id": "cash",
                "comparisons": [
                    {
                        "comparison_id": "cash",
                        "strategy": "fixture",
                        "benchmark": "cash",
                        "benchmark_access": "synthetic_fixture",
                        "minimum_daily_excess_bps": 0,
                    }
                ],
                "bootstrap": {
                    "block_marks": 2,
                    "minimum_blocks": 2,
                    "replications": 500,
                    "seed": 1,
                    "family_alpha_bps": 500,
                },
                "walk_forward_source": {
                    "plan_sha256": digest(plan_raw),
                    "tape_sha256": digest(tape_raw),
                    "mark_max_age_seconds": 86400,
                    "benchmarks": [{"series_id": "cash", "kind": "cash"}],
                },
            }
        )
    )


def test_frozen_walk_forward_regenerates_daily_evidence_without_prior_result_hash(tmp_path, capsys):
    plan, tape_doc, outputs = walk_forward_inputs()
    plan_raw, tape_raw = encode(plan), encode(tape_doc)
    protocol = walk_protocol(plan_raw, tape_raw)
    expected = protocol_sha256(protocol)
    evidence = derive_walk_forward_evidence(
        protocol,
        plan_raw,
        tape_raw,
        expected_protocol_sha256=expected,
        infer=walk_inference(outputs, []),
    )
    assert [row.nav_usd for row in evidence.daily_nav] == [
        {"fixture": "1000.00", "cash": "1000.00"},
        {"fixture": "1015.58", "cash": "1000.00"},
        {"fixture": "1015.58", "cash": "1000.00"},
        {"fixture": "1015.58", "cash": "1000.00"},
    ]
    report = evaluate_statistics(protocol, evidence, expected_protocol_sha256=expected)
    assert report["walk_forward_provenance_declared"]["llm_calls_used"] == 4
    assert report["alpha_measured"] is False
    tampered = msgspec.structs.replace(
        evidence,
        walk_forward_provenance=msgspec.structs.replace(
            evidence.walk_forward_provenance, tape_sha256="c" * 64
        ),
    )
    with pytest.raises(ValueError, match="frozen plan and tape"):
        evaluate_statistics(protocol, tampered, expected_protocol_sha256=expected)

    protocol_path = tmp_path / "protocol.json"
    evidence_path = tmp_path / "evidence.json"
    protocol_path.write_bytes(msgspec.json.encode(protocol))
    exact = msgspec.json.encode(evidence)
    evidence_path.write_bytes(exact)
    flags = [
        "statistics",
        str(protocol_path),
        str(evidence_path),
        "--protocol-sha256",
        expected,
        "--evidence-sha256",
        digest(exact),
    ]
    assert main(flags) == 0
    assert json.loads(capsys.readouterr().out)["verification_scope"]["evidence_file_hash_verified"]
    evidence_path.write_bytes(exact.replace(b"1015.58", b"9000.00"))
    assert main(flags) == 2
    assert "Evidence file SHA-256" in capsys.readouterr().err


def test_walk_forward_rejects_skipped_days_and_stale_held_nav():
    plan, tape_doc, outputs = walk_forward_inputs()
    plan["steps"] = plan["steps"][:1]
    plan_raw, tape_raw = encode(plan), encode(tape_doc)
    protocol = walk_protocol(plan_raw, tape_raw)
    for dates in (
        protocol.expected_nav_dates[:-1],
        protocol.expected_nav_dates[:2] + protocol.expected_nav_dates[-1:],
    ):
        with pytest.raises(ValueError, match="every UTC day"):
            load_protocol(
                msgspec.json.encode(msgspec.structs.replace(protocol, expected_nav_dates=dates))
            )
    with pytest.raises(ValueError, match="stale mark for TSLA on 2026-10-03"):
        derive_walk_forward_evidence(
            protocol,
            plan_raw,
            tape_raw,
            expected_protocol_sha256=protocol_sha256(protocol),
            infer=walk_inference(outputs[:1], []),
        )


def test_walk_forward_checks_the_benchmark_mark_when_strategy_holds_cash():
    plan, tape_doc, outputs = walk_forward_inputs()
    plan["steps"] = plan["steps"][:1]
    outputs[0]["batch"]["intents"][0].update(action="HOLD", notional_usd="0")
    formation = "2026-10-01T12:00:00Z"
    tape_doc["quotes"].append(
        quote(10, "99.99", "100.00") | {"observed_at": formation, "available_at": formation}
    )
    plan_raw, tape_raw = encode(plan), encode(tape_doc)
    protocol = walk_protocol(plan_raw, tape_raw)
    source = protocol.walk_forward_source
    benchmark = msgspec.structs.replace(
        source.benchmarks[0], series_id="hold", kind="buy_and_hold", symbol="TSLA"
    )
    protocol = msgspec.structs.replace(
        protocol,
        comparisons=(msgspec.structs.replace(protocol.comparisons[0], benchmark="hold"),),
        walk_forward_source=msgspec.structs.replace(source, benchmarks=(benchmark,)),
    )
    with pytest.raises(ValueError, match="stale mark for TSLA on 2026-10-03"):
        derive_walk_forward_evidence(
            protocol,
            plan_raw,
            tape_raw,
            expected_protocol_sha256=protocol_sha256(protocol),
            infer=walk_inference(outputs[:1], []),
        )


@pytest.mark.parametrize("changed", ["protocol", "plan", "tape"])
def test_walk_forward_evidence_checks_frozen_inputs_before_calling_research(changed):
    plan, tape_doc, _ = walk_forward_inputs()
    plan_raw, tape_raw = encode(plan), encode(tape_doc)
    protocol = walk_protocol(plan_raw, tape_raw)
    expected = protocol_sha256(protocol)
    if changed == "protocol":
        expected = "c" * 64
    elif changed == "plan":
        plan_raw += b"\n"
    else:
        tape_raw += b"\n"

    def unexpected(_):
        pytest.fail("a changed frozen input must not call research")

    with pytest.raises(ValueError, match="SHA-256"):
        derive_walk_forward_evidence(
            protocol, plan_raw, tape_raw, expected_protocol_sha256=expected, infer=unexpected
        )


def ablation_protocol(plan_raw, tape_raw, budget):
    doc = msgspec.to_builtins(walk_protocol(plan_raw, tape_raw))
    source = doc.pop("walk_forward_source")
    source.pop("benchmarks")
    doc["ablation_source"] = source | {
        "baseline": "full",
        "variants": {"full": "full_graph", "without-memory": "no_memory"},
        "max_total_llm_calls": budget,
    }
    doc["comparisons"][0].update(strategy="without-memory", benchmark="full")
    return load_protocol(encode(doc))


def test_ablation_uses_paired_nav_all_frozen_arms_and_aggregate_budget():
    plan, tape_doc, outputs = walk_forward_inputs()
    plan_raw, tape_raw = encode(plan), encode(tape_doc)
    protocol = ablation_protocol(plan_raw, tape_raw, 20)
    requested = []

    def infer_for(variant):
        requested.append(variant)
        records = json.loads(json.dumps(outputs))
        if variant == "no_memory":
            for result in records:
                result["batch"]["intents"][0].update(action="HOLD", notional_usd="0")
                result["metadata"].update(llm_calls=1, missing_usage_calls=1)
                result["metadata"]["provenance"]["config_sha256"] = "d" * 64
        return walk_inference(records, [])

    expected = protocol_sha256(protocol)
    evidence = derive_ablation_evidence(
        protocol,
        plan_raw,
        tape_raw,
        expected_protocol_sha256=expected,
        infer_for=infer_for,
    )
    assert requested == ["full_graph", "no_memory"]
    assert [day.nav_usd for day in evidence.daily_nav] == [
        {"full": "1000.00", "without-memory": "1000.00"},
        *[{"full": "1015.58", "without-memory": "1000.00"}] * 3,
    ]
    report = evaluate_statistics(protocol, evidence, expected_protocol_sha256=expected)
    assert report["ablation_total_llm_calls"] == 6 and not report["alpha_measured"]
    missing = msgspec.structs.replace(
        evidence,
        ablation_provenance=msgspec.structs.replace(
            evidence.ablation_provenance, arms={"full": evidence.ablation_provenance.arms["full"]}
        ),
    )
    with pytest.raises(ValueError, match="every frozen arm"):
        evaluate_statistics(protocol, missing, expected_protocol_sha256=expected)
    arms = evidence.ablation_provenance.arms
    repeated = msgspec.structs.replace(
        evidence,
        ablation_provenance=msgspec.structs.replace(
            evidence.ablation_provenance,
            arms=arms
            | {
                "without-memory": msgspec.structs.replace(
                    arms["without-memory"],
                    research_config_sha256_by_step=arms["full"].research_config_sha256_by_step,
                )
            },
        ),
    )
    assert report["ablation_provenance_declared"]["arms"]["without-memory"][
        "research_config_sha256_by_step"
    ] == ("d" * 64, "d" * 64)
    with pytest.raises(ValueError, match="distinct research configurations"):
        evaluate_statistics(protocol, repeated, expected_protocol_sha256=expected)


def test_ablation_rejects_aggregate_budget_before_creating_first_runtime():
    plan, tape_doc, _ = walk_forward_inputs()
    plan_raw, tape_raw = encode(plan), encode(tape_doc)
    protocol = ablation_protocol(plan_raw, tape_raw, 19)

    def unexpected(_):
        pytest.fail("an over-budget study must not create an inference runtime")

    with pytest.raises(ValueError, match="aggregate planned call budget"):
        derive_ablation_evidence(
            protocol,
            plan_raw,
            tape_raw,
            expected_protocol_sha256=protocol_sha256(protocol),
            infer_for=unexpected,
        )
