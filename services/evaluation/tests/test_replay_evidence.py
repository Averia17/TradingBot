import json
from datetime import UTC, datetime, time
from hashlib import sha256
from pathlib import Path

import msgspec
import pytest

from tradingbot_evaluation.__main__ import main
from tradingbot_evaluation.benchmarks import benchmark_nav_series, run_benchmarks
from tradingbot_evaluation.equal_weight import EqualWeightSchedule
from tradingbot_evaluation.replay import (
    load_replay,
    replay_nav_series,
    replay_prefix,
    run_replay,
    run_replay_with_account_checkpoints,
)
from tradingbot_evaluation.replay_evidence import derive_replay_evidence
from tradingbot_evaluation.research_replay import replay_account_at
from tradingbot_evaluation.statistics import (
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)
from tradingbot_evaluation.trend import TrendConfig


def at(day, hour=12):
    return f"2026-10-{day:02d}T{hour:02d}:00:00Z"


def quote(symbol, day, bid, ask, *, available=None, hour=12):
    return {
        "symbol": symbol,
        "observed_at": at(day, hour),
        "available_at": available or at(day, hour),
        "bid_usd": bid,
        "ask_usd": ask,
        "bid_size": 100,
        "ask_size": 100,
    }


def replay_document():
    return {
        "schema_version": "0.1",
        "source_mode": "synthetic_fixture",
        "starting_cash_usd": "1000.00",
        "commission_bps": 100,
        "quotes": [
            quote("TSLA", 1, "99.00", "100.00"),
            quote("NVDA", 1, "49.00", "50.00"),
            quote("TSLA", 2, "95.00", "100.00"),
            quote("NVDA", 2, "49.00", "50.00"),
            quote("TSLA", 3, "899.00", "900.00", available=at(4, 10)),
            quote("TSLA", 4, "110.00", "111.00"),
        ],
        "orders": [
            {
                "intent_id": "enter",
                "symbol": "TSLA",
                "side": "BUY",
                "quantity": 2,
                "ready_at": at(2, 11),
                "expires_at": at(2, 15),
            },
            {
                "intent_id": "exit",
                "symbol": "TSLA",
                "side": "SELL",
                "quantity": 2,
                "ready_at": at(4, 11),
                "expires_at": at(4, 15),
            },
        ],
        "cash_dividends": [
            {
                "action_id": "dividend",
                "symbol": "TSLA",
                "available_at": at(2, 15),
                "entitlement_at": at(3),
                "paid_at": at(3, 14),
                "cash_per_share_usd": "1.00",
            }
        ],
    }


def manifest_document(replay_sha):
    mappings = [
        {"series_id": "usd-cash", "kind": "cash"},
        {"series_id": "stock-hold", "kind": "buy_and_hold", "symbol": "TSLA"},
        {"series_id": "universe", "kind": "equal_weight_once"},
    ]
    return {
        "schema_version": "0.1",
        "experiment_id": "local-quote-evaluation",
        "source_mode": "synthetic_fixture",
        "configuration_sha256": "d" * 64,
        "model_training_cutoff": None,
        "nav_basis": "execution_net",
        "splits": {
            "train_start": "2026-10-01",
            "validation_start": "2026-10-02",
            "test_start": "2026-10-03",
            "end_exclusive": "2026-10-05",
            "embargo_calendar_days": 0,
        },
        "expected_nav_dates": [f"2026-10-{day:02d}" for day in range(1, 5)],
        "primary_comparison_id": "stock-hold",
        "comparisons": [
            {
                "comparison_id": mapping["series_id"],
                "strategy": "fixture-strategy",
                "benchmark": mapping["series_id"],
                "benchmark_access": "synthetic_fixture",
                "minimum_daily_excess_bps": 0,
            }
            for mapping in mappings
        ],
        "bootstrap": {
            "block_marks": 2,
            "minimum_blocks": 2,
            "seed": 0,
            "replications": 500,
            "family_alpha_bps": 500,
        },
        "replay_source": {"replay_sha256": replay_sha, "benchmarks": mappings},
    }


