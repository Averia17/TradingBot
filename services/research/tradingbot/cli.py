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
    from tradingbot.adapters.tradingagents import (
        create_portfolio_model,
        create_runtime,
        research_config,
        usage_model_identities,
    )
    from tradingbot.budget import CallBudget, capture_usage
    from tradingbot.prepared_analysis import load_prepared_reports
    from tradingbot.research import collect_reports, create_decisions

    overrides = json.loads(args.config.read_text(encoding="utf-8-sig")) if args.config else None
    if (args.data_archive is None) != (args.data_sha256 is None):
        raise ValueError("prepared analysis requires --data-archive and --data-sha256 together")
    snapshot = None
    if args.data_archive is not None:
        snapshot, reports = load_prepared_reports(args.data_archive, args.data_sha256, book, symbols)
    budget = CallBudget(args.max_calls)
    runtime_path = args.output.parent / "upstream-runtime"
    with capture_usage(args.usage_output, budget, "provider_reported"):
        if snapshot is None:
            runtime = create_runtime(runtime_path, overrides, callbacks=[budget])
            config = runtime.config
        else:
            config = research_config(runtime_path, overrides)
        budget.configure_identities(usage_model_identities(config))
        if snapshot is None:
            with budget.role("research"):
                reports = collect_reports(runtime.adapter, symbols, date, book)
        with budget.role("portfolio_allocator"):
            model = create_portfolio_model(config, callbacks=[budget])
            batch = create_decisions(model, reports, book)
    metadata = budget.summary() | {
        "source_mode": "upstream_date_research",
        "analysis_date": date,
        "models": {
            key: config[key] for key in ("llm_provider", "quick_think_llm", "deep_think_llm")
        } | {"reasoning_effort": config.get("openai_reasoning_effort")},
        "config_sha256": hashlib.sha256(
            json.dumps(config, sort_keys=True, default=str).encode()
        ).hexdigest(),
        "intraday_pit_verified": False,
        "alpha_measured": False,
    }
    if snapshot is not None:
        metadata |= {
            "source_mode": "prepared_archive_research",
            "snapshot_sha256": snapshot.sha256,
            "archive_sha256": snapshot.archive.sha256,
            "archive_source_mode": snapshot.source_mode,
        }
    save_run(args.output, batch, reports, book, metadata)
    return batch


def _identity_pin(args) -> dict[str, str] | None:
    if (args.identity_registry_ref is None) != (args.identity_registry_sha256 is None):
        raise ValueError("provide both identity registry reference and SHA-256")
    if args.identity_registry_ref is None:
        return None
    return {"ref": args.identity_registry_ref, "sha256": args.identity_registry_sha256}


