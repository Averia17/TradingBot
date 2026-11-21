"""Run a local synthetic evaluation or report the installed runtime identity."""

import argparse
import json
import sys
from collections.abc import Callable
from datetime import date, datetime
from hashlib import sha256
from importlib.metadata import version
from pathlib import Path

import msgspec

from tradingbot_evaluation.ablation_evidence import derive_ablation_evidence
from tradingbot_evaluation.benchmarks import run_benchmarks
from tradingbot_evaluation.checkpoint_history import CheckpointReceipt
from tradingbot_evaluation.equal_weight import EqualWeightSchedule, parse_rebalance_at
from tradingbot_evaluation.forward_freeze import (
    forward_configuration_sha256,
    forward_freeze_sha256,
    freeze_forward,
    load_forward_freeze,
    load_forward_plan,
    verify_forward,
)
from tradingbot_evaluation.forward_paper import PinnedArtifact, evaluate_forward_paper
from tradingbot_evaluation.operating_costs import (
    OperatingCosts,
    load_operating_costs,
    operating_costs_sha256,
)
from tradingbot_evaluation.paper_evidence import build_paper_evidence
from tradingbot_evaluation.power_review import review_forward_power
from tradingbot_evaluation.replay import (
    MAX_INPUT_BYTES,
    REPLAY_VERSION,
    Replay,
    load_replay,
    run_replay,
    synthetic_probe,
)
from tradingbot_evaluation.replay_evidence import (
    derive_identity_replay_evidence,
    derive_replay_evidence,
)
from tradingbot_evaluation.replay_identity import MAX_IDENTITY_REGISTRY_BYTES, Reference
from tradingbot_evaluation.research_costs import (
    ResearchPermitArchive,
    ResearchRates,
    synthetic_operating_costs,
    value_research_costs,
)
from tradingbot_evaluation.research_replay import (
    MAX_RESEARCH_EXPORT_BYTES,
    prepare_research_replay,
    run_research_replay,
)
from tradingbot_evaluation.statistics import (
    EvaluationEvidence,
    EvaluationProtocol,
    evaluate_statistics,
    load_evidence,
    load_protocol,
    protocol_sha256,
)
from tradingbot_evaluation.trend import TrendConfig
from tradingbot_evaluation.walk_forward import (
    load_walk_forward_plan,
    local_research,
    run_walk_forward,
    walk_forward_plan_schema,
)
from tradingbot_evaluation.walk_forward_evidence import derive_walk_forward_evidence


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    def command(
        name: str, run: Callable[[argparse.Namespace], dict | str], help_text: str
    ) -> argparse.ArgumentParser:
        subparser = commands.add_parser(name, help=help_text)
        subparser.set_defaults(run=run)
        return subparser

    command("doctor", _doctor, "Report installed versions and lock identity")
    command("probe-engine", lambda _: synthetic_probe(), "Run the two-equity native smoke fixture")
    command("schema", _schema, "Write the replay input JSON schema to stdout").set_defaults(
        contract=Replay
    )
    command(
        "walk-forward-schema",
        lambda _: walk_forward_plan_schema(),
        "Write the walk-forward plan JSON schema",
    )
    replay = command("replay", _replay, "Evaluate a local synthetic replay fixture")
    replay.add_argument("input", type=Path)
    replay.add_argument("--through", type=datetime.fromisoformat, help="Explicit replay horizon")
    research_replay = command(
        "research-replay",
        _research_replay,
        "Replay durable offline research decisions against a synthetic tape",
    )
    research_replay.add_argument("export", type=Path)
    research_replay.add_argument("tape", type=Path)
    research_replay.add_argument("--export-sha256", required=True)
    research_replay.add_argument("--tape-sha256", required=True)
    research_replay.add_argument("--identity-registry", type=Path)
    research_replay.add_argument("--identity-registry-sha256")
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
    walk = command(
        "walk-forward",
        _walk_forward,
        "Generate offline analyses on successive simulated portfolio snapshots",
    )
    walk_evidence = command(
        "walk-forward-evidence",
        _study_evidence,
        "Create daily NAV evidence from a frozen offline study",
    )
    walk_evidence.add_argument("protocol", type=Path)
    walk_evidence.add_argument("--protocol-sha256", required=True)
    walk_evidence.add_argument("--identity-registry", type=Path)
    ablation = command(
        "ablation-evidence",
        _study_evidence,
        "Compare frozen offline strategy variants on paired NAV dates",
    )
    ablation.add_argument("protocol", type=Path)
    ablation.add_argument("--protocol-sha256", required=True)
    ablation.add_argument("--identity-registry", type=Path)
    for study in (walk, walk_evidence, ablation):
        study.add_argument("plan", type=Path)
        study.add_argument("tape", type=Path)
        study.add_argument("--research-python", required=True, type=Path)
        study.add_argument("--archive-root", required=True, type=Path)
    walk.add_argument("--plan-sha256", required=True)
    walk.add_argument("--tape-sha256", required=True)
    walk.add_argument("--identity-registry", type=Path)
    walk.add_argument("--identity-registry-sha256")
    walk.add_argument(
        "--include-replay-input",
        action="store_true",
        help="Include the derived native replay input",
    )
    benchmark = command("benchmarks", _benchmarks, "Compare baselines on a local quote fixture")
    benchmark.add_argument("input", type=Path)
    benchmark.add_argument("--start-at", type=datetime.fromisoformat, required=True)
    benchmark.add_argument("--buy-and-hold-symbol", required=True)
    benchmark.add_argument("--trend-symbol")
    benchmark.add_argument("--trend-lookback-quotes", type=int)
    benchmark.add_argument("--trend-max-gap-seconds", type=int)
    benchmark.add_argument("--rebalance-at", action="append", type=parse_rebalance_at)
    benchmark.add_argument("--rebalance-phase-seconds", type=int)
    benchmark.add_argument("--rebalance-max-mark-age-seconds", type=int)
    statistics_schema = command(
        "statistics-schema", _statistics_schema, "Write a statistics JSON schema"
    )
    statistics_schema.add_argument("document", choices=("protocol", "evidence", "operating-costs"))
    freeze = command(
        "freeze-statistics", _freeze_statistics, "Print the digest of a local protocol"
    )
    freeze.add_argument("protocol", type=Path)
    forward_config = command(
        "forward-config-sha256", _forward_config, "Hash the local strategy configuration bundle"
    )
    forward_config.add_argument("plan", type=Path)
    forward_config.add_argument("--root", type=Path, required=True)
    forward_freeze = command(
        "freeze-forward",
        _freeze_forward,
        "Write canonical freeze JSON for a local strategy and protocol",
    )
    forward_freeze.add_argument("plan", type=Path)
    forward_freeze.add_argument("protocol", type=Path)
    forward_freeze.add_argument("--root", type=Path, required=True)
    forward_verify = command(
        "verify-forward", _verify_forward, "Verify a pinned forward freeze against local files"
    )
    forward_verify.add_argument("freeze", type=Path)
    forward_verify.add_argument("protocol", type=Path)
    forward_verify.add_argument("--root", type=Path, required=True)
    forward_verify.add_argument("--freeze-sha256", required=True)
    forward_paper = command(
        "forward-paper-report",
        _forward_paper,
        "Evaluate a registered paper checkpoint from pinned exports",
    )
    forward_paper.add_argument("freeze", type=Path)
    forward_paper.add_argument("protocol", type=Path)
    forward_paper.add_argument("--root", type=Path, required=True)
    forward_paper.add_argument("--freeze-sha256", required=True)
    forward_paper.add_argument("--through", type=date.fromisoformat, required=True)
    forward_paper.add_argument("--nav", type=Path, required=True)
    forward_paper.add_argument("--nav-sha256", required=True)
    forward_paper.add_argument("--benchmark", type=Path)
    forward_paper.add_argument("--benchmark-sha256")
    forward_paper.add_argument("--operating-costs", type=Path)
    forward_paper.add_argument("--operating-costs-sha256")
    forward_paper.add_argument("--previous-receipt", type=Path)
    forward_paper.add_argument("--previous-receipt-sha256")
    command(
        "forward-checkpoint-schema", _schema, "Write the forward checkpoint receipt schema"
    ).set_defaults(contract=CheckpointReceipt)
    forward_power = command(
        "forward-paper-power",
        _forward_power,
        "Review the forward paper statistical design without NAV",
    )
    forward_power.add_argument("protocol", type=Path)
    freeze_costs = command(
        "freeze-operating-costs",
        _freeze_costs,
        "Print the digest of a local USD operating cost ledger",
    )
    freeze_costs.add_argument("ledger", type=Path)
    command(
        "research-rates-schema", _schema, "Write the local model rate schedule schema"
    ).set_defaults(contract=ResearchRates)
    command(
        "research-permit-schema", _schema, "Write the durable call permit archive schema"
    ).set_defaults(contract=ResearchPermitArchive)
    research_costs = command(
        "research-costs",
        _research_costs,
        "Value archived research calls under a pinned rate schedule",
    )
    research_costs.add_argument("--rates", nargs=2, metavar=("FILE", "SHA256"), required=True)
    research_costs.add_argument(
        "--usage", nargs=2, metavar=("FILE", "SHA256"), action="append", default=[]
    )
    research_costs.add_argument(
        "--permit-archive", nargs=2, metavar=("FILE", "SHA256"), action="append", default=[]
    )
    research_costs.add_argument("--synthetic-opex-template", nargs=2, metavar=("FILE", "SHA256"))
    research_costs.add_argument("--series", help="NAV series charged the synthetic LLM estimates")
    statistics = command("statistics", _statistics, "Audit and evaluate a frozen local NAV study")
    statistics.add_argument("protocol", type=Path)
    statistics.add_argument("evidence", type=Path)
    statistics.add_argument("--protocol-sha256", required=True)
    statistics.add_argument("--evidence-sha256", help="Verify the exact bytes of a retained report")
    statistics.add_argument(
        "--operating-costs", type=Path, help="Cost ledger pinned by protocol 0.2"
    )
    replay_evidence = command(
        "replay-evidence",
        _replay_evidence,
        "Derive daily statistics evidence from a frozen local replay",
    )
    replay_evidence.add_argument("protocol", type=Path)
    replay_evidence.add_argument("input", type=Path)
    replay_evidence.add_argument("--protocol-sha256", required=True)
    replay_evidence.add_argument("--research-export", type=Path)
    replay_evidence.add_argument("--identity-registry", type=Path)
    paper_evidence = command(
        "paper-evidence",
        _paper_evidence,
        "Derive statistics evidence from a pinned paper NAV report",
    )
    paper_evidence.add_argument("protocol", type=Path)
    paper_evidence.add_argument("input", type=Path)
    paper_evidence.add_argument("--protocol-sha256", required=True)
    paper_evidence.add_argument("--benchmark-input", type=Path)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        result = args.run(args)
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"Evaluation failed: {exc}", file=sys.stderr)
        return 2
    if isinstance(result, str):
        # The canonical freeze is pinned by its exact stdout bytes.
        sys.stdout.write(result)
    else:
        print(json.dumps(result, indent=2, sort_keys=True))
    return 0