def dumps(document):
    return json.dumps(document).encode()


def inputs(source=None):
    replay = load_replay(dumps(source or replay_document()))
    manifest = manifest_document(run_replay(replay)["normalized_input_sha256"])
    return replay, manifest


def derive(replay, manifest):
    protocol = load_protocol(dumps(manifest))
    evidence = derive_replay_evidence(
        protocol, replay, expected_protocol_sha256=protocol_sha256(protocol)
    )
    return protocol, evidence


def scheduled_inputs():
    source = replay_document()
    source["orders"] = []
    source["cash_dividends"] = []
    source["quotes"] = [
        quote("TSLA", 1, "10.00", "10.00"),
        quote("NVDA", 1, "10.00", "10.00"),
        quote("TSLA", 2, "10.00", "10.00", hour=1),
        quote("NVDA", 2, "10.00", "10.00", hour=1),
        quote("TSLA", 3, "20.00", "20.00"),
        quote("NVDA", 3, "5.00", "5.00"),
        quote("TSLA", 3, "20.00", "20.00", hour=13)
        | {"observed_at": "2026-10-03T12:30:00Z", "available_at": "2026-10-03T12:30:00Z"},
        quote("NVDA", 3, "5.00", "5.00", hour=13)
        | {"observed_at": "2026-10-03T13:30:00Z", "available_at": "2026-10-03T13:30:00Z"},
    ]
    replay, manifest = inputs(source)
    manifest["replay_source"]["benchmarks"][-1].update(
        kind="equal_weight_scheduled",
        rebalance_schedule={
            "rebalance_at": [at(3)],
            "execution_phase_seconds": 3600,
            "max_mark_age_seconds": 604800,
        },
    )
    return replay, manifest


def test_frozen_equal_weight_daily_nav_reuses_one_schedule_on_every_prefix():
    replay, manifest = scheduled_inputs()
    protocol, evidence = derive(replay, manifest)
    assert [row.nav_usd["universe"] for row in evidence.daily_nav] == [
        "1000.00",
        "990.20",
        "1227.75",
        "1227.75",
    ]

    schedule = protocol.replay_source.benchmarks[-1].rebalance_schedule
    start = datetime.combine(protocol.expected_nav_dates[0], time.max, tzinfo=UTC)
    early = datetime.fromisoformat("2026-10-03T12:45:00Z")
    end = datetime.combine(protocol.expected_nav_dates[-1], time.max, tzinfo=UTC)

    def run(tape, cutoff):
        return run_benchmarks(
            tape,
            start_at=start,
            through=cutoff,
            buy_and_hold_symbol=None,
            include_equal_weight=False,
            equal_weight_schedule=schedule,
        )["benchmarks"]["equal_weight_scheduled"]

    full = run(replay, end)
    prefix = run(replay_prefix(replay, early), early)
    assert full["schedule_sha256"] == prefix["schedule_sha256"]
    assert full["ending_bid_mark_nav_usd"] == evidence.daily_nav[-1].nav_usd["universe"]
    assert prefix["fills"] == full["fills"][:3]
    assert "free_cash_before_buy_usd" not in prefix["rebalances"][1]
    assert prefix["ending_cash_usd"] == full["rebalances"][1]["free_cash_before_buy_usd"]
    assert derive(replay, manifest) == (protocol, evidence)


