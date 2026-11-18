"""Run all variants on the same dates, initial capital, local archives and quote tape."""

from collections.abc import Callable

import msgspec

from tradingbot_evaluation.replay_evidence import native_daily_nav
from tradingbot_evaluation.statistics import (
    AblationArm,
    AblationProvenance,
    DailyNav,
    EvaluationEvidence,
    EvaluationProtocol,
    ResearchVariant,
    _validate_ablation_evidence,
    _validate_protocol,
    load_evidence,
    protocol_sha256,
)
from tradingbot_evaluation.walk_forward_evidence import reproduce_study, study_plan


def derive_ablation_evidence(
    protocol: EvaluationProtocol,
    plan_raw: bytes,
    tape_raw: bytes,
    *,
    expected_protocol_sha256: str,
    infer_for: Callable[[ResearchVariant], Callable[[bytes], bytes]],
) -> EvaluationEvidence:
    frozen = protocol_sha256(protocol)
    if frozen != expected_protocol_sha256:
        raise ValueError("Frozen protocol SHA-256 differs from the expected digest")
    _validate_protocol(protocol)
    source = protocol.ablation_source
    if source is msgspec.UNSET:
        raise ValueError("A frozen ablation source is required")
    plan = study_plan(protocol, plan_raw, source.plan_sha256)
    planned_calls = sum(step.max_llm_calls for step in plan.steps) * len(source.variants)
    if planned_calls > source.max_total_llm_calls:
        raise ValueError("Ablation variants exceed the aggregate planned call budget")

    marks = {day: {} for day in protocol.expected_nav_dates}
    arms = {}
    for name, variant in sorted(source.variants.items()):
        replay, walk, configs = reproduce_study(
            plan,
            plan_raw,
            tape_raw,
            plan_sha256=source.plan_sha256,
            tape_sha256=source.tape_sha256,
            infer=infer_for(variant),
        )
        arms[name] = AblationArm(variant=variant, walk=walk, research_config_sha256_by_step=configs)
        for row in native_daily_nav(
            protocol,
            replay,
            (),
            strategy=name,
            mark_max_age_seconds=source.mark_max_age_seconds,
        ):
            marks[row.day].update(row.nav_usd)

    provenance = AblationProvenance(method="synthetic_paired_ablation", arms=arms)
    _validate_ablation_evidence(source, provenance)
    evidence = EvaluationEvidence(
        schema_version=protocol.schema_version,
        protocol_sha256=frozen,
        daily_nav=tuple(DailyNav(day=day, nav_usd=nav) for day, nav in marks.items()),
        daily_nav_basis="execution_net",
        ablation_provenance=provenance,
    )
    return load_evidence(msgspec.json.encode(evidence))