def _read(path: Path, limit: int = MAX_INPUT_BYTES) -> bytes:
    # Read one extra byte so the document decoder can reject an oversized input.
    with path.open("rb") as source:
        return source.read(limit + 1)


def _doctor(_: argparse.Namespace) -> dict:
    lock = Path("uv.lock")
    return {
        "evaluation_version": version("tradingbot-evaluation"),
        "nautilus_version": version("nautilus-trader"),
        "replay_schema_version": REPLAY_VERSION,
        "evaluation_lock_sha256": sha256(lock.read_bytes()).hexdigest() if lock.is_file() else None,
    }


def _schema(args: argparse.Namespace) -> dict:
    return _document_schema(args.contract)


def _statistics_schema(args: argparse.Namespace) -> dict:
    return _document_schema(
        {
            "protocol": EvaluationProtocol,
            "evidence": EvaluationEvidence,
            "operating-costs": OperatingCosts,
        }[args.document]
    )


def _document_schema(contract: type) -> dict:
    return {"$schema": "https://json-schema.org/draft/2020-12/schema"} | msgspec.json.schema(
        contract
    )


def _forward_config(args: argparse.Namespace) -> dict:
    plan = load_forward_plan(_read(args.plan))
    return {"configuration_sha256": forward_configuration_sha256(args.root, plan)}