def test_frozen_equal_weight_binding_is_strict_and_normalized():
    _, manifest = scheduled_inputs()
    spec = manifest["replay_source"]["benchmarks"][-1]
    schedule = spec["rebalance_schedule"]
    original = protocol_sha256(load_protocol(dumps(manifest)))
    schedule["rebalance_at"] = ["2026-10-03T13:00:00+01:00"]
    assert protocol_sha256(load_protocol(dumps(manifest))) == original
    schedule["rebalance_at"] = [at(4)]
    assert protocol_sha256(load_protocol(dumps(manifest))) != original
    schedule["rebalance_at"] = [at(5)]
    with pytest.raises(ValueError, match="inside the evaluation"):
        load_protocol(dumps(manifest))
    schedule["rebalance_at"] = ["2026-10-03T12:00:00.0000001Z"]
    with pytest.raises(ValueError, match="microsecond precision"):
        load_protocol(dumps(manifest))
    schedule["rebalance_at"] = [at(3)]
    schedule["unexpected"] = True
    with pytest.raises(ValueError, match="unknown field"):
        load_protocol(dumps(manifest))
    schedule.pop("unexpected")
    spec["kind"] = "equal_weight_once"
    with pytest.raises(ValueError, match="Only equal_weight_scheduled"):
        load_protocol(dumps(manifest))
    spec["kind"] = "equal_weight_scheduled"
    spec.pop("rebalance_schedule")
    with pytest.raises(ValueError, match="requires a frozen rebalance schedule"):
        load_protocol(dumps(manifest))


def test_durable_research_cli_input_feeds_frozen_daily_evidence(tmp_path, capsys):
    root = Path(__file__).resolve().parents[3]
    export = root / "contracts" / "valid-research-replay-export.json"
    execution = quote("TSLA", 4, "99.99", "100.00")
    execution["observed_at"] = execution["available_at"] = "2026-10-04T12:02:00Z"
    tape = tmp_path / "tape.json"
    tape.write_bytes(
        dumps(
            {
                **replay_document(),
                "commission_bps": 10,
                "quotes": [
                    quote("TSLA", 2, "99.99", "100.00", hour=0),
                    quote("TSLA", 4, "100.00", "100.00"),
                    execution,
                    quote("TSLA", 5, "105.00", "105.01"),
                ],
                "orders": [],
                "cash_dividends": [],
            }
        )
    )
    args = [
        "research-replay",
        str(export),
        str(tape),
        "--export-sha256",
        sha256(export.read_bytes()).hexdigest(),
        "--tape-sha256",
        sha256(tape.read_bytes()).hexdigest(),
        "--require-snapshot-continuity",
    ]
    assert main(args) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["snapshot_continuity_verified"] is True
    assert main([*args, "--emit-replay-input"]) == 0
    replay = load_replay(capsys.readouterr().out.encode())
    assert run_replay(replay)["normalized_input_sha256"] == report["normalized_input_sha256"]

    manifest = manifest_document(report["normalized_input_sha256"])
    manifest["splits"].update(
        train_start="2026-10-02",
        validation_start="2026-10-03",
        test_start="2026-10-04",
        end_exclusive="2026-10-06",
    )
    manifest["expected_nav_dates"] = [f"2026-10-{day:02d}" for day in range(2, 6)]
    protocol, evidence = derive(replay, manifest)
    assert [day.nav_usd["fixture-strategy"] for day in evidence.daily_nav] == [
        "1000.00",
        "1000.00",
        "999.89",
        "1004.90",
    ]
    statistics = evaluate_statistics(
        protocol, evidence, expected_protocol_sha256=protocol_sha256(protocol)
    )
    assert statistics["alpha_measured"] is False


def test_daily_native_marks_benchmark_cash_dividends_and_delayed_quotes():
    replay, manifest = inputs()
    protocol, evidence = derive(replay, manifest)
    assert [row.nav_usd for row in evidence.daily_nav] == [
        {
            "fixture-strategy": "1000.00",
            "usd-cash": "1000.00",
            "stock-hold": "1000.00",
            "universe": "1000.00",
        },
        {
            "fixture-strategy": "988.00",
            "usd-cash": "1000.00",
            "stock-hold": "946.00",
            "universe": "962.50",
        },
        {
            "fixture-strategy": "990.00",
            "usd-cash": "1000.00",
            "stock-hold": "955.00",
            "universe": "966.50",
        },
        {
            "fixture-strategy": "1017.80",
            "usd-cash": "1000.00",
            "stock-hold": "1090.00",
            "universe": "1026.50",
        },
    ]
    assert (
        evidence.replay_provenance.normalized_replay_sha256
        == manifest["replay_source"]["replay_sha256"]
    )
    report = evaluate_statistics(
        protocol, evidence, expected_protocol_sha256=protocol_sha256(protocol)
    )
    assert report["test_window"]["paired_return_count"] == 1
    assert report["alpha_measured"] is False and report["clean_out_of_sample"] is False
    assert report["verification_scope"]["ledger_provenance_verified"] is False
    assert report["comparisons"][0]["strategy_total_return_bps"] == "280.808081"


