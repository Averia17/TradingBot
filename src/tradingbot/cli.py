"""Research CLI. No wallet, swap, submit, signing or live command exists."""

import argparse
import hashlib
import json
import sys
from datetime import UTC, datetime
from importlib.metadata import PackageNotFoundError, version
from pathlib import Path

from tradingbot.adapters.tradingagents import UpstreamCompatibilityError
from tradingbot.contracts import DecisionBatch, PortfolioSnapshot


def write_json(path: Path, value) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def save_run(output: Path, batch, reports, book, metadata) -> None:
    output.mkdir(parents=True, exist_ok=False)
    write_json(output / "portfolio.json", book.model_dump(mode="json"))
    write_json(output / "reports.json", reports)
    write_json(output / "intents.json", batch.model_dump(mode="json"))
    write_json(output / "metadata.json", metadata)


def analyze(args):
    # Validate user inputs and paths BEFORE any model or market request.
    book = PortfolioSnapshot.model_validate_json(args.portfolio.read_text(encoding="utf-8-sig"))
    date = args.date or book.as_of.date().isoformat()
    if date != book.as_of.date().isoformat():
        raise ValueError("analysis date must match portfolio snapshot date")
    if book.as_of > datetime.now(UTC):
        raise ValueError("portfolio snapshot cannot be in the future")
    symbols = args.symbols
    if len(set(symbols)) != len(symbols) or any(
        not symbol.isascii() or not symbol.isalnum() or symbol != symbol.upper()
        for symbol in symbols
    ):
        raise ValueError("use unique uppercase US equity symbols")
    if args.output.exists():
        raise FileExistsError("run output already exists; choose a new path")
    from tradingbot.adapters.tradingagents import create_runtime
    from tradingbot.budget import CallBudget
    from tradingbot.research import collect_reports, create_decisions

    overrides = json.loads(args.config.read_text(encoding="utf-8-sig")) if args.config else None
    budget = CallBudget(args.max_calls)
    runtime = create_runtime(args.output.parent / "upstream-runtime", overrides, callbacks=[budget])
    config = runtime.config
    reports = collect_reports(runtime.adapter, symbols, date, book)
    batch = create_decisions(runtime.decision_model(), reports, book)
    metadata = budget.summary() | {
        "source_mode": "upstream_date_research",
        "analysis_date": date,
        "models": {
            key: config[key] for key in ("llm_provider", "quick_think_llm", "deep_think_llm")
        },
        "config_sha256": hashlib.sha256(
            json.dumps(config, sort_keys=True, default=str).encode()
        ).hexdigest(),
        "intraday_pit_verified": False,
        "alpha_measured": False,
    }
    save_run(args.output, batch, reports, book, metadata)
    return batch


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    demo = commands.add_parser("demo", help="Offline synthetic fixture; no API calls")
    demo.add_argument(
        "--output",
        type=Path,
        default=Path("runtime") / f"demo-{datetime.now(UTC):%Y%m%d-%H%M%S-%f}",
    )
    schema = commands.add_parser("schema", help="Export the exact research JSON contract")
    schema.add_argument("--output", type=Path, required=True)
    commands.add_parser("doctor", help="Show installed component versions without credentials")
    commands.add_parser(
        "probe-engine", help="Nautilus synthetic cash/positions compatibility probe"
    )
    real = commands.add_parser(
        "analyze", help="Run actual TradingAgents; requires provider API credentials"
    )
    real.add_argument("--portfolio", type=Path, required=True)
    real.add_argument("--symbols", nargs="+", required=True)
    real.add_argument("--date")
    real.add_argument("--config", type=Path)
    real.add_argument("--max-calls", type=int, default=64)
    real.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "schema":
            args.output.parent.mkdir(parents=True, exist_ok=True)
            write_json(args.output, DecisionBatch.model_json_schema())
        elif args.command == "doctor":
            from tradingbot.adapters.tradingagents import check_compatibility

            components = {}
            for name in ("self-custody-ai-trader", "tradingagents", "nautilus_trader"):
                try:
                    components[name] = version(name)
                except PackageNotFoundError:
                    components[name] = "not installed"
            components["tradingagents_adapter"] = check_compatibility()
            print(json.dumps(components, indent=2))
        elif args.command == "probe-engine":
            from tradingbot.evaluation import run_synthetic_probe

            print(json.dumps(run_synthetic_probe(), indent=2))
        else:
            if args.command == "demo":
                from tradingbot.demo import demo_batch

                batch, reports, book = demo_batch()
                save_run(
                    args.output,
                    batch,
                    reports,
                    book,
                    {"source_mode": "synthetic_fixture", "llm_calls": 0, "alpha_measured": False},
                )
            else:
                batch = analyze(args)
            print(batch.model_dump_json(indent=2))
            print(f"Artifacts: {args.output.resolve()}", file=sys.stderr)
    except UpstreamCompatibilityError as exc:
        print(f"Upstream compatibility check failed: {exc}", file=sys.stderr)
        return 2
    except (OSError, ValueError, RuntimeError) as exc:
        print(
            f"Research run failed ({type(exc).__name__}); no execution is possible.",
            file=sys.stderr,
        )
        return 2
    return 0
