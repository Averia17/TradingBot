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
from tradingbot_evaluation.replay import (
    MAX_INPUT_BYTES,
    REPLAY_VERSION,
    Replay,
    load_replay,
    run_replay,
    synthetic_probe,
)


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
        elif args.command == "schema":
            result = {
                "$schema": "https://json-schema.org/draft/2020-12/schema"
            } | msgspec.json.schema(Replay)
        else:
            with args.input.open("rb") as source:
                fixture = load_replay(source.read(MAX_INPUT_BYTES + 1))
            if args.command == "benchmarks":
                result = run_benchmarks(
                    fixture, start_at=args.start_at, buy_and_hold_symbol=args.buy_and_hold_symbol
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
