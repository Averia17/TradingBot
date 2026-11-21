"""Derive daily marks from an analysis plan and tape pinned before the run."""

from collections.abc import Callable
from datetime import UTC, datetime, time
from hashlib import sha256

import msgspec

from tradingbot_evaluation.replay import MAX_INPUT_BYTES, Replay, replay_sha256
from tradingbot_evaluation.replay_evidence import (
    MAX_MARK_DAYS,
    native_daily_nav,
    native_replay_provenance,
    resolve_identity_benchmarks,
    validate_identity_quotes,
)
from tradingbot_evaluation.replay_identity import ReplayIdentity
from tradingbot_evaluation.research_replay import _time_ns, load_research_tape
from tradingbot_evaluation.statistics import (
    AblationSource,
    EvaluationEvidence,
    EvaluationProtocol,
    WalkForwardProvenance,
    WalkForwardSource,
    load_evidence,
    verify_frozen_protocol,
)
from tradingbot_evaluation.walk_forward import (
    WalkForwardPlan,
    _run_walk_forward,
    load_walk_forward_plan,
)


def derive_walk_forward_evidence(
    protocol: EvaluationProtocol,
    plan_raw: bytes,
    tape_raw: bytes,
    *,
    expected_protocol_sha256: str,
    infer: Callable[[bytes], bytes],
    identity_registry_raw: bytes | None = None,
) -> EvaluationEvidence:
    frozen = verify_frozen_protocol(protocol, expected_protocol_sha256)
    source = protocol.walk_forward_source
    if source is msgspec.UNSET:
        raise ValueError("A frozen walk-forward source is required")
    plan = study_plan(protocol, plan_raw, source.plan_sha256)
    tape, identity = study_tape(source, plan, tape_raw, identity_registry_raw)
    benchmarks = source.benchmarks
    if identity is not None:
        benchmarks = resolve_identity_benchmarks(benchmarks, identity)
    replay, provenance, _ = reproduce_study(
        plan,
        tape,
        identity,
        source=source,
        infer=infer,
    )
    evidence = EvaluationEvidence(
        schema_version=protocol.schema_version,
        protocol_sha256=frozen,
        daily_nav=native_daily_nav(
            protocol,
            replay,
            benchmarks,
            mark_max_age_seconds=source.mark_max_age_seconds,
            identity=identity,
        ),
        daily_nav_basis="execution_net",
        walk_forward_provenance=provenance,
    )
    return load_evidence(msgspec.json.encode(evidence))


def study_tape(
    source: WalkForwardSource | AblationSource,
    plan: WalkForwardPlan,
    tape_raw: bytes,
    registry_raw: bytes | None,
) -> tuple[Replay, ReplayIdentity | None]:
    registry_sha = source.identity_registry_sha256
    has_registry = registry_sha is not msgspec.UNSET
    if has_registry != (registry_raw is not None) or has_registry != (
        plan.identity_registry is not msgspec.UNSET
    ):
        raise ValueError(
            "Stable study evidence requires the registry, plan and frozen registry pin"
        )
    if (
        plan.identity_registry is not msgspec.UNSET
        and plan.identity_registry.sha256 != registry_sha
    ):
        raise ValueError("Walk-forward plan registry SHA-256 differs from the frozen protocol")
    if len(tape_raw) > MAX_INPUT_BYTES:
        raise ValueError("Walk-forward tape exceeds the input limit")
    if sha256(tape_raw).hexdigest() != source.tape_sha256:
        raise ValueError("Walk-forward tape SHA-256 differs from the frozen protocol")
    tape, identity = load_research_tape(
        tape_raw, registry_raw, registry_sha if has_registry else None
    )
    if identity is not None:
        if identity.registry != plan.identity_registry:
            raise ValueError("Walk-forward plan and tape registry references differ")
        validate_identity_quotes(tape, identity)
    return tape, identity


def study_plan(
    protocol: EvaluationProtocol, plan_raw: bytes, expected_plan_sha256: str
) -> WalkForwardPlan:
    if len(protocol.expected_nav_dates) > MAX_MARK_DAYS:
        raise ValueError("Walk-forward date count exceeds the daily replay bound")
    plan = load_walk_forward_plan(plan_raw, expected_plan_sha256)
    if plan.experiment_id != protocol.experiment_id:
        raise ValueError("Walk-forward plan and protocol name different experiments")
    first = datetime.combine(protocol.expected_nav_dates[0], time.max, tzinfo=UTC)
    last = datetime.combine(protocol.expected_nav_dates[-1], time.max, tzinfo=UTC)
    if _time_ns(plan.steps[0].as_of) < _time_ns(first.isoformat()) or _time_ns(
        plan.steps[-1].completed_at
    ) > _time_ns(last.isoformat()):
        raise ValueError("Walk-forward steps must lie within the frozen NAV window")
    return plan


def reproduce_study(
    plan: WalkForwardPlan,
    tape: Replay,
    identity: ReplayIdentity | None,
    *,
    source: WalkForwardSource | AblationSource,
    infer: Callable[[bytes], bytes],
) -> tuple[Replay, WalkForwardProvenance, tuple[str, ...]]:
    prepared, report = _run_walk_forward(
        plan,
        tape,
        identity,
        plan_hash=source.plan_sha256,
        tape_hash=source.tape_sha256,
        infer=infer,
    )
    replay = prepared.replay
    digest = replay_sha256(replay)
    if digest != report["normalized_input_sha256"]:
        raise RuntimeError("Walk-forward produced a different normalized replay")
    steps = report["research_steps"]
    steps_digest = sha256(msgspec.json.encode(steps, order="deterministic")).hexdigest()
    config_digests = tuple(
        step["research_metadata"]["provenance"]["config_sha256"] for step in steps
    )
    provenance = WalkForwardProvenance(
        method="synthetic_walk_forward",
        plan_sha256=source.plan_sha256,
        tape_sha256=source.tape_sha256,
        research_steps_sha256=steps_digest,
        step_count=len(plan.steps),
        llm_calls_used=report["llm_calls_used"],
        max_total_llm_calls=plan.max_total_llm_calls,
        replay=native_replay_provenance(digest),
        identity_registry_sha256=source.identity_registry_sha256,
    )
    return replay, provenance, config_digests