def test_native_checkpoints_match_prefixes_at_equal_time_fills_and_corporate_actions():
    source = replay_document()
    source["quotes"].append(quote("TSLA", 3, "52.00", "52.00", hour=14))
    source["stock_splits"] = [
        {
            "action_id": "tsla-split",
            "symbol": "TSLA",
            "available_at": at(3, 13),
            "effective_at": at(3, 14),
            "new_shares": 2,
            "old_shares": 1,
        }
    ]
    # The late, pre-split observation cannot replace a mark in current units.
    source["quotes"][-3].update(observed_at=at(3, 12))
    replay = load_replay(dumps(source))
    start_at = datetime.fromisoformat(at(1, 18))
    cutoffs = tuple(datetime.fromisoformat(value) for value in (at(2), at(3, 14), at(4)))
    config = {
        "start_at": start_at,
        "buy_and_hold_symbol": "TSLA",
        "include_equal_weight": True,
        "trend": TrendConfig("TSLA", 1, 36 * 3600),
        "equal_weight_schedule": EqualWeightSchedule(
            (datetime.fromisoformat(at(3, 13)),), 60, 604800,
        ),
    }
    strategy = replay_nav_series(replay, cutoffs)
    benchmarks = benchmark_nav_series(replay, cutoffs, **config)
    account_cutoffs = (
        datetime.fromisoformat(at(1, 11)), *cutoffs, datetime.fromisoformat(at(4, 14)),
    )
    through = account_cutoffs[-1]
    terminal_report, accounts = run_replay_with_account_checkpoints(
        replay, through=through, cutoffs=account_cutoffs,
    )
    assert terminal_report == run_replay(replay, through=through)
    assert accounts == {cutoff: replay_account_at(replay, cutoff) for cutoff in account_cutoffs}
    assert list(accounts.values()) == [
        ("1000.00", {}), ("798.00", {"TSLA": "2"}), ("800.00", {"TSLA": "4"}),
        ("1017.80", {"TSLA": "2"}), ("1017.80", {"TSLA": "2"}),
    ]

    for cutoff in cutoffs:
        prefix = replay_prefix(replay, cutoff)
        terminal = run_replay(prefix, through=cutoff)
        assert strategy[cutoff].nav_usd == terminal["ending_bid_mark_nav_usd"]
        assert strategy[cutoff].held_symbols == terminal["open_positions"].keys()
        reports = run_benchmarks(prefix, through=cutoff, **config)["benchmarks"]
        assert benchmarks.keys() == reports.keys()
        for kind, result in reports.items():
            mark = benchmarks[kind][cutoff]
            assert mark.nav_usd == result["ending_bid_mark_nav_usd"]
            assert mark.held_symbols == result["open_positions"].keys()


def test_future_quote_changes_only_future_nav_after_refreezing():
    replay, manifest = inputs()
    _, original = derive(replay, manifest)
    source = replay_document()
    source["quotes"][-1].update(bid_usd="130.00", ask_usd="131.00")
    changed, other_manifest = inputs(source)
    with pytest.raises(ValueError, match="replay SHA-256"):
        derive(changed, manifest)
    _, refrozen = derive(changed, other_manifest)
    assert original.daily_nav[:-1] == refrozen.daily_nav[:-1]
    assert original.daily_nav[-1] != refrozen.daily_nav[-1]


@pytest.mark.parametrize("change", ["gross", "operating_net", "unmapped", "symbol", "duplicate"])
def test_invalid_cost_basis_and_benchmark_mappings_fail_closed(change):
    replay, manifest = inputs()
    mappings = manifest["replay_source"]["benchmarks"]
    if change in {"gross", "operating_net"}:
        manifest["nav_basis"] = change
    elif change == "unmapped":
        mappings.pop()
    elif change == "symbol":
        mappings[1]["symbol"] = "NONE"
    else:
        mappings.append(mappings[0])
    with pytest.raises(ValueError, match="execution_net|benchmark"):
        derive(replay, manifest)


