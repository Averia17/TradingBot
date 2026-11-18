"""Apply a pinned USD cost ledger to execution-net marks for reporting."""

import json
from bisect import bisect_left
from collections import defaultdict
from datetime import UTC, date, datetime
from decimal import Decimal, localcontext
from hashlib import sha256
from typing import Annotated, Literal, get_args

import msgspec

from tradingbot_evaluation.replay import MAX_INPUT_BYTES, IntentID, Timestamp, _unique_fields

USD = Annotated[str, msgspec.Meta(pattern=r"^(0|[1-9][0-9]{0,11})(\.[0-9]{1,8})?$")]
CostCategory = Literal[
    "llm",
    "data",
    "infrastructure",
    "chain_fees",
    "additional_execution_fees",
    "fx",
    "issuer_drag",
    "other_operating",
]
COST_CATEGORIES = get_args(CostCategory)


class Expense(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    cost_id: IntentID
    series_id: IntentID
    category: CostCategory
    incurred_at: Timestamp
    amount_usd: USD | None
    reference_id: IntentID


class CostCoverage(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    series_id: IntentID
    category: CostCategory
    status: Literal["complete", "zero", "unknown"]
    reference_id: IntentID


class OperatingCosts(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    source_mode: Literal["synthetic_fixture", "archived_local"]
    basis: Literal["additional_to_execution_net"]
    first_mark_day: date
    last_mark_day: date
    expenses: Annotated[tuple[Expense, ...], msgspec.Meta(max_length=10_000)]
    coverage: Annotated[tuple[CostCoverage, ...], msgspec.Meta(max_length=256)]


def _utc_day(timestamp: datetime) -> date:
    if timestamp.utcoffset() is None:
        raise ValueError("Cost timestamp must include a timezone")
    try:
        return timestamp.astimezone(UTC).date()
    except OverflowError:
        raise ValueError("Cost timestamp has no representable UTC day") from None


def load_operating_costs(data: bytes) -> OperatingCosts:
    if len(data) > MAX_INPUT_BYTES:
        raise ValueError("Operating cost ledger exceeds the input limit")
    costs = msgspec.convert(json.loads(data, object_pairs_hook=_unique_fields), type=OperatingCosts)
    _validate_costs(costs)
    return costs


def _validate_costs(costs: OperatingCosts) -> None:
    if costs.first_mark_day >= costs.last_mark_day:
        raise ValueError("Cost ledger mark days must be chronological")
    if len({row.cost_id for row in costs.expenses}) != len(costs.expenses):
        raise ValueError("Duplicate cost ID in the operating cost ledger")
    if len({(row.series_id, row.category) for row in costs.coverage}) != len(costs.coverage):
        raise ValueError("Duplicate coverage in the operating cost ledger")
    if any(
        not costs.first_mark_day < _utc_day(row.incurred_at) <= costs.last_mark_day
        for row in costs.expenses
    ):
        raise ValueError("Cost must be incurred after the funding mark and within the mark window")


def operating_costs_sha256(costs: OperatingCosts) -> str:
    return sha256(msgspec.json.encode(costs, order="deterministic")).hexdigest()


def apply_operating_costs(
    dates: tuple[date, ...],
    execution_navs: dict[str, list[Decimal]],
    frozen_sha256: str,
    costs: OperatingCosts | None,
) -> tuple[dict[str, list[Decimal]] | None, dict]:
    report = {
        "sha256": frozen_sha256,
        "method": "cumulative_costs_subtracted_from_execution_net_marks",
        "attribution": "utc_incurred_day_to_first_frozen_mark_on_or_after",
        "funding_and_execution_recomputed": False,
        "status": "unavailable",
        "daily_nav": None,
    }
    if costs is None:
        return None, report | {"blockers": [{"reason": "missing_cost_ledger"}]}
    if operating_costs_sha256(costs) != frozen_sha256:
        raise ValueError("Operating cost ledger SHA-256 differs from the frozen digest")
    _validate_costs(costs)
    if (costs.first_mark_day, costs.last_mark_day) != (dates[0], dates[-1]):
        raise ValueError("Cost ledger mark window differs from the frozen NAV window")
    if any(row.series_id not in execution_navs for row in (*costs.expenses, *costs.coverage)):
        raise ValueError("Operating cost ledger names an unregistered NAV series")

    with localcontext() as context:
        context.prec = 50
        coverage = {(row.series_id, row.category): row.status for row in costs.coverage}
        seen = {(row.series_id, row.category) for row in costs.expenses}
        unknown = {
            (row.series_id, row.category) for row in costs.expenses if row.amount_usd is None
        }
        totals: dict[tuple[str, str], Decimal] = defaultdict(Decimal)
        debits: dict[tuple[str, int], Decimal] = defaultdict(Decimal)
        for row in costs.expenses:
            if coverage.get((row.series_id, row.category)) == "zero":
                raise ValueError("Zero cost coverage conflicts with an expense")
            if row.amount_usd is not None:
                amount = Decimal(row.amount_usd)
                totals[row.series_id, row.category] += amount
                debits[row.series_id, bisect_left(dates, _utc_day(row.incurred_at))] += amount

        blockers = []
        known: dict[str, dict[str, str | None]] = {}
        for series_id in execution_navs:
            known[series_id] = {}
            for category in COST_CATEGORIES:
                key = (series_id, category)
                status = coverage.get(key)
                known[series_id][category] = (
                    format(totals[key], "f") if key in totals or status == "zero" else None
                )
                if status is None or status == "unknown":
                    reason = "coverage_unknown"
                elif key in unknown:
                    reason = "amount_unknown"
                elif status == "complete" and key not in seen:
                    reason = "expenses_missing"
                else:
                    continue
                blockers.append({"series_id": series_id, "category": category, "reason": reason})

        report |= {
            "source_mode": costs.source_mode,
            "known_cost_usd_by_series": known,
            "blockers": blockers,
        }
        if blockers:
            return None, report

        operating_navs = {}
        for series_id, marks in execution_navs.items():
            zero = Decimal()
            cumulative = zero
            adjusted = []
            for index, nav in enumerate(marks):
                cumulative += debits.get((series_id, index), zero)
                if nav <= cumulative:
                    blockers.append(
                        {
                            "series_id": series_id,
                            "day": dates[index].isoformat(),
                            "reason": "non_positive_operating_nav",
                        }
                    )
                    break
                adjusted.append(nav - cumulative)
            operating_navs[series_id] = adjusted
        if blockers:
            return None, report

        report["status"] = "calculated"
        report["daily_nav"] = [
            {
                "day": day.isoformat(),
                "nav_usd": {
                    series_id: format(marks[index], "f")
                    for series_id, marks in operating_navs.items()
                },
            }
            for index, day in enumerate(dates)
        ]
        return operating_navs, report
