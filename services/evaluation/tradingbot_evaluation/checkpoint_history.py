"""Carry previously published daily observations into the next paper checkpoint."""

import json
from datetime import date
from hashlib import sha256
from typing import Literal

import msgspec

from tradingbot_evaluation.operating_costs import OperatingCosts, _utc_day
from tradingbot_evaluation.paper_evidence import PaperBenchmark, PaperNAV, load_paper_benchmark
from tradingbot_evaluation.replay import _unique_fields
from tradingbot_evaluation.statistics import SHA256


class PrefixCommitment(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    artifact_sha256: SHA256
    prefix_sha256: SHA256


class CheckpointReceipt(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    freeze_sha256: SHA256
    checkpoint_utc: date
    previous_receipt_sha256: SHA256 | None
    nav: PrefixCommitment
    benchmark: PrefixCommitment | None
    operating_costs: PrefixCommitment | None


def load_checkpoint_receipt(raw: bytes) -> CheckpointReceipt:
    return msgspec.convert(
        json.loads(raw, object_pairs_hook=_unique_fields), type=CheckpointReceipt
    )


def _digest(value: dict | PaperBenchmark) -> str:
    return sha256(msgspec.json.encode(value, order="deterministic")).hexdigest()


def _nav_prefix(nav: PaperNAV, through: date) -> str:
    value = msgspec.structs.asdict(nav)
    # Each extended export uses a new marks input; the archived account and
    # its original daily rows are the fields that must carry over.
    del value["input_sha256"]
    value["daily_nav"] = tuple(row for row in nav.daily_nav if row.day <= through)
    return _digest(value)


def _benchmark_prefix(benchmark: PaperBenchmark, through: date) -> str:
    levels = tuple(level for level in benchmark.levels if level.day <= through)
    return _digest(msgspec.structs.replace(benchmark, levels=levels))


def _costs_prefix(costs: OperatingCosts, through: date) -> str:
    # Coverage asserts facts about the export's whole interval. A category can
    # become nonzero as that interval grows; existing dated entries must remain.
    entries = (item for item in costs.expenses if _utc_day(item.incurred_at) <= through)
    return _digest(
        {
            "schema_version": costs.schema_version,
            "source_mode": costs.source_mode,
            "basis": costs.basis,
            "first_mark_day": costs.first_mark_day,
            "expenses": tuple(sorted(entries, key=lambda item: item.cost_id)),
        }
    )


def record_checkpoint(
    *,
    freeze_sha256: str,
    through: date,
    nav_raw: bytes,
    benchmark_raw: bytes | None,
    costs: OperatingCosts | None,
    costs_sha256: str | None,
    previous: CheckpointReceipt | None,
    previous_receipt_sha256: str | None,
) -> CheckpointReceipt:
    nav = msgspec.convert(json.loads(nav_raw, object_pairs_hook=_unique_fields), type=PaperNAV)
    benchmark = load_paper_benchmark(benchmark_raw) if benchmark_raw is not None else None
    if previous is not None:
        cutoff = previous.checkpoint_utc
        if _nav_prefix(nav, cutoff) != previous.nav.prefix_sha256:
            raise ValueError("Paper NAV differs from the previous checkpoint NAV prefix")
        if (benchmark is None) != (previous.benchmark is None) or (
            benchmark is not None
            and _benchmark_prefix(benchmark, cutoff) != previous.benchmark.prefix_sha256
        ):
            raise ValueError(
                "Benchmark levels differ from the previous checkpoint benchmark prefix"
            )
        if previous.operating_costs is not None and (
            costs is None or _costs_prefix(costs, cutoff) != previous.operating_costs.prefix_sha256
        ):
            raise ValueError("Expenses differ from the previous checkpoint operating cost prefix")
    return CheckpointReceipt(
        schema_version="0.1",
        freeze_sha256=freeze_sha256,
        checkpoint_utc=through,
        previous_receipt_sha256=previous_receipt_sha256,
        nav=PrefixCommitment(sha256(nav_raw).hexdigest(), _nav_prefix(nav, through)),
        benchmark=PrefixCommitment(
            sha256(benchmark_raw).hexdigest(), _benchmark_prefix(benchmark, through)
        )
        if benchmark is not None
        else None,
        operating_costs=PrefixCommitment(costs_sha256, _costs_prefix(costs, through))
        if costs is not None
        else None,
    )
