"""Evaluate a registered paper checkpoint against separately pinned observations."""

from dataclasses import dataclass
from datetime import date, timedelta
from fractions import Fraction
from hashlib import sha256
from pathlib import Path

import msgspec

from tradingbot_evaluation.checkpoint_history import load_checkpoint_receipt, record_checkpoint
from tradingbot_evaluation.forward_freeze import ForwardFreeze, verify_forward
from tradingbot_evaluation.operating_costs import load_operating_costs, operating_costs_sha256
from tradingbot_evaluation.paper_evidence import (
    build_paper_evidence_with_cost_diagnostics,
    load_paper_benchmark,
)
from tradingbot_evaluation.power_review import forward_model_interval, review_forward_power
from tradingbot_evaluation.regime_review import review_forward_regimes
from tradingbot_evaluation.replay import MAX_INPUT_BYTES
from tradingbot_evaluation.statistics import (
    GAUSSIAN_PAIRED_MEAN,
    EvaluationProtocol,
    evaluate_statistics,
    protocol_sha256,
)


@dataclass(frozen=True)
class PinnedArtifact:
    path: Path
    sha256: str

    def read(self, description: str) -> bytes:
        with self.path.open("rb") as source:
            raw = source.read(MAX_INPUT_BYTES + 1)
        if len(raw) > MAX_INPUT_BYTES or sha256(raw).hexdigest() != self.sha256:
            raise ValueError(f"{description} SHA-256 or file size does not match")
        return raw


