"""Commit a local strategy bundle and its evaluation protocol to exact digests."""

import json
import os
import re
import stat
from hashlib import sha256
from pathlib import Path, PureWindowsPath
from typing import Annotated, Literal

import msgspec

from tradingbot_evaluation.replay import MAX_INPUT_BYTES, IntentID, _unique_fields
from tradingbot_evaluation.statistics import SHA256, EvaluationProtocol, protocol_sha256

MAX_ARTIFACT_BYTES = 8 * 1024 * 1024
MAX_BUNDLE_BYTES = 64 * 1024 * 1024
MAX_ARTIFACTS = 128

ARTIFACT_PATH_PATTERN = r"^[a-zA-Z0-9_.-]+(?:/[a-zA-Z0-9_.-]+)*$"
ArtifactPath = Annotated[
    str,
    msgspec.Meta(
        max_length=240,
        pattern=ARTIFACT_PATH_PATTERN,
    ),
]
ArtifactGroup = Annotated[tuple[ArtifactPath, ...], msgspec.Meta(min_length=1, max_length=8)]
ModelID = Annotated[str, msgspec.Meta(pattern=r"^[a-zA-Z0-9][a-zA-Z0-9_.:/@-]{0,191}$")]


class StrategyFiles(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    strategy: ArtifactGroup
    prompts: ArtifactGroup
    model: ArtifactGroup
    universe: ArtifactGroup
    data: ArtifactGroup
    risk: ArtifactGroup
    schedule: ArtifactGroup
    memory: ArtifactGroup
    scorer: ArtifactGroup
    costs: ArtifactGroup
    runtime: ArtifactGroup


class ForwardStrategy(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    model_id: ModelID
    files: StrategyFiles


class ForwardPlan(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    experiment_id: IntentID
    mode: Literal["paper", "shadow"]
    primary_strategy: IntentID
    change_policy: Literal["new_run_for_any_change"]
    insufficient_power: Literal["continue_in_current_mode"]
    strategies: Annotated[
        dict[IntentID, ForwardStrategy], msgspec.Meta(min_length=1, max_length=16)
    ]


class Artifact(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    path: ArtifactPath
    sha256: SHA256
    size_bytes: Annotated[int, msgspec.Meta(ge=0, le=MAX_ARTIFACT_BYTES)]


class ForwardConfiguration(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    plan: ForwardPlan
    artifacts: Annotated[tuple[Artifact, ...], msgspec.Meta(min_length=1, max_length=MAX_ARTIFACTS)]


class ForwardFreeze(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    configuration: ForwardConfiguration
    configuration_sha256: SHA256
    protocol_sha256: SHA256
    primary_comparison_id: IntentID
    comparison_ids: Annotated[tuple[IntentID, ...], msgspec.Meta(min_length=1, max_length=16)]


def _load(data: bytes, contract: type):
    if len(data) > MAX_INPUT_BYTES:
        raise ValueError("Forward freeze document exceeds the input limit")
    return msgspec.convert(json.loads(data, object_pairs_hook=_unique_fields), type=contract)


def load_forward_plan(data: bytes) -> ForwardPlan:
    plan = _load(data, ForwardPlan)
    _artifact_paths(plan)
    return plan


def load_forward_freeze(data: bytes) -> ForwardFreeze:
    frozen = _load(data, ForwardFreeze)
    paths = tuple(artifact.path for artifact in frozen.configuration.artifacts)
    if paths != _artifact_paths(frozen.configuration.plan):
        raise ValueError("Frozen artifact paths must match the configuration exactly and in order")
    if _digest(frozen.configuration) != frozen.configuration_sha256:
        raise ValueError("Forward freeze configuration SHA-256 does not match its contents")
    if (
        frozen.comparison_ids != tuple(sorted(set(frozen.comparison_ids)))
        or frozen.primary_comparison_id not in frozen.comparison_ids
    ):
        raise ValueError("Invalid frozen comparison IDs")
    return frozen


def _artifact_paths(plan: ForwardPlan) -> tuple[str, ...]:
    if plan.primary_strategy not in plan.strategies:
        raise ValueError("The primary strategy must name a registered strategy")
    paths = set()
    for strategy in plan.strategies.values():
        for group in msgspec.structs.astuple(strategy.files):
            if len(group) != len(set(group)):
                raise ValueError(
                    "Configuration artifact paths must be unique within each file group"
                )
            for path in group:
                if not re.fullmatch(ARTIFACT_PATH_PATTERN, path) or any(
                    part in {".", ".."} or part.endswith(".") or PureWindowsPath(part).is_reserved()
                    for part in path.split("/")
                ):
                    raise ValueError("Invalid relative configuration artifact path")
            paths.update(group)
    if len(paths) > MAX_ARTIFACTS or len({path.casefold() for path in paths}) != len(paths):
        raise ValueError("Too many configuration artifact paths or paths collide on case")
    return tuple(sorted(paths))


def _file_identity(info: os.stat_result) -> tuple[int, ...]:
    # Windows may provide a different creation time through stat and fstat.
    return info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns


def _read_artifact(root: Path, path: str) -> Artifact:
    try:
        target = (root / path).resolve(strict=True)
        if not target.is_relative_to(root):
            raise ValueError(
                f"Configuration artifact must be inside the configuration root: {path}"
            )
        before = target.stat()
        if not stat.S_ISREG(before.st_mode):
            raise ValueError(f"Configuration artifact must be a regular file: {path}")
        if before.st_size > MAX_ARTIFACT_BYTES:
            raise ValueError(f"Configuration artifact size exceeds the limit: {path}")
        with target.open("rb") as reader:
            opened = os.fstat(reader.fileno())
            if _file_identity(before) != _file_identity(opened):
                raise ValueError(f"Configuration artifact changed during verification: {path}")
            data = reader.read(MAX_ARTIFACT_BYTES + 1)
            after = os.fstat(reader.fileno())
        if len(data) > MAX_ARTIFACT_BYTES:
            raise ValueError(f"Configuration artifact size exceeds the limit: {path}")
        if (
            len(data) != before.st_size
            or _file_identity(opened) != _file_identity(after)
            or opened.st_ctime_ns != after.st_ctime_ns
            or _file_identity(before) != _file_identity(target.stat())
        ):
            raise ValueError(f"Configuration artifact changed during verification: {path}")
    except (OSError, RuntimeError) as exc:
        raise ValueError(f"Cannot read configuration artifact: {path}") from exc
    return Artifact(path, sha256(data).hexdigest(), len(data))


def _capture_configuration(root: Path, plan: ForwardPlan) -> ForwardConfiguration:
    root = root.resolve()
    if not root.is_dir():
        raise ValueError("Forward configuration root must be a directory")
    artifacts = []
    total_bytes = 0
    for path in _artifact_paths(plan):
        artifact = _read_artifact(root, path)
        total_bytes += artifact.size_bytes
        if total_bytes > MAX_BUNDLE_BYTES:
            raise ValueError("Configuration bundle exceeds the total size limit")
        artifacts.append(artifact)
    return ForwardConfiguration(plan, tuple(artifacts))


def _digest(value: ForwardConfiguration | ForwardFreeze) -> str:
    return sha256(msgspec.json.encode(value, order="deterministic")).hexdigest()


def forward_configuration_sha256(root: Path, plan: ForwardPlan) -> str:
    return _digest(_capture_configuration(root, plan))


def forward_freeze_sha256(frozen: ForwardFreeze) -> str:
    return _digest(frozen)


def freeze_forward(root: Path, plan: ForwardPlan, protocol: EvaluationProtocol) -> ForwardFreeze:
    configuration = _capture_configuration(root, plan)
    configuration_sha256 = _digest(configuration)
    if protocol.configuration_sha256 != configuration_sha256:
        raise ValueError("Evaluation protocol configuration SHA-256 differs from this bundle")
    if protocol.experiment_id != plan.experiment_id:
        raise ValueError("Evaluation protocol and forward configuration name different experiments")
    registered = {row.strategy for row in protocol.comparisons}
    if protocol.ablation_source is not msgspec.UNSET:
        registered.add(protocol.ablation_source.baseline)
    if registered != set(plan.strategies):
        raise ValueError("Evaluation protocol must cover all and only the registered strategies")
    comparisons = {row.comparison_id: row.strategy for row in protocol.comparisons}
    if comparisons.get(protocol.primary_comparison_id) != plan.primary_strategy:
        raise ValueError("Evaluation protocol primary comparison must use the primary strategy")
    return ForwardFreeze(
        schema_version="0.1",
        configuration=configuration,
        configuration_sha256=configuration_sha256,
        protocol_sha256=protocol_sha256(protocol),
        primary_comparison_id=protocol.primary_comparison_id,
        comparison_ids=tuple(sorted(comparisons)),
    )


def verify_forward(
    root: Path,
    frozen: ForwardFreeze,
    protocol: EvaluationProtocol,
    *,
    expected_sha256: str,
) -> ForwardFreeze:
    """Verify content against an independently retained digest and local inputs."""
    if expected_sha256 != forward_freeze_sha256(frozen):
        raise ValueError("Manifest differs from the expected forward freeze SHA-256")
    if protocol_sha256(protocol) != frozen.protocol_sha256:
        raise ValueError("Supplied protocol differs from the frozen evaluation protocol SHA-256")
    recreated = freeze_forward(root, frozen.configuration.plan, protocol)
    if recreated != frozen:
        raise ValueError("Local configuration and protocol do not reproduce the forward freeze")
    return recreated
