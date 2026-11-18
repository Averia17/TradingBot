"""Run a local synthetic evaluation or report the installed runtime identity."""

import argparse
import json
import sys
from datetime import datetime
from hashlib import sha256
from importlib.metadata import version
from pathlib import Path

import msgspec

from tradingbot_evaluation.ablation_evidence import derive_ablation_evidence
from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.forward_freeze import (
    forward_configuration_sha256,
    forward_freeze_sha256,
    freeze_forward,
    load_forward_freeze,
    load_forward_plan,
    verify_forward,
)
from tradingbot_evaluation.operating_costs import (
    OperatingCosts,
    load_operating_costs,
    operating_costs_sha256,
)
from tradingbot_evaluation.paper_evidence import build_paper_evidence
from tradingbot_evaluation.replay import (
    MAX_INPUT_BYTES,
    REPLAY_VERSION,
    Replay,
    load_replay,
    run_replay,
    synthetic_probe,
)
from tradingbot_evaluation.replay_evidence import derive_replay_evidence
from tradingbot_evaluation.statistics import (
    EvaluationEvidence,
    EvaluationProtocol,
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)
from tradingbot_evaluation.trend import TrendConfig
from tradingbot_evaluation.walk_forward import WalkForwardPlan, local_research, run_walk_forward
from tradingbot_evaluation.walk_forward_evidence import derive_walk_forward_evidence


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("doctor", help="Report installed versions and lock identity")
    commands.add_parser("probe-engine", help="Run the two-equity native smoke fixture")
    commands.add_parser("schema", help="Write the replay input JSON schema to stdout")
    commands.add_parser("walk-forward-schema", help="Write the walk-forward plan JSON schema")
    replay = commands.add_parser("replay", help="Evaluate a local synthetic replay fixture")
    replay.add_argument("input", type=Path)
    replay.add_argument("--through", type=datetime.fromisoformat, help="Explicit replay horizon")
    research_replay = commands.add_parser(
        "research-replay", help="Replay durable offline research decisions against a synthetic tape"
    )
    research_replay.add_argument("export", type=Path)
    research_replay.add_argument("tape", type=Path)
    research_replay.add_argument("--export-sha256", required=True)
    research_replay.add_argument("--tape-sha256", required=True)
    research_replay.add_argument(
        "--require-snapshot-continuity",
        action="store_true",
        help="Verify every stored portfolio against the native account at its cutoff",
    )
    research_replay.add_argument(
        "--emit-replay-input",
        action="store_true",
        help="Write derived Nautilus input for freezing and daily evidence instead of the report",
    )
    walk = commands.add_parser(
        "walk-forward", help="Generate offline analyses on successive simulated portfolio snapshots"
    )
    walk_evidence = commands.add_parser(
        "walk-forward-evidence", help="Create daily NAV evidence from a frozen offline study"
    )
    walk_evidence.add_argument("protocol", type=Path)
    walk_evidence.add_argument("--protocol-sha256", required=True)
    ablation = commands.add_parser(
        "ablation-evidence", help="Compare frozen offline strategy variants on paired NAV dates"
    )
    ablation.add_argument("protocol", type=Path)
    ablation.add_argument("--protocol-sha256", required=True)
    for study in (walk, walk_evidence, ablation):
        study.add_argument("plan", type=Path)
        study.add_argument("tape", type=Path)
        study.add_argument("--research-python", required=True, type=Path)
        study.add_argument("--archive-root", required=True, type=Path)
    walk.add_argument("--plan-sha256", required=True)
    walk.add_argument("--tape-sha256", required=True)
    walk.add_argument(
        "--include-replay-input",
        action="store_true",
        help="Include the derived native replay input",
    )
    benchmark = commands.add_parser("benchmarks", help="Compare baselines on a local quote fixture")
    benchmark.add_argument("input", type=Path)
    benchmark.add_argument("--start-at", type=datetime.fromisoformat, required=True)
    benchmark.add_argument("--buy-and-hold-symbol", required=True)
    benchmark.add_argument("--trend-symbol")
    benchmark.add_argument("--trend-lookback-quotes", type=int)
    benchmark.add_argument("--trend-max-gap-seconds", type=int)
    statistics_schema = commands.add_parser(
        "statistics-schema", help="Write a statistics JSON schema"
    )
    statistics_schema.add_argument("document", choices=("protocol", "evidence", "operating-costs"))
    freeze = commands.add_parser("freeze-statistics", help="Print the digest of a local protocol")
    freeze.add_argument("protocol", type=Path)
    forward_config = commands.add_parser(
        "forward-config-sha256", help="Hash the local strategy configuration bundle"
    )
    forward_config.add_argument("plan", type=Path)
    forward_config.add_argument("--root", type=Path, required=True)
    forward_freeze = commands.add_parser(
        "freeze-forward", help="Write canonical freeze JSON for a local strategy and protocol"
    )
    forward_freeze.add_argument("plan", type=Path)
    forward_freeze.add_argument("protocol", type=Path)
    forward_freeze.add_argument("--root", type=Path, required=True)
    forward_verify = commands.add_parser(
        "verify-forward", help="Verify a pinned forward freeze against local files"
    )
    forward_verify.add_argument("freeze", type=Path)
    forward_verify.add_argument("protocol", type=Path)
    forward_verify.add_argument("--root", type=Path, required=True)
    forward_verify.add_argument("--freeze-sha256", required=True)
    freeze_costs = commands.add_parser(
        "freeze-operating-costs", help="Print the digest of a local USD operating cost ledger"
    )
    freeze_costs.add_argument("ledger", type=Path)
    statistics = commands.add_parser(
        "statistics", help="Audit and evaluate a frozen local NAV study"
    )
    statistics.add_argument("protocol", type=Path)
    statistics.add_argument("evidence", type=Path)
    statistics.add_argument("--protocol-sha256", required=True)
    statistics.add_argument("--evidence-sha256", help="Verify the exact bytes of a retained report")
    statistics.add_argument(
        "--operating-costs", type=Path, help="Cost ledger pinned by protocol 0.2"
    )
    replay_evidence = commands.add_parser(
        "replay-evidence", help="Derive daily statistics evidence from a frozen local replay"
    )
    replay_evidence.add_argument("protocol", type=Path)
    replay_evidence.add_argument("input", type=Path)
    replay_evidence.add_argument("--protocol-sha256", required=True)
    paper_evidence = commands.add_parser(
        "paper-evidence", help="Derive statistics evidence from a pinned paper NAV report"
    )
    paper_evidence.add_argument("protocol", type=Path)
    paper_evidence.add_argument("input", type=Path)
    paper_evidence.add_argument("--protocol-sha256", required=True)
    paper_evidence.add_argument("--benchmark-input", type=Path)
    args = parser.parse_args(argv)

    try:
        if args.command == "doctor":
            lock = Path("uv.lock")
            result = {
                "evaluation_version": version("tradingbot-evaluation"),
                "nautilus_version": version("nautilus-trader"),
                "replay_schema_version": REPLAY_VERSION,
                "evaluation_lock_sha256": sha256(lock.read_bytes()).hexdigest()
                if lock.is_file()
                else None,
            }
        elif args.command == "probe-engine":
            result = synthetic_probe()
        elif args.command in {"forward-config-sha256", "freeze-forward", "verify-forward"}:
            if args.command != "verify-forward":
                with args.plan.open("rb") as source:
                    forward = load_forward_plan(source.read(MAX_INPUT_BYTES + 1))
            if args.command == "forward-config-sha256":
                result = {"configuration_sha256": forward_configuration_sha256(args.root, forward)}
            else:
                with args.protocol.open("rb") as source:
                    protocol = load_protocol(source.read(MAX_INPUT_BYTES + 1))
                if args.command == "freeze-forward":
                    frozen = freeze_forward(args.root, forward, protocol)
                    # The exact stdout bytes are the digest pinned by verify-forward.
                    sys.stdout.write(msgspec.json.encode(frozen, order="deterministic").decode())
                    return 0
                with args.freeze.open("rb") as source:
                    frozen = load_forward_freeze(source.read(MAX_INPUT_BYTES + 1))
                verified = verify_forward(
                    args.root, frozen, protocol, expected_sha256=args.freeze_sha256
                )
                result = {
                    "verified": True,
                    "forward_freeze_sha256": forward_freeze_sha256(verified),
                    "protocol_sha256": verified.protocol_sha256,
                }
        elif args.command == "research-replay":
            from tradingbot_evaluation.research_replay import (
                MAX_RESEARCH_EXPORT_BYTES,
                prepare_research_replay,
                run_research_replay,
            )

            with args.export.open("rb") as source:
                export_raw = source.read(MAX_RESEARCH_EXPORT_BYTES + 1)
            with args.tape.open("rb") as source:
                tape_raw = source.read(MAX_INPUT_BYTES + 1)
            pins = {
                "expected_export_sha256": args.export_sha256,
                "expected_tape_sha256": args.tape_sha256,
                "require_snapshot_continuity": args.require_snapshot_continuity,
            }
            if args.emit_replay_input:
                prepared = prepare_research_replay(export_raw, tape_raw, **pins)
                result = json.loads(msgspec.json.encode(prepared.replay))
            else:
                result = run_research_replay(export_raw, tape_raw, **pins)
        elif args.command in {"walk-forward", "walk-forward-evidence", "ablation-evidence"}:
            with args.plan.open("rb") as source:
                plan_raw = source.read(MAX_INPUT_BYTES + 1)
            with args.tape.open("rb") as source:
                tape_raw = source.read(MAX_INPUT_BYTES + 1)
            if args.command == "walk-forward":
                result = run_walk_forward(
                    plan_raw,
                    tape_raw,
                    infer=local_research(args.research_python, args.archive_root),
                    expected_plan_sha256=args.plan_sha256,
                    expected_tape_sha256=args.tape_sha256,
                    include_replay_input=args.include_replay_input,
                )
            else:
                with args.protocol.open("rb") as source:
                    protocol = load_protocol(source.read(MAX_INPUT_BYTES + 1))
                if args.command == "ablation-evidence":
                    evidence = derive_ablation_evidence(
                        protocol,
                        plan_raw,
                        tape_raw,
                        expected_protocol_sha256=args.protocol_sha256,
                        infer_for=lambda variant: local_research(
                            args.research_python, args.archive_root, variant=variant
                        ),
                    )
                else:
                    evidence = derive_walk_forward_evidence(
                        protocol,
                        plan_raw,
                        tape_raw,
                        infer=local_research(args.research_python, args.archive_root),
                        expected_protocol_sha256=args.protocol_sha256,
                    )
                result = msgspec.to_builtins(evidence)
        elif args.command == "walk-forward-schema":
            result = {"$schema": "https://json-schema.org/draft/2020-12/schema"} | (
                msgspec.json.schema(WalkForwardPlan)
            )
        elif args.command in {"schema", "statistics-schema"}:
            contract = (
                Replay
                if args.command == "schema"
                else {
                    "protocol": EvaluationProtocol,
                    "evidence": EvaluationEvidence,
                    "operating-costs": OperatingCosts,
                }[args.document]
            )
            result = {
                "$schema": "https://json-schema.org/draft/2020-12/schema"
            } | msgspec.json.schema(contract)
        elif args.command == "freeze-operating-costs":
            with args.ledger.open("rb") as source:
                costs = load_operating_costs(source.read(MAX_INPUT_BYTES + 1))
            result = {"operating_costs_sha256": operating_costs_sha256(costs)}
        elif args.command in {
            "freeze-statistics",
            "statistics",
            "replay-evidence",
            "paper-evidence",
        }:
            with args.protocol.open("rb") as source:
                protocol = load_protocol(source.read(MAX_INPUT_BYTES + 1))
            if args.command == "freeze-statistics":
                result = {"protocol_sha256": protocol_sha256(protocol)}
            elif args.command in {"replay-evidence", "paper-evidence"}:
                with args.input.open("rb") as source:
                    raw = source.read(MAX_INPUT_BYTES + 1)
                if args.command == "replay-evidence":
                    evidence = derive_replay_evidence(
                        protocol, load_replay(raw), expected_protocol_sha256=args.protocol_sha256
                    )
                else:
                    benchmark = None
                    if args.benchmark_input is not None:
                        with args.benchmark_input.open("rb") as source:
                            benchmark = source.read(MAX_INPUT_BYTES + 1)
                    evidence = build_paper_evidence(
                        protocol,
                        raw,
                        expected_protocol_sha256=args.protocol_sha256,
                        benchmark_raw=benchmark,
                    )
                result = json.loads(msgspec.json.encode(evidence))
            else:
                with args.evidence.open("rb") as source:
                    raw = source.read(MAX_INPUT_BYTES + 1)
                if (
                    args.evidence_sha256 is not None
                    and sha256(raw).hexdigest() != args.evidence_sha256
                ):
                    raise ValueError("Evidence file SHA-256 differs from the expected digest")
                evidence = load_evidence(raw)
                costs = None
                if args.operating_costs is not None:
                    with args.operating_costs.open("rb") as source:
                        costs = load_operating_costs(source.read(MAX_INPUT_BYTES + 1))
                result = evaluate_statistics(
                    protocol,
                    evidence,
                    expected_protocol_sha256=args.protocol_sha256,
                    operating_costs=costs,
                )
                if args.evidence_sha256 is not None:
                    result["verification_scope"]["evidence_file_hash_verified"] = True
        else:
            with args.input.open("rb") as source:
                fixture = load_replay(source.read(MAX_INPUT_BYTES + 1))
            if args.command == "benchmarks":
                trend_args = (
                    args.trend_symbol,
                    args.trend_lookback_quotes,
                    args.trend_max_gap_seconds,
                )
                trend = None
                if any(value is not None for value in trend_args):
                    if any(value is None for value in trend_args):
                        raise ValueError(
                            "Trend benchmark requires symbol, lookback quotes and max gap seconds"
                        )
                    trend = TrendConfig(*trend_args)
                result = run_benchmarks(
                    fixture,
                    start_at=args.start_at,
                    buy_and_hold_symbol=args.buy_and_hold_symbol,
                    trend=trend,
                )
            else:
                result = run_replay(fixture, through=args.through)
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"Evaluation failed: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
