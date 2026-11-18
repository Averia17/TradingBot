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
    Ticker,
    replay_event_count,
)
from tradingbot_evaluation.research_replay import (
    _AMOUNT,
    _BUDGET,
    _ID,
    _RECEIPT_TIME,
    _at,
    _decode,
    _floor_microseconds,
    _load_tape,
    _Manifest,
    _midpoint,
    _pinned,
    _prepare_records,
    _Record,
    _reference_quote,
    _Result,
    _tape_timeline,
    _time_ns,
    replay_account_at,
)

MAX_STEP_RESULT_BYTES = 2 * 1024 * 1024
MAX_STEPS = 32


class WalkForwardStep(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    as_of: _RECEIPT_TIME
    completed_at: _RECEIPT_TIME
    requested_symbols: Annotated[tuple[Ticker, ...], msgspec.Meta(min_length=1, max_length=8)]
    max_llm_calls: _BUDGET
    data_manifest: _Manifest


class WalkForwardPlan(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture"]
    experiment_id: _ID
    policy_version: _ID
    max_position_pct: _AMOUNT
    max_total_llm_calls: Annotated[int, msgspec.Meta(ge=1, le=2048)]
    steps: Annotated[tuple[WalkForwardStep, ...], msgspec.Meta(min_length=1, max_length=MAX_STEPS)]


def _validate_plan(plan: WalkForwardPlan) -> None:
    if sum(step.max_llm_calls for step in plan.steps) > plan.max_total_llm_calls:
        raise ValueError("Walk-forward planned calls exceed the total budget")
    if not 0 < Decimal(plan.max_position_pct) <= 1:
        raise ValueError("Walk-forward position cap must be positive and at most one")
    previous_completion = -1
    for step in plan.steps:
        cutoff, completion = _time_ns(step.as_of), _time_ns(step.completed_at)
        if not previous_completion < cutoff <= completion:
            raise ValueError("Walk-forward steps must be in a non-overlapping chronological order")
        if len(step.requested_symbols) != len(set(step.requested_symbols)):
            raise ValueError("Walk-forward step has duplicate symbols")
        previous_completion = completion


def load_walk_forward_plan(plan_raw: bytes, expected_sha256: str) -> WalkForwardPlan:
    if len(plan_raw) > MAX_INPUT_BYTES:
        raise ValueError("Walk-forward plan exceeds the input limit")
    _pinned(plan_raw, expected_sha256, "walk-forward plan")
    plan = _decode(plan_raw, WalkForwardPlan)
    _validate_plan(plan)
    return plan


def local_research(
    python: Path, archive_root: Path, *, variant: str | None = None
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

    def infer(payload: bytes) -> bytes:
        if variant is not None:
            payload = msgspec.json.encode(msgspec.json.decode(payload) | {"variant": variant})
        try:
            response = subprocess.run(
                [
                    str(executable),
                    "-I",
                    "-m",
                    "tradingbot",
                    "historical-step",
                    "--archive-root",
                    str(archive),
                ],
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
) -> dict:
    plan = load_walk_forward_plan(plan_raw, expected_plan_sha256)
    plan_hash = expected_plan_sha256
    tape_hash = _pinned(tape_raw, expected_tape_sha256, "tape")
    tape = _load_tape(tape_raw)
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
        for symbol, quantity in sorted(held.items()):
            mark = _reference_quote(timeline[symbol], splits[symbol], as_of)
            if mark is None:
                raise ValueError(f"Walk-forward held position {symbol} lacks a point-in-time mark")
            positions.append(
                {
                    "symbol": symbol,
                    "instrument_id": f"fixture:{symbol}",
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
            "job_schema_version": "0.1",
            "job_id": str(uuid5(study_id, f"job:{index}")),
            "portfolio": portfolio,
            "requested_symbols": step.requested_symbols,
            "policy_version": plan.policy_version,
            "data_manifest": msgspec.to_builtins(step.data_manifest),
            "max_llm_calls": step.max_llm_calls,
        }
        result_raw = infer(msgspec.json.encode({"job": job, "completed_at": step.completed_at}))
        if len(result_raw) > MAX_STEP_RESULT_BYTES:
            raise ValueError("Walk-forward research result exceeds the per-step input limit")
        result = _decode(result_raw, _Result)
        if result.result_schema_version != "0.2" or (
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
            require_snapshot_continuity=index == len(plan.steps) - 1,
        )
        replay = prepared.replay

    evaluation = prepared.validated_evaluation
    if evaluation is None:
        raise RuntimeError("Walk-forward continuity was not verified")
    report = evaluation | {
        "walk_forward_schema_version": "0.1",
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
    if include_replay_input:
        report["native_replay_input"] = msgspec.to_builtins(prepared.replay)
    return report
