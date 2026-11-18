"""Run a chronological synthetic study with a new offline analysis at every cutoff."""

import os
import subprocess
from collections.abc import Callable
from decimal import Decimal
from hashlib import sha256
from pathlib import Path
from typing import Annotated, Literal
from uuid import NAMESPACE_URL, uuid5

import msgspec

from tradingbot_evaluation.replay import (
    MAX_INPUT_BYTES,
    MAX_PREFIX_EVENTS,
    MAX_PREFIX_SYMBOLS,
    replay_event_count,
)
from tradingbot_evaluation.replay_identity import (
    DisplaySymbol,
    Reference,
    SecurityBinding,
)
from tradingbot_evaluation.research_replay import (
    _AMOUNT,
    _BUDGET,
    _ID,
    _NATIVE_SYMBOL,
    _RECEIPT_TIME,
    _at,
    _decode,
    _floor_microseconds,
    _Manifest,
    _midpoint,
    _pinned,
    _prepare_records,
    _Record,
    _reference_quote,
    _Result,
    _tape_timeline,
    _time_ns,
    load_research_tape,
    replay_account_at,
)

MAX_STEP_RESULT_BYTES = 2 * 1024 * 1024
MAX_STEPS = 32


class WalkForwardStep(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    as_of: _RECEIPT_TIME
    completed_at: _RECEIPT_TIME
    requested_symbols: Annotated[
        tuple[DisplaySymbol, ...], msgspec.Meta(min_length=1, max_length=8)
    ]
    max_llm_calls: _BUDGET
    data_manifest: _Manifest
    security_bindings: (
        Annotated[tuple[SecurityBinding, ...], msgspec.Meta(min_length=1, max_length=8)]
        | msgspec.UnsetType
    ) = msgspec.UNSET


class WalkForwardPlan(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1", "0.2"]
    source_mode: Literal["synthetic_fixture"]
    experiment_id: _ID
    policy_version: _ID
    max_position_pct: _AMOUNT
    max_total_llm_calls: Annotated[int, msgspec.Meta(ge=1, le=2048)]
    steps: Annotated[tuple[WalkForwardStep, ...], msgspec.Meta(min_length=1, max_length=MAX_STEPS)]
    identity_registry: Reference | msgspec.UnsetType = msgspec.UNSET


def _validate_plan(plan: WalkForwardPlan) -> None:
    if sum(step.max_llm_calls for step in plan.steps) > plan.max_total_llm_calls:
        raise ValueError("Walk-forward planned calls exceed the total budget")
    if not 0 < Decimal(plan.max_position_pct) <= 1:
        raise ValueError("Walk-forward position cap must be positive and at most one")
    stable = plan.schema_version == "0.2"
    pin = plan.identity_registry
    if stable != (pin is not msgspec.UNSET) or pin is not msgspec.UNSET and not pin.valid_path():
        raise ValueError("Walk-forward plan identity version and registry reference differ")
    previous_completion = -1
    for step in plan.steps:
        cutoff, completion = _time_ns(step.as_of), _time_ns(step.completed_at)
        if not previous_completion < cutoff <= completion:
            raise ValueError("Walk-forward steps must be in a non-overlapping chronological order")
        if len(step.requested_symbols) != len(set(step.requested_symbols)):
            raise ValueError("Walk-forward step has duplicate symbols")
        bindings = step.security_bindings
        if stable:
            if (
                cutoff % 1000
                or completion % 1000
                or bindings is msgspec.UNSET
                or tuple(item.symbol for item in bindings) != step.requested_symbols
                or len({item.security_id for item in bindings}) != len(bindings)
            ):
                raise ValueError(
                    "Identity walk-forward requires exact clocks and ordered security bindings"
                )
        elif bindings is not msgspec.UNSET or any(
            not _NATIVE_SYMBOL.fullmatch(symbol) for symbol in step.requested_symbols
        ):
            raise ValueError(
                "Legacy walk-forward requires native symbols without identity bindings"
            )
        previous_completion = completion


def load_walk_forward_plan(plan_raw: bytes, expected_sha256: str) -> WalkForwardPlan:
    if len(plan_raw) > MAX_INPUT_BYTES:
        raise ValueError("Walk-forward plan exceeds the input limit")
    _pinned(plan_raw, expected_sha256, "walk-forward plan")
    plan = _decode(plan_raw, WalkForwardPlan)
    _validate_plan(plan)
    return plan


def walk_forward_plan_schema() -> dict:
    schema = msgspec.json.schema(WalkForwardPlan)
    definition = schema["$defs"]["WalkForwardPlan"]
    definition["if"] = {"properties": {"schema_version": {"const": "0.2"}}}
    definition["then"] = {
        "required": ["identity_registry"],
        "properties": {"steps": {"items": {"required": ["security_bindings"]}}},
    }
    definition["else"] = {
        "not": {"required": ["identity_registry"]},
        "properties": {"steps": {"items": {"not": {"required": ["security_bindings"]}}}},
    }
    return {"$schema": "https://json-schema.org/draft/2020-12/schema"} | schema


def local_research(
    python: Path,
    archive_root: Path,
    *,
    variant: str | None = None,
    identity_registry: Reference | None = None,
) -> Callable[[bytes], bytes]:
    executable = python.absolute()
    archive = archive_root.resolve(strict=True)
    if not executable.is_file() or not archive.is_dir():
        raise ValueError(
            "Walk-forward requires an installed research Python and local archive root"
        )
    # Child needs the local interpreter, Git and temporary storage. Exclude
    # provider keys, tracing settings, proxies and custom Python import paths.
    supported = {
        "APPDATA",
        "HOMEDRIVE",
        "HOMEPATH",
        "LOCALAPPDATA",
        "PATH",
        "SYSTEMROOT",
        "TEMP",
        "TMP",
        "TMPDIR",
        "USERPROFILE",
        "WINDIR",
    }
    env = {key: value for key, value in os.environ.items() if key.upper() in supported}
    command = [
        str(executable),
        "-I",
        "-m",
        "tradingbot",
        "historical-step",
        "--archive-root",
        str(archive),
    ]
    if identity_registry is not None:
        command.extend(
            [
                "--identity-registry-ref",
                identity_registry.ref,
                "--identity-registry-sha256",
                identity_registry.sha256,
            ]
        )

    def infer(payload: bytes) -> bytes:
        if variant is not None:
            payload = msgspec.json.encode(msgspec.json.decode(payload) | {"variant": variant})
        try:
            response = subprocess.run(
                command,
                input=payload,
                capture_output=True,
                check=False,
                timeout=120,
                env=env,
            )
        except subprocess.TimeoutExpired as exc:
            raise RuntimeError(
                "Offline historical research exceeded its wall-clock deadline"
            ) from exc
        if response.returncode:
            raise RuntimeError(
                f"Offline historical research command failed ({response.returncode})"
            )
        return response.stdout

    return infer


def run_walk_forward(
    plan_raw: bytes,
    tape_raw: bytes,
    *,
    infer: Callable[[bytes], bytes],
    expected_plan_sha256: str,
    expected_tape_sha256: str,
    include_replay_input: bool = False,
    identity_registry_raw: bytes | None = None,
    expected_identity_registry_sha256: str | None = None,
) -> dict:
    plan = load_walk_forward_plan(plan_raw, expected_plan_sha256)
    if len(tape_raw) > MAX_INPUT_BYTES:
        raise ValueError("Walk-forward tape exceeds the input limit")
    plan_hash = expected_plan_sha256
    tape_hash = _pinned(tape_raw, expected_tape_sha256, "tape")
    tape, identity = load_research_tape(
        tape_raw, identity_registry_raw, expected_identity_registry_sha256
    )
    if (identity is None) != (plan.schema_version == "0.1") or (
        identity is not None and identity.registry != plan.identity_registry
    ):
        raise ValueError("Walk-forward plan and synthetic tape identity pins differ")
    if identity is not None:
        for step in plan.steps:
            cutoff = _floor_microseconds(_time_ns(step.as_of))
            for binding in step.security_bindings:
                identity.resolve(binding, cutoff)
    timeline, splits = _tape_timeline(tape)
    event_bound = replay_event_count(tape) + sum(len(step.requested_symbols) for step in plan.steps)
    if (
        event_bound > MAX_PREFIX_EVENTS
        or len({quote.symbol for quote in tape.quotes}) > MAX_PREFIX_SYMBOLS
    ):
        raise ValueError("Walk-forward tape and planned orders exceed the replay bounds")

    records: list[_Record] = []
    portfolios = []
    replay = tape
    calls_used = 0
    study_id = uuid5(NAMESPACE_URL, f"tradingbot:walk-forward:{plan_hash}:{tape_hash}")
    for index, step in enumerate(plan.steps):
        as_of = _floor_microseconds(_time_ns(step.as_of))
        cash, held = replay_account_at(replay, as_of)
        positions = []
        for native_key, quantity in sorted(held.items()):
            symbol, instrument_id, mark_floor = native_key, f"fixture:{native_key}", None
            if identity is not None:
                symbol, active = identity.describe_holding(native_key, as_of)
                instrument_id, mark_floor = (
                    active.mapping.instrument.instrument_id,
                    active.effective_at,
                )
            mark = _reference_quote(
                timeline[native_key], splits[native_key], as_of, mark_floor=mark_floor
            )
            if mark is None:
                raise ValueError(f"Walk-forward held position {symbol} lacks a point-in-time mark")
            positions.append(
                {
                    "symbol": symbol,
                    "instrument_id": instrument_id,
                    "quantity": quantity,
                    "mark_price_usd": format(_midpoint(mark).normalize(), "f"),
                }
            )
        portfolio = {
            "snapshot_id": str(uuid5(study_id, f"snapshot:{index}")),
            "as_of": _at(as_of),
            "currency": "USD",
            "cash_asset": "USD",
            "cash_usd": cash,
            "reserved_cash_usd": "0",
            "max_position_pct": plan.max_position_pct,
            "positions": positions,
        }
        job = {
            "job_schema_version": plan.schema_version,
            "job_id": str(uuid5(study_id, f"job:{index}")),
            "portfolio": portfolio,
            "requested_symbols": step.requested_symbols,
            "policy_version": plan.policy_version,
            "data_manifest": msgspec.to_builtins(step.data_manifest),
            "max_llm_calls": step.max_llm_calls,
        }
        if identity is not None:
            job["identity_registry"] = msgspec.to_builtins(identity.registry)
            job["security_bindings"] = msgspec.to_builtins(step.security_bindings)
        result_raw = infer(msgspec.json.encode({"job": job, "completed_at": step.completed_at}))
        if len(result_raw) > MAX_STEP_RESULT_BYTES:
            raise ValueError("Walk-forward research result exceeds the per-step input limit")
        result = _decode(result_raw, _Result)
        result_version = "0.3" if identity is not None else "0.2"
        if result.result_schema_version != result_version or (
            _time_ns(result.batch.created_at) != _time_ns(step.completed_at)
        ):
            raise ValueError("Walk-forward requires an offline graph result on the simulated clock")
        request_raw = msgspec.json.encode(job)
        records.append(
            _Record(
                job["job_id"],
                1,
                _at(as_of),
                step.completed_at,
                request_raw,
                sha256(request_raw).hexdigest(),
                result_raw,
                sha256(result_raw).hexdigest(),
            )
        )
        portfolios.append(portfolio)
        calls_used += result.metadata.llm_calls
        # Reuse the stored-decision validator and sizing path. Keep the
        # in-memory simulated timestamps outside the durable export protocol.
        prepared = _prepare_records(
            tuple(records),
            tape,
            plan_hash,
            tape_hash,
            require_snapshot_continuity=identity is not None or index == len(plan.steps) - 1,
            identity=identity,
        )
        replay = prepared.replay

    evaluation = prepared.validated_evaluation
    if evaluation is None:
        raise RuntimeError("Walk-forward continuity was not verified")
    if identity is not None:
        horizon = _floor_microseconds(int(evaluation["evaluated_through_ns"]))
        for key in evaluation["open_positions"]:
            try:
                _, active = identity.describe_holding(key, horizon)
            except ValueError as exc:
                raise ValueError(
                    f"Walk-forward terminal valuation has an ineligible holding {key}"
                ) from exc
            if (
                _reference_quote(
                    timeline[key], splits[key], horizon, mark_floor=active.effective_at
                )
                is None
            ):
                raise ValueError(
                    f"Walk-forward terminal valuation has no eligible mark for holding {key}"
                )
    report = evaluation | {
        "walk_forward_schema_version": plan.schema_version,
        "experiment_id": plan.experiment_id,
        "clock_source": "simulated_plan",
        "plan_sha256": plan_hash,
        "synthetic_tape_sha256": tape_hash,
        "intent_pipeline_connected": True,
        "snapshot_continuity_verified": True,
        "max_total_llm_calls": plan.max_total_llm_calls,
        "llm_calls_used": calls_used,
        "research_steps": [
            meta | {"portfolio": portfolio}
            for meta, portfolio in zip(prepared.jobs, portfolios, strict=True)
        ],
        "decisions": [
            row | {"status": evaluation["intents"].get(row["decision_id"], row["status"])}
            for row in prepared.decisions
        ],
    }
    if identity is not None:
        report["identity_registry"] = msgspec.to_builtins(identity.registry)
        report["replay_instruments"] = msgspec.to_builtins(identity.instruments)
    if include_replay_input:
        report["native_replay_input"] = (
            prepared.native_input() if identity else msgspec.to_builtins(prepared.replay)
        )
    return report