def test_pre_anchor_intent_is_rejected_and_generated_provenance_is_required():
    source = replay_document()
    source["orders"][0].update(ready_at=at(1, 11), expires_at=at(1, 15))
    replay, manifest = inputs(source)
    with pytest.raises(ValueError, match="initial NAV anchor"):
        derive(replay, manifest)
    replay, manifest = inputs()
    protocol, evidence = derive(replay, manifest)
    tampered = msgspec.structs.replace(evidence, replay_provenance=msgspec.UNSET)
    with pytest.raises(ValueError, match="replay provenance"):
        evaluate_statistics(protocol, tampered, expected_protocol_sha256=protocol_sha256(protocol))


def test_buy_and_hold_only_uses_registered_benchmark():
    source = replay_document()
    source["stock_splits"] = [
        {
            "action_id": "nvda-reverse",
            "symbol": "NVDA",
            "available_at": at(2, 15),
            "effective_at": at(3, 10),
            "new_shares": 1,
            "old_shares": 2,
        }
    ]
    replay, manifest = inputs(source)
    manifest["comparisons"] = manifest["comparisons"][1:2]
    manifest["replay_source"]["benchmarks"] = manifest["replay_source"]["benchmarks"][1:2]
    _, evidence = derive(replay, manifest)
    assert evidence.daily_nav[2].nav_usd == {"fixture-strategy": "990.00", "stock-hold": "955.00"}


def test_frozen_trend_mapping_runs_native_prefixes_with_fees_and_dividend():
    source = replay_document()
    source["quotes"].extend(
        [
            quote("TSLA", 2, "109.00", "110.00", hour=13),
            quote("TSLA", 2, "109.00", "110.00", hour=14),
        ]
    )
    replay, manifest = inputs(source)
    trend = {
        "series_id": "trend-benchmark",
        "kind": "trend_momentum",
        "symbol": "TSLA",
        "lookback_quotes": 1,
        "max_gap_seconds": 36 * 3600,
    }
    manifest["replay_source"]["benchmarks"].append(trend)
    manifest["comparisons"].append(
        manifest["comparisons"][0]
        | {"comparison_id": "trend-benchmark", "benchmark": "trend-benchmark"}
    )
    protocol, evidence = derive(replay, manifest)
    assert [row.nav_usd["trend-benchmark"] for row in evidence.daily_nav] == [
        "1000.00",
        "981.10",
        "990.10",
        "999.10",
    ]
    trend["lookback_quotes"] = 2
    assert protocol_sha256(protocol) != protocol_sha256(load_protocol(dumps(manifest)))
    trend.pop("max_gap_seconds")
    with pytest.raises(ValueError, match="trend_momentum"):
        load_protocol(dumps(manifest))


def test_cli_emits_evidence_readable_by_statistics(tmp_path, capsys):
    _, manifest = inputs()
    manifest_path, replay_path, result_path = [
        tmp_path / name for name in ("plan", "fixture", "evidence")
    ]
    manifest_path.write_bytes(dumps(manifest))
    replay_path.write_bytes(dumps(replay_document()))
    digest = protocol_sha256(load_protocol(dumps(manifest)))
    assert (
        main(["replay-evidence", str(manifest_path), str(replay_path), "--protocol-sha256", digest])
        == 0
    )
    result_path.write_text(capsys.readouterr().out, encoding="utf-8")
    evidence = load_evidence(result_path.read_bytes())
    assert evidence.replay_provenance.external_costs_usd is None
    assert (
        main(["statistics", str(manifest_path), str(result_path), "--protocol-sha256", digest]) == 0
    )
    report = json.loads(capsys.readouterr().out)
    assert report["replay_provenance_declared"]["method"] == "nautilus_prefix"