def _freeze_forward(args: argparse.Namespace) -> str:
    plan = load_forward_plan(_read(args.plan))
    protocol = load_protocol(_read(args.protocol))
    frozen = freeze_forward(args.root, plan, protocol)
    return msgspec.json.encode(frozen, order="deterministic").decode()


def _verify_forward(args: argparse.Namespace) -> dict:
    protocol = load_protocol(_read(args.protocol))
    frozen = load_forward_freeze(_read(args.freeze))
    verified = verify_forward(args.root, frozen, protocol, expected_sha256=args.freeze_sha256)
    return {
        "verified": True,
        "forward_freeze_sha256": forward_freeze_sha256(verified),
        "protocol_sha256": verified.protocol_sha256,
    }


def _forward_paper(args: argparse.Namespace) -> dict:
    freeze = PinnedArtifact(args.freeze, args.freeze_sha256)
    protocol = load_protocol(_read(args.protocol))
    return evaluate_forward_paper(
        args.root,
        load_forward_freeze(freeze.read("Forward freeze")),
        protocol,
        expected_freeze_sha256=freeze.sha256,
        through=args.through,
        nav=PinnedArtifact(args.nav, args.nav_sha256),
        benchmark=_optional_pin(args.benchmark, args.benchmark_sha256, "Paper benchmark"),
        expenses=_optional_pin(
            args.operating_costs, args.operating_costs_sha256, "Operating cost ledger"
        ),
        previous_receipt=_optional_pin(
            args.previous_receipt, args.previous_receipt_sha256, "Previous checkpoint receipt"
        ),
    )