def evaluate_forward_paper(
    root: Path,
    frozen: ForwardFreeze,
    protocol: EvaluationProtocol,
    *,
    expected_freeze_sha256: str,
    through: date,
    nav: PinnedArtifact,
    benchmark: PinnedArtifact | None = None,
    expenses: PinnedArtifact | None = None,
    previous_receipt: PinnedArtifact | None = None,
) -> dict:
    registration = protocol.paper_forward_source
    if registration is msgspec.UNSET:
        raise ValueError("A registered forward paper source is required")
    verify_forward(root, frozen, protocol, expected_sha256=expected_freeze_sha256)
    if through not in registration.checkpoints_utc:
        raise ValueError("The requested UTC checkpoint is outside the registration")
    history_required = registration.checkpoint_history is not msgspec.UNSET
    checkpoint_index = registration.checkpoints_utc.index(through)
    needs_previous = history_required and checkpoint_index > 0
    if needs_previous and previous_receipt is None:
        raise ValueError("The registered policy requires the previous checkpoint receipt")
    if not needs_previous and previous_receipt is not None:
        raise ValueError("This checkpoint does not accept a previous checkpoint receipt")
    previous = None
    if previous_receipt is not None:
        previous = load_checkpoint_receipt(previous_receipt.read("Previous checkpoint receipt"))
        if (
            previous.freeze_sha256 != expected_freeze_sha256
            or previous.checkpoint_utc != registration.checkpoints_utc[checkpoint_index - 1]
            or (previous.previous_receipt_sha256 is None) != (checkpoint_index == 1)
        ):
            raise ValueError(
                "Receipt does not identify the immediately previous registered checkpoint"
            )
    if (benchmark is None) != (registration.benchmark is msgspec.UNSET):
        raise ValueError("A benchmark file and SHA-256 are required only for a registered index")
    if expenses is not None and protocol.nav_basis != "operating_net":
        raise ValueError("Operating costs require a registered operating_net study")
    costs = None
    if expenses is not None:
        costs = load_operating_costs(expenses.read("Operating cost ledger"))
        kind = (
            "synthetic_fixture" if protocol.source_mode == "synthetic_fixture" else "archived_local"
        )
        if costs.source_mode != kind:
            raise ValueError("Operating cost ledger source does not match the registration")

    # The checkpoint protocol is derived exactly once from the registration and artifact pins.
    # It supports the same arithmetic and validations as an existing protocol 0.2.
    checkpoint = msgspec.structs.replace(
        protocol,
        paper_forward_source=msgspec.UNSET,
        paper_nav_sha256=nav.sha256,
        paper_benchmark_sha256=benchmark.sha256 if benchmark is not None else msgspec.UNSET,
        operating_costs_sha256=operating_costs_sha256(costs)
        if costs is not None
        else msgspec.UNSET,
        nav_basis="operating_net" if costs is not None else "execution_net",
        expected_nav_dates=tuple(day for day in protocol.expected_nav_dates if day <= through),
        splits=msgspec.structs.replace(protocol.splits, end_exclusive=through + timedelta(days=1)),
    )
    checkpoint_digest = protocol_sha256(checkpoint)
    nav_raw = nav.read("Paper NAV")
    benchmark_raw = benchmark.read("Paper benchmark") if benchmark is not None else None
    evidence, cost_diagnostics = build_paper_evidence_with_cost_diagnostics(
        checkpoint,
        nav_raw,
        expected_protocol_sha256=checkpoint_digest,
        benchmark_raw=benchmark_raw,
        forward_source=registration,
    )
    result = evaluate_statistics(
        checkpoint, evidence, expected_protocol_sha256=checkpoint_digest, operating_costs=costs
    )
    if history_required:
        receipt = record_checkpoint(
            freeze_sha256=expected_freeze_sha256,
            through=through,
            nav_raw=nav_raw,
            benchmark_raw=benchmark_raw,
            costs=costs,
            costs_sha256=expenses.sha256 if expenses is not None else None,
            previous=previous,
            previous_receipt_sha256=(
                previous_receipt.sha256 if previous_receipt is not None else None
            ),
        )
        result["checkpoint_receipt"] = msgspec.to_builtins(receipt)
    result["execution_cost_diagnostics"] = cost_diagnostics
    power = review_forward_power(protocol)
    if power is not None:
        result["power_status"] = (
            "conditionally_simulated" if power["status"] == "simulated" else "unavailable"
        )
        result["power_review"] = power
    comparison_basis = result["comparisons_nav_basis"]
    basis_available = protocol.nav_basis == comparison_basis
    comparison_nav = (
        tuple(day["nav_usd"] for day in result["operating_costs"]["daily_nav"])
        if comparison_basis == "operating_net"
        else tuple(day.nav_usd for day in evidence.daily_nav)
    )
    primary = next(
        row
        for row in result["comparisons"]
        if row["comparison_id"] == protocol.primary_comparison_id
    )
    drawdown_limit_exceeded = None
    if basis_available:
        marks = (day[primary["strategy"]] for day in comparison_nav)
        retention = Fraction(10_000 - registration.maximum_drawdown_bps, 10_000)
        peak = Fraction()
        drawdown_limit_exceeded = False
        for value in marks:
            mark = Fraction(value)
            peak = max(peak, mark)
            if mark < peak * retention:
                drawdown_limit_exceeded = True
                break
    final = through == protocol.expected_nav_dates[-1]
    if power is not None and power["method"] == GAUSSIAN_PAIRED_MEAN:
        nav_marks = None
        if not final:
            reason = "final_checkpoint_required"
        elif not basis_available:
            reason = "registered_nav_basis_unavailable"
        else:
            reason = None
            nav_marks = comparison_nav
        result["model_conditional_inference"] = forward_model_interval(
            protocol, nav_marks=nav_marks, unavailable_reason=reason
        )
    regime = registration.regime_review
    if regime is not msgspec.UNSET:
        if benchmark_raw is None:
            raise ValueError("Forward regime review requires the primary total-return index file")
        primary_comparison = next(
            row
            for row in protocol.comparisons
            if row.comparison_id == protocol.primary_comparison_id
        )
        result["regime_review"] = review_forward_regimes(
            protocol,
            regime,
            primary_comparison,
            load_paper_benchmark(benchmark_raw),
            comparison_nav,
            nav_basis=comparison_basis,
            requested_nav_basis_available=basis_available,
        )
    cost_status = "not_requested"
    if protocol.nav_basis == "operating_net":
        cost_status = "missing" if costs is None else result["operating_costs"]["status"]
    result["forward_paper"] = {
        "freeze_sha256": expected_freeze_sha256,
        "registered_protocol_sha256": frozen.protocol_sha256,
        "checkpoint_protocol_sha256": checkpoint_digest,
        "checkpoint_utc": through.isoformat(),
        "period_status": "final" if final else "interim",
        "inference_scope": "registered_final_window" if final else "descriptive_interim",
        "paper_nav_sha256": nav.sha256,
        "benchmark_sha256": benchmark.sha256 if benchmark is not None else None,
        "operating_costs_file_sha256": expenses.sha256 if expenses is not None else None,
        "requested_nav_basis": protocol.nav_basis,
        "requested_nav_basis_available": basis_available,
        "operating_costs_status": cost_status,
        "registered_return_threshold_status": primary["status"]
        if final and basis_available
        else None,
        "maximum_drawdown_bps": registration.maximum_drawdown_bps,
        "drawdown_limit_scope": "all_marks_from_initial_funding",
        "drawdown_limit_exceeded": drawdown_limit_exceeded,
        "status": "drawdown_limit_exceeded" if drawdown_limit_exceeded else "inconclusive",
    }
    return result