def _serve_worker(args) -> None:
    import logging
    import signal
    from threading import Event

    from tradingbot.worker import run_worker

    registry = _identity_pin(args)
    stop = Event()

    def request_stop(_signum, _frame):
        stop.set()

    handlers = {
        signum: signal.signal(signum, request_stop) for signum in (signal.SIGINT, signal.SIGTERM)
    }
    try:
        logging.basicConfig(level=logging.INFO)
        run_worker(
            args.control_url,
            args.worker_id,
            stop=stop,
            poll_interval=args.poll_interval,
            heartbeat_interval=args.heartbeat_interval,
            request_timeout=args.request_timeout,
            archive_root=args.archive_root,
            identity_registry=registry,
        )
    finally:
        for signum, handler in handlers.items():
            signal.signal(signum, handler)


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
    schema.add_argument(
        "--document", choices=("decision-batch", "llm-usage", "perp-screen"),
        default="decision-batch",
    )
    commands.add_parser("doctor", help="Show installed component versions without credentials")
    commands.add_parser(
        "probe-engine", help="Nautilus synthetic cash/positions compatibility probe"
    )
    worker = commands.add_parser("worker", help="Poll Control with offline TradingAgents models")
    worker.add_argument("--control-url", default="http://127.0.0.1:8080")
    worker.add_argument("--worker-id", help="Worker identity (generated by default)")
    worker.add_argument(
        "--archive-root",
        type=Path,
        help="Require verified local data manifests under this directory",
    )
    worker.add_argument("--identity-registry-ref", help="Owner-approved archive-relative registry")
    worker.add_argument("--identity-registry-sha256", help="Owner-approved registry SHA-256")
    worker.add_argument(
        "--poll-interval", type=float, default=1, help="Polling interval in seconds"
    )
    worker.add_argument(
        "--heartbeat-interval", type=float, default=10, help="Maximum heartbeat interval in seconds"
    )
    worker.add_argument(
        "--request-timeout", type=float, default=5, help="Control HTTP timeout in seconds"
    )
    historical = commands.add_parser(
        "historical-step", help="Read one synthetic historical step from stdin"
    )
    historical.add_argument("--archive-root", type=Path, required=True)
    historical.add_argument("--usage-output", type=Path, help="Write local model-callback evidence")
    historical.add_argument("--identity-registry-ref", help="Owner-approved archive-relative registry")
    historical.add_argument("--identity-registry-sha256", help="Owner-approved registry SHA-256")
    identity_audit = commands.add_parser(
        "verify-identity-export", help="Recompute the stable identity projection from a Go export"
    )
    identity_audit.add_argument("--input", type=Path, required=True)
    identity_audit.add_argument("--input-sha256", required=True)
    identity_audit.add_argument("--archive-root", type=Path, required=True)
    identity_audit.add_argument("--identity-registry-ref", required=True)
    identity_audit.add_argument("--identity-registry-sha256", required=True)
    identity_review = commands.add_parser(
        "prepare-identity-review", help="Prepare a frozen stable paper review from local Go receipts"
    )
    identity_review.add_argument("--paper-cli", type=Path, required=True)
    identity_review.add_argument("--archive-root", type=Path, required=True)
    identity_review.add_argument("--account", required=True)
    identity_review.add_argument("--order", required=True)
    identity_review.add_argument("--as-of", required=True, help="Frozen RFC 3339 cutoff")
    identity_review.add_argument("--policy", type=Path, required=True)
    identity_review.add_argument("--marks", type=Path, required=True)
    for reference in ("data-manifest", "identity-registry", "quote-tape"):
        identity_review.add_argument(f"--{reference}-ref", required=True)
        identity_review.add_argument(f"--{reference}-sha256", required=True)
    identity_review.add_argument("--max-llm-calls", type=int, required=True)
    prepared = commands.add_parser(
        "prepare-research", help="Collect equity prices and merge pinned issuer archives"
    )
    prepared.add_argument("--symbols", nargs="+", required=True)
    prepared.add_argument("--source", action="append", nargs=2, required=True, metavar=("PATH", "SHA256"))
    prepared.add_argument("--cash-usd", default="1000", help="Cash for the virtual research portfolio")
    prepared.add_argument("--output", type=Path, required=True)
    real = commands.add_parser(
        "analyze", help="Run actual TradingAgents; requires provider API credentials"
    )
    real.add_argument("--portfolio", type=Path, required=True)
    real.add_argument("--symbols", nargs="+", required=True)
    real.add_argument("--date")
    real.add_argument("--config", type=Path)
    real.add_argument("--data-archive", type=Path, help="Read market, filing and news evidence from a local archive")
    real.add_argument("--data-sha256", help="SHA-256 of the exact prepared archive bytes")
    real.add_argument("--max-calls", type=int, default=64)
    real.add_argument("--output", type=Path, required=True)
    real.add_argument("--usage-output", type=Path, help="Write local model-callback evidence")
    perp = commands.add_parser("perp-research", help="Select a perpetual entry for research")
    perp.add_argument("--study", type=Path, required=True)
    perp.add_argument("--screen-binary", type=Path, required=True)
    perp.add_argument("--fixture-forecast", type=Path, help="Scripted forecast for synthetic input")
    perp.add_argument("--config", type=Path)
    perp.add_argument("--max-calls", type=int, choices=range(1, 65), default=64, metavar="1-64")
    perp.add_argument("--usage-output", type=Path)
    equity = commands.add_parser("equity-theses", help="Forecast underlying equities for route research")
    equity.add_argument("--route-binary", type=Path, required=True)
    equity.add_argument("--study", type=Path, required=True)
    equity.add_argument("--archive-root", type=Path, required=True)
    for reference in ("data-manifest", "identity-registry"):
        equity.add_argument(f"--{reference}-ref", required=True)
        equity.add_argument(f"--{reference}-sha256", required=True)
    equity.add_argument("--config", type=Path)
    equity.add_argument("--usage-output", type=Path)
    equity.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "schema":
            from tradingbot.llm_usage import LLMUsageReport

            if args.document == "perp-screen":
                from tradingbot.perp_research import PerpScreen

                document = PerpScreen
            else:
                document = LLMUsageReport if args.document == "llm-usage" else DecisionBatch
            args.output.parent.mkdir(parents=True, exist_ok=True)
            write_json(args.output, document.model_json_schema(mode="serialization"))
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
            from tradingbot.nautilus_probe import run_synthetic_probe

            print(json.dumps(run_synthetic_probe(), indent=2))
        elif args.command == "perp-research":
            from tradingbot.perp_research import run_perp_research

            print(json.dumps(run_perp_research(args), indent=2, ensure_ascii=True))
        elif args.command == "equity-theses":
            from tradingbot.equity_thesis import run_equity_theses

            print(json.dumps(run_equity_theses(args), indent=2))
        elif args.command == "worker":
            _serve_worker(args)
        elif args.command == "historical-step":
            from tradingbot.historical_step import MAX_STEP_BYTES, run_historical_step

            data = sys.stdin.buffer.read(MAX_STEP_BYTES + 1)
            result = run_historical_step(
                data, args.archive_root, usage_output=args.usage_output,
                owner_identity_registry=_identity_pin(args),
            )
            print(json.dumps(result, sort_keys=True, separators=(",", ":")))
        elif args.command == "verify-identity-export":
            from tradingbot.identity_audit import MAX_IDENTITY_EXPORT_BYTES, audit_identity_export

            with args.input.open("rb") as stream:
                data = stream.read(MAX_IDENTITY_EXPORT_BYTES + 1)
            registry = {"ref": args.identity_registry_ref, "sha256": args.identity_registry_sha256}
            result = audit_identity_export(data, args.input_sha256, args.archive_root, registry)
            print(json.dumps(result, sort_keys=True, separators=(",", ":")))
        elif args.command == "prepare-identity-review":
            from tradingbot.prepare_identity_review import prepare_from_args

            print(json.dumps(prepare_from_args(args), separators=(",", ":")))
        elif args.command == "prepare-research":
            from tradingbot.prepare_research import prepare

            if args.output.exists():
                raise FileExistsError("prepared output already exists; choose a new path")
            sources = [(Path(path), digest) for path, digest in args.source]
            book, archive = prepare(args.symbols, sources, args.cash_usd)
            args.output.mkdir(parents=True, exist_ok=False)
            archive_path = args.output / "data-archive.json"
            write_json(archive_path, archive)
            write_json(args.output / "portfolio.json", book.model_dump(mode="json"))
            print(json.dumps({
                "as_of": book.as_of.isoformat(),
                "archive_sha256": hashlib.sha256(archive_path.read_bytes()).hexdigest(),
                "records": len(archive["records"]),
            }))
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