def _read_pinned_registry(args: argparse.Namespace) -> bytes | None:
    if (args.identity_registry is None) != (args.identity_registry_sha256 is None):
        raise ValueError("Pass both the identity registry and its independently pinned SHA-256")
    return (
        None
        if args.identity_registry is None
        else _read(args.identity_registry, MAX_IDENTITY_REGISTRY_BYTES)
    )


def _research_replay(args: argparse.Namespace) -> dict:
    export = _read(args.export, MAX_RESEARCH_EXPORT_BYTES)
    tape = _read(args.tape)
    pins = {
        "expected_export_sha256": args.export_sha256,
        "expected_tape_sha256": args.tape_sha256,
        "require_snapshot_continuity": args.require_snapshot_continuity,
        "identity_registry_raw": _read_pinned_registry(args),
        "expected_identity_registry_sha256": args.identity_registry_sha256,
    }
    if args.emit_replay_input:
        return prepare_research_replay(export, tape, **pins).native_input()
    return run_research_replay(export, tape, **pins)


def _registry_reference(
    registry: bytes | None, plan: bytes, plan_digest: str | None
) -> Reference | None:
    if registry is None or plan_digest is None:
        return None
    reference = load_walk_forward_plan(plan, plan_digest).identity_registry
    return None if reference is msgspec.UNSET else reference


def _walk_forward(args: argparse.Namespace) -> dict:
    plan = _read(args.plan)
    tape = _read(args.tape)
    registry = _read_pinned_registry(args)
    return run_walk_forward(
        plan,
        tape,
        infer=local_research(
            args.research_python,
            args.archive_root,
            identity_registry=_registry_reference(registry, plan, args.plan_sha256),
        ),
        expected_plan_sha256=args.plan_sha256,
        expected_tape_sha256=args.tape_sha256,
        identity_registry_raw=registry,
        expected_identity_registry_sha256=args.identity_registry_sha256,
        include_replay_input=args.include_replay_input,
    )


def _study_evidence(args: argparse.Namespace) -> dict:
    plan = _read(args.plan)
    tape = _read(args.tape)
    protocol = load_protocol(_read(args.protocol))
    registry = (
        None
        if args.identity_registry is None
        else _read(args.identity_registry, MAX_IDENTITY_REGISTRY_BYTES)
    )
    is_ablation = args.command == "ablation-evidence"
    source = protocol.ablation_source if is_ablation else protocol.walk_forward_source
    plan_digest = None if source is msgspec.UNSET else source.plan_sha256
    reference = _registry_reference(registry, plan, plan_digest)

    def infer(variant: str | None = None) -> Callable[[bytes], bytes]:
        return local_research(
            args.research_python,
            args.archive_root,
            variant=variant,
            identity_registry=reference,
        )

    if is_ablation:
        evidence = derive_ablation_evidence(
            protocol,
            plan,
            tape,
            expected_protocol_sha256=args.protocol_sha256,
            identity_registry_raw=registry,
            infer_for=infer,
        )
    else:
        evidence = derive_walk_forward_evidence(
            protocol,
            plan,
            tape,
            expected_protocol_sha256=args.protocol_sha256,
            identity_registry_raw=registry,
            infer=infer(),
        )
    return msgspec.to_builtins(evidence)


def _freeze_costs(args: argparse.Namespace) -> dict:
    return {
        "operating_costs_sha256": operating_costs_sha256(load_operating_costs(_read(args.ledger)))
    }


def _research_costs(args: argparse.Namespace) -> dict:
    rate_path, rate_digest = args.rates
    result = value_research_costs(
        PinnedArtifact(Path(rate_path), rate_digest),
        tuple(PinnedArtifact(Path(path), digest) for path, digest in args.usage),
        permit_archives=tuple(
            PinnedArtifact(Path(path), digest) for path, digest in args.permit_archive
        ),
    )
    if (args.synthetic_opex_template is None) != (args.series is None):
        raise ValueError("A synthetic cost template and its NAV series are required together")
    if args.synthetic_opex_template is not None:
        path, digest = args.synthetic_opex_template
        ledger = synthetic_operating_costs(PinnedArtifact(Path(path), digest), args.series, result)
        result["synthetic_operating_costs"] = json.loads(msgspec.json.encode(ledger))
        result["synthetic_opex_template_sha256"] = digest
        result["synthetic_expense_rounding"] = "ceil_to_usd_8_decimals_per_utc_day"
    return result


