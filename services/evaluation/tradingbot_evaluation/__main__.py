"""Run a local synthetic evaluation or report the installed runtime identity."""

import argparse
import json
import sys
from datetime import datetime
from hashlib import sha256
from importlib.metadata import version
from pathlib import Path

import msgspec

from tradingbot_evaluation.benchmarks import run_benchmarks
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


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("doctor", help="Report installed versions and lock identity")
    commands.add_parser("probe-engine", help="Run the two-equity native smoke fixture")
    commands.add_parser("schema", help="Write the replay input JSON schema to stdout")
    replay = commands.add_parser("replay", help="Evaluate a local synthetic replay fixture")
    replay.add_argument("input", type=Path)
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
                    evidence = build_paper_evidence(
                        protocol, raw, expected_protocol_sha256=args.protocol_sha256
                    )
                result = json.loads(msgspec.json.encode(evidence))
            else:
                with args.evidence.open("rb") as source:
                    evidence = load_evidence(source.read(MAX_INPUT_BYTES + 1))
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
                result = run_replay(fixture)
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"Evaluation failed: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
