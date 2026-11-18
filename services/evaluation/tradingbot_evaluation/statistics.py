"""Audit frozen splits and compare synchronized local daily USD NAV marks."""

import json
from datetime import UTC, date, datetime, time, timedelta
from decimal import ROUND_CEILING, ROUND_FLOOR, Decimal, localcontext
from fractions import Fraction
from hashlib import sha256
from itertools import pairwise
from math import ceil, floor, fsum
from random import Random
from typing import Annotated, Literal

import msgspec

from tradingbot_evaluation.operating_costs import USD, OperatingCosts, apply_operating_costs
from tradingbot_evaluation.replay import (
    MAX_INPUT_BYTES,
    IntentID,
    Ticker,
    Timestamp,
    _unique_fields,
)
from tradingbot_evaluation.trend import TrendLookbackQuotes, TrendMaxGapSeconds

SHA256 = Annotated[str, msgspec.Meta(pattern=r"^[a-f0-9]{64}$")]
SplitName = Literal["train", "validation", "test"]
MAX_BOOTSTRAP_DRAWS = 10_000_000
GAUSSIAN_PAIRED_MEAN = "gaussian_ar1_paired_mean_v1"
CASH_SERIES = "uninvested-cash"


class SplitWindows(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    train_start: date
    validation_start: date
    test_start: date
    end_exclusive: date
    embargo_calendar_days: Annotated[int, msgspec.Meta(ge=0, le=366)]


class BlockBootstrap(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    block_marks: Annotated[int, msgspec.Meta(ge=2, le=252)]
    minimum_blocks: Annotated[int, msgspec.Meta(ge=2, le=1000)]
    replications: Annotated[int, msgspec.Meta(ge=100, le=10_000)]
    seed: Annotated[int, msgspec.Meta(ge=0, le=2**53 - 1)]
    family_alpha_bps: Annotated[int, msgspec.Meta(ge=1, le=2500)]


class Comparison(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    comparison_id: IntentID
    strategy: IntentID
    benchmark: IntentID
    benchmark_access: Literal["synthetic_fixture", "theoretical_reference", "unknown"]
    minimum_daily_excess_bps: Annotated[int, msgspec.Meta(ge=0, le=10_000)]


class ReplayBenchmark(msgspec.Struct, frozen=True, forbid_unknown_fields=True, omit_defaults=True):
    series_id: IntentID
    kind: Literal["cash", "buy_and_hold", "equal_weight_once", "trend_momentum"]
    symbol: Ticker | None = None
    lookback_quotes: TrendLookbackQuotes | None = None
    max_gap_seconds: TrendMaxGapSeconds | None = None


class ReplaySource(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    replay_sha256: SHA256
    benchmarks: Annotated[tuple[ReplayBenchmark, ...], msgspec.Meta(min_length=1, max_length=4)]


class WalkForwardSource(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    plan_sha256: SHA256
    tape_sha256: SHA256
    mark_max_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]
    benchmarks: Annotated[tuple[ReplayBenchmark, ...], msgspec.Meta(min_length=1, max_length=4)]


ResearchVariant = Literal["full_graph", "no_memory", "single_analyst", "factual_reflection"]


class AblationSource(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    plan_sha256: SHA256
    tape_sha256: SHA256
    mark_max_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]
    max_total_llm_calls: Annotated[int, msgspec.Meta(ge=1, le=8192)]
    baseline: IntentID
    variants: Annotated[dict[IntentID, ResearchVariant], msgspec.Meta(min_length=2, max_length=4)]


PaperAccount = Annotated[
    str, msgspec.Meta(pattern=r"^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
]


class PaperMarkSource(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    kind: Literal["synthetic_fixture", "local_archive"]
    id: IntentID


class ForwardPaperBenchmark(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    series_id: IntentID
    modeled_entry_cost_bps: Annotated[int, msgspec.Meta(ge=0, le=9999)]
    max_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]
    archive_source_id: IntentID | msgspec.UnsetType = msgspec.UNSET


class ForwardPaperPowerReview(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    method: Literal["gaussian_ar1_paired_moving_block_percentile_v1", "gaussian_ar1_paired_mean_v1"]
    alternative_increment_bps: Annotated[int, msgspec.Meta(ge=1, le=10_000)]
    daily_excess_sd_bps: Annotated[int, msgspec.Meta(ge=1, le=10_000)]
    lag_one_correlation_bps: Annotated[int, msgspec.Meta(ge=-9999, le=9999)]
    target_detection_bps: Annotated[int, msgspec.Meta(ge=1, le=9999)]
    trials: Annotated[int, msgspec.Meta(ge=100, le=2000)]
    seed: Annotated[int, msgspec.Meta(ge=0, le=2**53 - 1)]


class ForwardRegimeReview(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    lookback_utc_days: Annotated[int, msgspec.Meta(ge=1, le=365)]
    minimum_distinct_observations: Annotated[int, msgspec.Meta(ge=2, le=366)]
    rising_threshold_bps: Annotated[int, msgspec.Meta(ge=1, le=10_000)]
    declining_threshold_bps: Annotated[int, msgspec.Meta(ge=1, le=9999)]
    minimum_pairs_per_regime: Annotated[int, msgspec.Meta(ge=1, le=365)]


class ForwardPaperSource(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    account_id: PaperAccount
    initial_cash_usd: USD
    valuation_source: PaperMarkSource
    mark_max_age_seconds: Annotated[int, msgspec.Meta(ge=1, le=604800)]
    checkpoints_utc: Annotated[tuple[date, ...], msgspec.Meta(min_length=1, max_length=366)]
    maximum_drawdown_bps: Annotated[int, msgspec.Meta(ge=0, le=10_000)]
    benchmark: ForwardPaperBenchmark | msgspec.UnsetType = msgspec.UNSET
    power_review: ForwardPaperPowerReview | msgspec.UnsetType = msgspec.UNSET
    checkpoint_history: Literal["require_prior_receipt"] | msgspec.UnsetType = msgspec.UNSET
    regime_review: ForwardRegimeReview | msgspec.UnsetType = msgspec.UNSET


class EvaluationProtocol(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1", "0.2"]
    experiment_id: IntentID
    configuration_sha256: SHA256
    source_mode: Literal["synthetic_fixture", "retrospective"]
    model_training_cutoff: date | None
    nav_basis: Literal["gross", "execution_net", "operating_net"]
    splits: SplitWindows
    expected_nav_dates: Annotated[tuple[date, ...], msgspec.Meta(min_length=2, max_length=4096)]
    primary_comparison_id: IntentID
    comparisons: Annotated[tuple[Comparison, ...], msgspec.Meta(min_length=1, max_length=16)]
    bootstrap: BlockBootstrap
    replay_source: ReplaySource | msgspec.UnsetType = msgspec.UNSET
    operating_costs_sha256: SHA256 | msgspec.UnsetType = msgspec.UNSET
    paper_nav_sha256: SHA256 | msgspec.UnsetType = msgspec.UNSET
    walk_forward_source: WalkForwardSource | msgspec.UnsetType = msgspec.UNSET
    paper_benchmark_sha256: SHA256 | msgspec.UnsetType = msgspec.UNSET
    ablation_source: AblationSource | msgspec.UnsetType = msgspec.UNSET
    paper_forward_source: ForwardPaperSource | msgspec.UnsetType = msgspec.UNSET


class DailyNav(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    day: date
    nav_usd: dict[IntentID, USD]


class DecisionInput(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    reference_id: IntentID
    kind: Literal["market_observation", "factual_memory"]
    observed_at: Timestamp
    available_at: Timestamp


class DecisionWindow(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    decision_id: IntentID
    as_of: Timestamp
    label_end: Timestamp
    label_available_at: Timestamp
    inputs: tuple[DecisionInput, ...] = ()


class ReplayProvenance(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    method: Literal["nautilus_prefix"]
    normalized_replay_sha256: SHA256
    nautilus_version: Annotated[str, msgspec.Meta(min_length=1, max_length=32)]
    mark_policy: Literal["last_known_bid_at_utc_day_end"]
    execution_costs: Literal["bid_ask_and_simulated_commission"]
    external_costs_usd: None


class PaperNavProvenance(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    method: Literal["paper_ledger_export"]
    paper_nav_sha256: SHA256
    account_id: IntentID
    paper_benchmark_sha256: SHA256 | msgspec.UnsetType = msgspec.UNSET
    paper_benchmark_archive_source_id: IntentID | msgspec.UnsetType = msgspec.UNSET


class WalkForwardProvenance(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    method: Literal["synthetic_walk_forward"]
    plan_sha256: SHA256
    tape_sha256: SHA256
    research_steps_sha256: SHA256
    step_count: Annotated[int, msgspec.Meta(ge=1, le=32)]
    llm_calls_used: Annotated[int, msgspec.Meta(ge=1, le=2048)]
    max_total_llm_calls: Annotated[int, msgspec.Meta(ge=1, le=2048)]
    replay: ReplayProvenance


class AblationArm(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    variant: ResearchVariant
    walk: WalkForwardProvenance
    research_config_sha256_by_step: Annotated[
        tuple[SHA256, ...], msgspec.Meta(min_length=1, max_length=32)
    ]


class AblationProvenance(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    method: Literal["synthetic_paired_ablation"]
    arms: Annotated[dict[IntentID, AblationArm], msgspec.Meta(min_length=2, max_length=4)]


class EvaluationEvidence(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1", "0.2"]
    protocol_sha256: SHA256
    daily_nav: Annotated[tuple[DailyNav, ...], msgspec.Meta(min_length=2, max_length=4096)]
    decisions: tuple[DecisionWindow, ...] = ()
    replay_provenance: ReplayProvenance | msgspec.UnsetType = msgspec.UNSET
    daily_nav_basis: Literal["execution_net"] | msgspec.UnsetType = msgspec.UNSET
    paper_nav_provenance: PaperNavProvenance | msgspec.UnsetType = msgspec.UNSET
    walk_forward_provenance: WalkForwardProvenance | msgspec.UnsetType = msgspec.UNSET
    ablation_provenance: AblationProvenance | msgspec.UnsetType = msgspec.UNSET


def _decode(data: bytes, contract):
    if len(data) > MAX_INPUT_BYTES:
        raise ValueError("Statistics input exceeds the input limit")
    return msgspec.convert(json.loads(data, object_pairs_hook=_unique_fields), type=contract)


def load_protocol(data: bytes) -> EvaluationProtocol:
    protocol = _decode(data, EvaluationProtocol)
    _validate_protocol(protocol)
    return protocol


def load_evidence(data: bytes) -> EvaluationEvidence:
    evidence = _decode(data, EvaluationEvidence)
    _validate_evidence_basis(evidence)
    return evidence


def _validate_evidence_basis(evidence: EvaluationEvidence) -> None:
    if evidence.schema_version == "0.2" and evidence.daily_nav_basis != "execution_net":
        raise ValueError("Evidence 0.2 requires unadjusted execution_net daily NAV")
    if evidence.schema_version == "0.1" and (
        evidence.daily_nav_basis is not msgspec.UNSET
        or evidence.paper_nav_provenance is not msgspec.UNSET
        or evidence.walk_forward_provenance is not msgspec.UNSET
        or evidence.ablation_provenance is not msgspec.UNSET
    ):
        raise ValueError("Evidence daily_nav_basis and new source provenance require 0.2")
    walk = evidence.walk_forward_provenance
    if walk is not msgspec.UNSET and walk.llm_calls_used > walk.max_total_llm_calls:
        raise ValueError("Walk-forward evidence exceeds the declared call budget")


def _validate_protocol(protocol: EvaluationProtocol) -> None:
    has_costs = protocol.operating_costs_sha256 is not msgspec.UNSET
    has_paper_nav = protocol.paper_nav_sha256 is not msgspec.UNSET
    has_paper_benchmark = protocol.paper_benchmark_sha256 is not msgspec.UNSET
    has_walk = protocol.walk_forward_source is not msgspec.UNSET
    has_ablation = protocol.ablation_source is not msgspec.UNSET
    forward = protocol.paper_forward_source
    has_forward = forward is not msgspec.UNSET
    if protocol.schema_version == "0.2" and (
        protocol.nav_basis == "gross"
        or has_costs != (protocol.nav_basis == "operating_net" and not has_forward)
    ):
        raise ValueError(
            "Protocol 0.2 requires a frozen cost ledger SHA-256 only for operating_net"
        )
    if protocol.schema_version == "0.1" and (
        has_costs or has_paper_nav or has_walk or has_paper_benchmark or has_ablation or has_forward
    ):
        raise ValueError(
            "Operating cost ledger, paper NAV and study source pins require protocol 0.2"
        )
    if has_paper_nav and protocol.replay_source is not msgspec.UNSET:
        raise ValueError("Paper NAV export and replay source are mutually exclusive")
    if has_walk and (has_paper_nav or protocol.replay_source is not msgspec.UNSET):
        raise ValueError("Walk-forward, paper NAV and replay sources are mutually exclusive")
    if has_ablation and (has_walk or has_paper_nav or protocol.replay_source is not msgspec.UNSET):
        raise ValueError("Paired ablation requires its own frozen source")
    if has_paper_benchmark and not has_paper_nav:
        raise ValueError("Paper benchmark requires a frozen paper NAV export")
    if has_forward and (
        has_paper_nav or has_walk or has_ablation or protocol.replay_source is not msgspec.UNSET
    ):
        raise ValueError("Forward paper registration cannot pin another source or finished results")
    windows = protocol.splits
    if (
        not windows.train_start
        < windows.validation_start
        < windows.test_start
        < windows.end_exclusive
    ):
        raise ValueError("Split boundaries must be in chronological order")
    embargo = timedelta(days=windows.embargo_calendar_days)
    if embargo >= min(
        windows.test_start - windows.validation_start, windows.end_exclusive - windows.test_start
    ):
        raise ValueError("Embargo must leave both validation and test windows open")
    days = protocol.expected_nav_dates
    if (
        days[0] != windows.train_start
        or days[-1] >= windows.end_exclusive
        or days[-1] < windows.test_start + embargo
        or any(day >= following for day, following in zip(days[:-1], days[1:], strict=True))
    ):
        raise ValueError(
            "Declared NAV dates must be ordered and cover initial funding and the test window"
        )
    ids = {row.comparison_id for row in protocol.comparisons}
    pairs = {(row.strategy, row.benchmark) for row in protocol.comparisons}
    if len(ids) != len(protocol.comparisons) or len(pairs) != len(ids):
        raise ValueError("Registered comparison IDs and strategy-benchmark pairs must be unique")
    if protocol.primary_comparison_id not in ids or any(
        strategy == baseline for strategy, baseline in pairs
    ):
        raise ValueError("Primary comparison must be registered against a distinct benchmark")
    if protocol.replay_source is not msgspec.UNSET:
        _validate_replay_source(protocol, protocol.replay_source)
    if has_walk or has_ablation:
        if days[-1] != windows.end_exclusive - timedelta(days=1) or any(
            right - left != timedelta(days=1) for left, right in pairwise(days)
        ):
            raise ValueError("Walk-forward requires every UTC day through the end of the window")
    if has_walk:
        _validate_replay_source(protocol, protocol.walk_forward_source)
    if has_ablation:
        source = protocol.ablation_source
        if (
            protocol.source_mode != "synthetic_fixture"
            or source.baseline not in source.variants
            or len(set(source.variants.values())) != len(source.variants)
            or pairs
            != {
                (challenger, source.baseline)
                for challenger in source.variants
                if challenger != source.baseline
            }
            or any(row.benchmark_access != "synthetic_fixture" for row in protocol.comparisons)
        ):
            raise ValueError(
                "Paired ablation must compare every distinct variant with its baseline"
            )
    if has_paper_benchmark and (
        not paper_benchmark_access_matches(protocol)
        or len({row.strategy for row in protocol.comparisons}) != 1
        or len({row.benchmark for row in protocol.comparisons} - {CASH_SERIES}) != 1
    ):
        raise ValueError("Paper benchmark requires one eligible noncash reference and one strategy")
    if has_forward:
        _validate_forward_paper(protocol, forward, windows.test_start + embargo)


def paper_benchmark_access_matches(protocol: EvaluationProtocol) -> bool:
    if protocol.source_mode == "synthetic_fixture":
        return all(row.benchmark_access == "synthetic_fixture" for row in protocol.comparisons)
    return all(
        row.benchmark == CASH_SERIES or row.benchmark_access in {"theoretical_reference", "unknown"}
        for row in protocol.comparisons
    )


def _validate_forward_paper(
    protocol: EvaluationProtocol, source: ForwardPaperSource, test_start: date
) -> None:
    days = protocol.expected_nav_dates
    checkpoints = source.checkpoints_utc
    if (
        len(days) > 366
        or days[-1] != protocol.splits.end_exclusive - timedelta(days=1)
        or any(right - left != timedelta(days=1) for left, right in pairwise(days))
        or checkpoints != tuple(sorted(set(checkpoints)))
        or checkpoints[0] <= test_start
        or checkpoints[-1] != days[-1]
    ):
        raise ValueError("Forward paper requires a complete UTC calendar and ordered checkpoints")
    synthetic = protocol.source_mode == "synthetic_fixture"
    if Decimal(source.initial_cash_usd) <= 0 or synthetic != (
        source.valuation_source.kind == "synthetic_fixture"
    ):
        raise ValueError("Forward paper funding and valuation source must match the registration")
    comparisons = protocol.comparisons
    benchmarks = {row.benchmark for row in comparisons}
    configured = source.benchmark
    expected_noncash = set() if configured is msgspec.UNSET else {configured.series_id}
    if (
        len({row.strategy for row in comparisons}) != 1
        or len(comparisons) != len(benchmarks)
        or not paper_benchmark_access_matches(protocol)
        or benchmarks - {CASH_SERIES} != expected_noncash
        or (
            configured is not msgspec.UNSET
            and (configured.archive_source_id is msgspec.UNSET) != synthetic
        )
    ):
        raise ValueError("Forward paper requires one strategy and registered benchmark settings")
    regime = source.regime_review
    if regime is not msgspec.UNSET:
        primary = next(
            row for row in comparisons if row.comparison_id == protocol.primary_comparison_id
        )
        if primary.benchmark not in expected_noncash:
            raise ValueError(
                "Forward regime review requires a registered primary total-return index"
            )
        if regime.minimum_distinct_observations > regime.lookback_utc_days + 1:
            raise ValueError("Forward regime review requires a feasible observation minimum")
    design = source.power_review
    if design is not msgspec.UNSET and design.method != GAUSSIAN_PAIRED_MEAN:
        n = (days[-1] - test_start).days
        config = protocol.bootstrap
        if design.trials * config.replications * ceil(Fraction(n, config.block_marks)) > (
            MAX_BOOTSTRAP_DRAWS
        ):
            raise ValueError("Power review exceeds the local work limit")


def _validate_replay_source(
    protocol: EvaluationProtocol, source: ReplaySource | WalkForwardSource
) -> None:
    if protocol.source_mode != "synthetic_fixture" or (
        protocol.schema_version == "0.1" and protocol.nav_basis != "execution_net"
    ):
        raise ValueError(
            "Replay evidence requires a synthetic_fixture with execution_net input marks"
        )
    if len({row.strategy for row in protocol.comparisons}) != 1:
        raise ValueError("Replay evidence requires one strategy across comparisons")
    benchmark_ids = {row.benchmark for row in protocol.comparisons}
    if (
        {mapping.series_id for mapping in source.benchmarks} != benchmark_ids
        or len(benchmark_ids) != len(source.benchmarks)
        or len({mapping.kind for mapping in source.benchmarks}) != len(source.benchmarks)
        or any(row.benchmark_access != "synthetic_fixture" for row in protocol.comparisons)
    ):
        raise ValueError(
            "Replay benchmark mapping must cover each registered synthetic series once"
        )
    for mapping in source.benchmarks:
        if (mapping.kind in {"buy_and_hold", "trend_momentum"}) != (mapping.symbol is not None):
            raise ValueError(
                "The buy_and_hold and trend_momentum replay benchmarks require a symbol"
            )
        if mapping.kind == "trend_momentum":
            if mapping.lookback_quotes is None or mapping.max_gap_seconds is None:
                raise ValueError(
                    "The trend_momentum replay benchmark requires lookback and max gap"
                )
        elif mapping.lookback_quotes is not None or mapping.max_gap_seconds is not None:
            raise ValueError(
                "Only the trend_momentum replay benchmark accepts lookback and max gap"
            )


def protocol_sha256(protocol: EvaluationProtocol) -> str:
    return sha256(msgspec.json.encode(protocol, order="deterministic")).hexdigest()


def _validate_ablation_evidence(
    source: AblationSource | msgspec.UnsetType, evidence: AblationProvenance | msgspec.UnsetType
) -> None:
    if source is msgspec.UNSET:
        if evidence is not msgspec.UNSET:
            raise ValueError("Ablation provenance requires a frozen ablation source")
        return
    if evidence is msgspec.UNSET or evidence.arms.keys() != source.variants.keys():
        raise ValueError("Ablation evidence must cover every frozen arm")
    if any(
        arm.variant != source.variants[key]
        or arm.walk.plan_sha256 != source.plan_sha256
        or arm.walk.tape_sha256 != source.tape_sha256
        or arm.walk.llm_calls_used > arm.walk.max_total_llm_calls
        or len(arm.research_config_sha256_by_step) != arm.walk.step_count
        for key, arm in evidence.arms.items()
    ):
        raise ValueError(
            "Ablation evidence differs from the frozen variants, inputs or call limits"
        )
    walks = [arm.walk for arm in evidence.arms.values()]
    if len({(walk.step_count, walk.max_total_llm_calls) for walk in walks}) != 1:
        raise ValueError("Ablation arms must use identical step counts and call limits")
    if sum(walk.llm_calls_used for walk in walks) > source.max_total_llm_calls:
        raise ValueError("Ablation exceeded the aggregate call budget")
    if len({arm.research_config_sha256_by_step for arm in evidence.arms.values()}) != len(walks):
        raise ValueError("Ablation requires distinct research configurations for every arm")


def _split_audit(windows: SplitWindows, decisions: tuple[DecisionWindow, ...]) -> dict:
    retained: dict[SplitName, list[str]] = {"train": [], "validation": [], "test": []}
    excluded = []
    seen = set()
    embargo = timedelta(days=windows.embargo_calendar_days)
    for decision in sorted(decisions, key=lambda row: (row.as_of, row.decision_id)):
        day = decision.as_of.astimezone(UTC).date()
        if decision.decision_id in seen or not windows.train_start <= day < windows.end_exclusive:
            raise ValueError(
                "Decision IDs must be unique and decision times must be in the frozen windows"
            )
        seen.add(decision.decision_id)
        if not decision.as_of < decision.label_end <= decision.label_available_at:
            raise ValueError(
                "Decision label must end after the decision and be available no earlier than its end"
            )
        references = {item.reference_id for item in decision.inputs}
        if len(references) != len(decision.inputs):
            raise ValueError("Decision input references must be unique")
        if any(
            not item.observed_at <= item.available_at <= decision.as_of for item in decision.inputs
        ):
            raise ValueError("Decision references future or inconsistently timestamped evidence")

        if day < windows.validation_start:
            split, first, until = "train", windows.train_start, windows.validation_start
        elif day < windows.test_start:
            split, first, until = (
                "validation",
                windows.validation_start + embargo,
                windows.test_start,
            )
        else:
            split, first, until = "test", windows.test_start + embargo, windows.end_exclusive
        boundary = datetime.combine(until, time.min, tzinfo=UTC)
        if day < first:
            reason = "embargo"
        elif decision.label_end >= boundary:
            reason = "label_horizon_overlap"
        elif decision.label_available_at >= boundary:
            reason = "label_not_available_before_boundary"
        else:
            retained[split].append(decision.decision_id)
            continue
        excluded.append({"decision_id": decision.decision_id, "split": split, "reason": reason})
    return {"retained": retained, "excluded": excluded}


def _aligned_nav(
    protocol: EvaluationProtocol, daily_nav: tuple[DailyNav, ...]
) -> dict[str, list[Decimal]]:
    if tuple(mark.day for mark in daily_nav) != protocol.expected_nav_dates:
        raise ValueError(
            "Daily NAV dates must exactly match the frozen chronological mark schedule"
        )
    series = {name for row in protocol.comparisons for name in (row.strategy, row.benchmark)}
    values = {name: [] for name in sorted(series)}
    for mark in daily_nav:
        if mark.nav_usd.keys() != series:
            raise ValueError(
                "Daily NAV marks must include every registered series and only those series"
            )
        for name in values:
            amount = Decimal(mark.nav_usd[name])
            if amount <= 0:
                raise ValueError("Daily NAV marks must be positive")
            values[name].append(amount)
    if len({nav[0] for nav in values.values()}) != 1:
        raise ValueError("All NAV series must start from the same USD funding")
    return values


def _moving_block_interval(
    differences: tuple[float, ...], config: BlockBootstrap, tail: Fraction
) -> tuple[float, float]:
    """Percentile interval from contiguous overlapping blocks of paired excess returns."""
    n = len(differences)
    block_count, remainder = divmod(n, config.block_marks)
    starts = range(n - config.block_marks + 1)
    blocks = [fsum(differences[start : start + config.block_marks]) for start in starts]
    truncated = (
        [fsum(differences[start : start + remainder]) for start in starts] if remainder else []
    )
    rng = Random(config.seed)
    distribution = []
    for _ in range(config.replications):
        selected = [blocks[rng.randrange(len(starts))] for _ in range(block_count)]
        if remainder:
            selected.append(truncated[rng.randrange(len(starts))])
        distribution.append(fsum(selected) / n)
    distribution.sort()
    return (
        distribution[floor(tail * (len(distribution) - 1))],
        distribution[ceil((1 - tail) * (len(distribution) - 1))],
    )


def _bootstrap_insufficiency(n: int, config: BlockBootstrap, tail: Fraction) -> str | None:
    if n // config.block_marks < config.minimum_blocks:
        return "too_few_complete_blocks"
    if floor(config.replications * tail) == 0:
        return "too_few_corrected_tail_draws"
    return None


def _outward_excess_bounds(low: float, high: float) -> tuple[Decimal, Decimal]:
    with localcontext() as context:
        context.prec = 50
        resolution = Decimal("0.000001")
        return (
            Decimal.from_float(low).quantize(resolution, rounding=ROUND_FLOOR),
            Decimal.from_float(high).quantize(resolution, rounding=ROUND_CEILING),
        )


def _risk_method(dates: tuple[date, ...]) -> dict:
    spacing = [(end - start).days for start, end in pairwise(dates)]
    return {
        "mark_count": len(dates),
        "return_count": len(spacing),
        "calendar_days_per_interval": {"min": min(spacing), "max": max(spacing)}
        if spacing
        else None,
        "return_sample": "adjacent_frozen_marks_equal_weight",
        "return_unit": "basis_points_per_observed_interval",
        "annualized": False,
        "maximum_drawdown": "peak_to_trough_on_observed_marks",
        "volatility": "sample_standard_deviation",
        "downside_deviation": "root_mean_square_of_returns_below_zero_over_all_intervals",
        "minimum_returns_for_risk_ratios": 2,
        "excess_return": "strategy_minus_benchmark_interval_return",
        "relative_wealth": "strategy_nav_divided_by_benchmark_nav",
    }


def _maximum_drawdown_bps(marks: list[Decimal] | tuple[Decimal, ...]) -> str:
    peak = marks[0]
    drawdown = Decimal()
    for mark in marks:
        peak = max(peak, mark)
        drawdown = max(drawdown, (peak - mark) / peak)
    return f"{drawdown * 10_000:.6f}"


def _return_risk(returns: tuple[Decimal, ...]) -> dict[str, str | None]:
    zero = Decimal()
    count = len(returns)
    mean = sum(returns, zero) / count if count else None
    downside = (
        (sum((min(value, zero) ** 2 for value in returns), zero) / count).sqrt() if count else None
    )
    volatility = None
    if count > 1:
        variance = sum(((value - mean) ** 2 for value in returns), zero) / (count - 1)
        volatility = variance.sqrt()
    return {
        "mean_return_bps": f"{mean:.6f}" if mean is not None else None,
        "volatility_bps": f"{volatility:.6f}" if volatility is not None else None,
        "downside_deviation_bps": f"{downside:.6f}" if downside is not None else None,
        "mean_return_per_volatility": f"{mean / volatility:.6f}" if volatility else None,
        "mean_return_per_downside_deviation": f"{mean / downside:.6f}"
        if volatility and downside
        else None,
    }


def _paired_excess_bps(
    strategy_growth: tuple[Decimal, ...], benchmark_growth: tuple[Decimal, ...]
) -> tuple[Decimal, ...]:
    return tuple(
        (strategy - benchmark) * 10_000
        for strategy, benchmark in zip(strategy_growth, benchmark_growth, strict=True)
    )


def _comparison_report(
    row: Comparison,
    navs: dict[str, list[Decimal]],
    start: int,
    config: BlockBootstrap,
    tail: Fraction,
    insufficiency: str | None,
) -> dict:
    strategy, benchmark = (navs[name][start:] for name in (row.strategy, row.benchmark))
    with localcontext() as context:
        context.prec = 50
        strategy_growth = tuple(end / begin for begin, end in pairwise(strategy))
        benchmark_growth = tuple(end / begin for begin, end in pairwise(benchmark))
        excess = _paired_excess_bps(strategy_growth, benchmark_growth)
        differences = tuple(map(float, excess))
        strategy_total = (strategy[-1] / strategy[0] - 1) * 10_000
        benchmark_total = (benchmark[-1] / benchmark[0] - 1) * 10_000
        relative_wealth = tuple(s / b for s, b in zip(strategy, benchmark, strict=True))
        risk = {
            "strategy": _return_risk(tuple((value - 1) * 10_000 for value in strategy_growth))
            | {"max_drawdown_bps": _maximum_drawdown_bps(strategy)},
            "benchmark": _return_risk(tuple((value - 1) * 10_000 for value in benchmark_growth))
            | {"max_drawdown_bps": _maximum_drawdown_bps(benchmark)},
            "excess": _return_risk(excess)
            | {"max_relative_wealth_drawdown_bps": _maximum_drawdown_bps(relative_wealth)},
        }
    interval = None
    status = "inconclusive"
    reason = insufficiency
    if reason is None:
        if max(differences) == min(differences):
            reason = "no_daily_excess_variation"
        else:
            low, high = _moving_block_interval(differences, config, tail)
            if low == high:
                reason = "degenerate_bootstrap_interval"
            else:
                lower, upper = _outward_excess_bounds(low, high)
                interval = {"lower": format(lower, "f"), "upper": format(upper, "f")}
                if lower > row.minimum_daily_excess_bps:
                    status = "above_registered_threshold"
                elif upper < row.minimum_daily_excess_bps:
                    status = "below_registered_threshold"
                else:
                    reason = "interval_intersects_registered_threshold"
    return {
        "comparison_id": row.comparison_id,
        "strategy": row.strategy,
        "benchmark": row.benchmark,
        "benchmark_access_declared": row.benchmark_access,
        "minimum_daily_excess_bps": row.minimum_daily_excess_bps,
        "strategy_total_return_bps": f"{strategy_total:.6f}",
        "benchmark_total_return_bps": f"{benchmark_total:.6f}",
        "mean_daily_excess_bps": f"{fsum(differences) / len(differences):.6f}"
        if differences
        else None,
        "daily_excess_bps_interval": interval,
        "risk": risk,
        "status": status,
        "inconclusive_reason": reason,
    }


def evaluate_statistics(
    protocol: EvaluationProtocol,
    evidence: EvaluationEvidence,
    *,
    expected_protocol_sha256: str,
    operating_costs: OperatingCosts | None = None,
) -> dict:
    frozen = protocol_sha256(protocol)
    if expected_protocol_sha256 != frozen or evidence.protocol_sha256 != frozen:
        raise ValueError("Frozen protocol SHA-256 differs from the expected digest or evidence")
    _validate_protocol(protocol)
    if protocol.paper_forward_source is not msgspec.UNSET:
        raise ValueError("Forward paper registration requires forward-paper-report")
    _validate_evidence_basis(evidence)
    if protocol.schema_version != evidence.schema_version:
        raise ValueError("Evidence and protocol versions must match")
    if protocol.schema_version == "0.1" and operating_costs is not None:
        raise ValueError("An operating cost ledger must be frozen in protocol 0.2")
    provenance = evidence.replay_provenance
    source = protocol.replay_source
    if source is msgspec.UNSET:
        if provenance is not msgspec.UNSET:
            raise ValueError("Replay provenance requires a frozen replay source")
    elif provenance is msgspec.UNSET or provenance.normalized_replay_sha256 != source.replay_sha256:
        raise ValueError("Evidence replay provenance must match the frozen replay SHA-256")
    paper = evidence.paper_nav_provenance
    if protocol.paper_nav_sha256 is msgspec.UNSET:
        if paper is not msgspec.UNSET:
            raise ValueError("Paper NAV provenance requires a frozen paper NAV export")
    elif paper is msgspec.UNSET or paper.paper_nav_sha256 != protocol.paper_nav_sha256:
        raise ValueError("Evidence paper NAV provenance must match the frozen paper NAV SHA-256")
    elif paper.paper_benchmark_sha256 != protocol.paper_benchmark_sha256:
        raise ValueError(
            "Evidence paper benchmark provenance must match the frozen benchmark SHA-256"
        )
    elif (paper.paper_benchmark_archive_source_id is not msgspec.UNSET) != (
        protocol.paper_benchmark_sha256 is not msgspec.UNSET
        and protocol.source_mode == "retrospective"
    ):
        raise ValueError("Evidence paper benchmark requires its archived source identity")
    walk = evidence.walk_forward_provenance
    walk_source = protocol.walk_forward_source
    if walk_source is msgspec.UNSET:
        if walk is not msgspec.UNSET:
            raise ValueError("Walk-forward provenance requires frozen source inputs")
    elif walk is msgspec.UNSET or (
        walk.plan_sha256 != walk_source.plan_sha256 or walk.tape_sha256 != walk_source.tape_sha256
    ):
        raise ValueError(
            "Evidence walk-forward provenance must match the frozen plan and tape hashes"
        )
    _validate_ablation_evidence(protocol.ablation_source, evidence.ablation_provenance)
    split = _split_audit(protocol.splits, evidence.decisions)
    navs = _aligned_nav(protocol, evidence.daily_nav)
    test_start = protocol.splits.test_start + timedelta(days=protocol.splits.embargo_calendar_days)
    dates = protocol.expected_nav_dates
    operating_navs, costs_report = None, None
    if protocol.operating_costs_sha256 is not msgspec.UNSET:
        operating_navs, costs_report = apply_operating_costs(
            dates, navs, protocol.operating_costs_sha256, operating_costs
        )
    anchor = next(index for index, day in enumerate(dates) if day >= test_start)
    n = len(dates) - anchor - 1
    config = protocol.bootstrap
    # A Bonferroni tail adjustment across every registered comparison. Each
    # comparison resamples the same dates with the same seed and block rule.
    family_size = len(protocol.comparisons)
    alpha = Fraction(config.family_alpha_bps, 10_000)
    tail = alpha / (2 * family_size)
    complete_blocks = n // config.block_marks
    tail_draws = floor(config.replications * tail)
    insufficiency = _bootstrap_insufficiency(n, config, tail)
    if insufficiency is None and (
        config.replications
        * ceil(Fraction(n, config.block_marks))
        * family_size
        * (1 + int(operating_navs is not None))
        > MAX_BOOTSTRAP_DRAWS
    ):
        raise ValueError("Bootstrap exceeds the local work limit")

    cutoff = "unknown" if protocol.model_training_cutoff is None else "unverified"
    classification = (
        "synthetic_fixture"
        if protocol.source_mode == "synthetic_fixture"
        else f"retrospective_training_cutoff_{cutoff}"
    )
    result = {
        "statistics_schema_version": protocol.schema_version,
        "experiment_id": protocol.experiment_id,
        "configuration_sha256": protocol.configuration_sha256,
        "protocol_sha256": frozen,
        "evidence_sha256": sha256(msgspec.json.encode(evidence, order="deterministic")).hexdigest(),
        "evidence_classification": classification,
        "model_training_cutoff": protocol.model_training_cutoff.isoformat()
        if protocol.model_training_cutoff
        else None,
        "model_training_cutoff_status": cutoff,
        "clean_out_of_sample": False,
        "alpha_measured": False,
        "power_status": "unmeasured",
        "nav_basis_declared": protocol.nav_basis,
        "funding_model_declared": "single_initial_usd_funding",
        "verification_scope": {
            "nav_calendar": "declared_dates",
            "ledger_provenance_verified": False,
            "costs_verified": False,
            "benchmark_access_verified": False,
            "protocol_freeze_time_verified": False,
        },
        "primary_comparison_id": protocol.primary_comparison_id,
        "split_audit": split,
        "test_window": {
            "anchor_mark_day": dates[anchor].isoformat(),
            "through_mark_day": dates[-1].isoformat(),
            "paired_return_count": n,
            "coverage": "all_declared_nav_marks_present",
        },
        "risk_method": _risk_method(dates[anchor:]),
        "bootstrap": {
            "method": "paired_moving_block_percentile",
            "comparison_correction": "bonferroni",
            "nominal_family_confidence": f"{float(1 - alpha):.6f}",
            "nominal_per_comparison_confidence": f"{float(1 - 2 * tail):.6f}",
            "sample_assumption": "approximately_stationary_excess_returns",
            "sample_assumption_verified": False,
            "block_marks": config.block_marks,
            "complete_blocks": complete_blocks,
            "minimum_complete_blocks": config.minimum_blocks,
            "replications": config.replications,
            "seed": config.seed,
            "corrected_tail_draws_floor": tail_draws,
        },
        "comparisons": [
            _comparison_report(row, navs, anchor, config, tail, insufficiency)
            for row in protocol.comparisons
        ],
    }
    if costs_report is not None:
        result["operating_costs"] = costs_report
        result["execution_net_comparisons"] = result["comparisons"]
        result["comparisons_nav_basis"] = "execution_net"
        result["requested_nav_basis_available"] = operating_navs is not None
        if operating_navs is not None:
            result["comparisons_nav_basis"] = "operating_net"
            result["comparisons"] = [
                _comparison_report(row, operating_navs, anchor, config, tail, insufficiency)
                for row in protocol.comparisons
            ]
    elif protocol.nav_basis == "operating_net":
        # Legacy input retains its historical digest and documented numbers,
        # but gives no separate marks or operating-cost evidence.
        result["comparisons_nav_basis"] = "unverified_input"
        result["requested_nav_basis_available"] = False
        result["operating_costs"] = {
            "status": "unavailable",
            "daily_nav": None,
            "blockers": [{"reason": "legacy_cost_basis_unverified"}],
        }
    elif protocol.schema_version == "0.2":
        result["comparisons_nav_basis"] = "execution_net"
        result["requested_nav_basis_available"] = True
    if provenance is not msgspec.UNSET:
        result["replay_provenance_declared"] = msgspec.structs.asdict(provenance)
    if paper is not msgspec.UNSET:
        result["paper_nav_provenance_declared"] = msgspec.to_builtins(paper)
    if walk is not msgspec.UNSET:
        result["walk_forward_provenance_declared"] = msgspec.to_builtins(walk)
    if evidence.ablation_provenance is not msgspec.UNSET:
        result["ablation_provenance_declared"] = msgspec.to_builtins(evidence.ablation_provenance)
        result["ablation_total_llm_calls"] = sum(
            arm.walk.llm_calls_used for arm in evidence.ablation_provenance.arms.values()
        )
    return result