def _freeze_statistics(args: argparse.Namespace) -> dict:
    return {"protocol_sha256": protocol_sha256(load_protocol(_read(args.protocol)))}


def _forward_power(args: argparse.Namespace) -> dict:
    protocol = load_protocol(_read(args.protocol))
    power = review_forward_power(protocol)
    if power is None:
        raise ValueError("Forward paper power review is not registered in the protocol")
    return {"protocol_sha256": protocol_sha256(protocol), "power_review": power}


def _replay_evidence(args: argparse.Namespace) -> dict:
    protocol = load_protocol(_read(args.protocol))
    raw = _read(args.input)
    has_identity_inputs = args.research_export is not None or args.identity_registry is not None
    stable_source = (
        protocol.replay_source is not msgspec.UNSET
        and protocol.replay_source.identity is not msgspec.UNSET
    )
    if stable_source or has_identity_inputs:
        if args.research_export is None or args.identity_registry is None:
            raise ValueError(
                "Stable replay evidence requires --research-export and --identity-registry"
            )
        evidence = derive_identity_replay_evidence(
            protocol,
            raw,
            _read(args.research_export, MAX_RESEARCH_EXPORT_BYTES),
            _read(args.identity_registry, MAX_IDENTITY_REGISTRY_BYTES),
            expected_protocol_sha256=args.protocol_sha256,
        )
    else:
        evidence = derive_replay_evidence(
            protocol, load_replay(raw), expected_protocol_sha256=args.protocol_sha256
        )
    return json.loads(msgspec.json.encode(evidence))


def _paper_evidence(args: argparse.Namespace) -> dict:
    protocol = load_protocol(_read(args.protocol))
    raw = _read(args.input)
    benchmark = None if args.benchmark_input is None else _read(args.benchmark_input)
    evidence = build_paper_evidence(
        protocol,
        raw,
        expected_protocol_sha256=args.protocol_sha256,
        benchmark_raw=benchmark,
    )
    return json.loads(msgspec.json.encode(evidence))


def _statistics(args: argparse.Namespace) -> dict:
    protocol = load_protocol(_read(args.protocol))
    raw = _read(args.evidence)
    if args.evidence_sha256 is not None and sha256(raw).hexdigest() != args.evidence_sha256:
        raise ValueError("Evidence file SHA-256 differs from the expected digest")
    evidence = load_evidence(raw)
    costs = (
        None if args.operating_costs is None else load_operating_costs(_read(args.operating_costs))
    )
    result = evaluate_statistics(
        protocol,
        evidence,
        expected_protocol_sha256=args.protocol_sha256,
        operating_costs=costs,
    )
    if args.evidence_sha256 is not None:
        result["verification_scope"]["evidence_file_hash_verified"] = True
    return result


def _benchmarks(args: argparse.Namespace) -> dict:
    fixture = load_replay(_read(args.input))
    trend_args = (args.trend_symbol, args.trend_lookback_quotes, args.trend_max_gap_seconds)
    trend = None
    if any(value is not None for value in trend_args):
        if any(value is None for value in trend_args):
            raise ValueError("Trend benchmark requires symbol, lookback quotes and max gap seconds")
        trend = TrendConfig(*trend_args)
    rebalance_args = (
        args.rebalance_at,
        args.rebalance_phase_seconds,
        args.rebalance_max_mark_age_seconds,
    )
    schedule = None
    if any(value is not None for value in rebalance_args):
        if any(value is None for value in rebalance_args):
            raise ValueError(
                "Scheduled equal weight requires cutoff, phase seconds and max mark age"
            )
        schedule = EqualWeightSchedule(tuple(args.rebalance_at), *rebalance_args[1:])
    return run_benchmarks(
        fixture,
        start_at=args.start_at,
        buy_and_hold_symbol=args.buy_and_hold_symbol,
        trend=trend,
        equal_weight_schedule=schedule,
    )


def _replay(args: argparse.Namespace) -> dict:
    return run_replay(load_replay(_read(args.input)), through=args.through)


def _optional_pin(path: Path | None, digest: str | None, description: str) -> PinnedArtifact | None:
    if path is None and digest is None:
        return None
    if path is None or digest is None:
        raise ValueError(f"{description} file and SHA-256 must be supplied together")
    return PinnedArtifact(path, digest)


if __name__ == "__main__":
    raise SystemExit(main())
