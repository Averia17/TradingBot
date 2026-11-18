"""Describe paired forward returns by predeclared, point-in-time index trend."""

from datetime import timedelta
from fractions import Fraction

import msgspec

from tradingbot_evaluation.paper_evidence import PaperBenchmark, _utc_nanoseconds
from tradingbot_evaluation.statistics import Comparison, EvaluationProtocol, ForwardRegimeReview

REGIMES = ("declining", "flat", "rising")


def _return_bps(initial: Fraction, final: Fraction) -> Fraction:
    return (final / initial - 1) * 10_000


def _trend(
    offset: int,
    design: ForwardRegimeReview,
    index_values: tuple[Fraction, ...],
    observed_times: tuple[int, ...],
) -> tuple[str, str | None, int, Fraction | None]:
    start = max(0, offset - design.lookback_utc_days)
    distinct = len(set(observed_times[start : offset + 1]))
    if offset < design.lookback_utc_days:
        return "unclassified", "insufficient_lookback", distinct, None
    if distinct < design.minimum_distinct_observations:
        return "unclassified", "too_few_distinct_index_observations", distinct, None
    trailing = _return_bps(index_values[start], index_values[offset])
    if trailing <= -design.declining_threshold_bps:
        return "declining", None, distinct, trailing
    if trailing >= design.rising_threshold_bps:
        return "rising", None, distinct, trailing
    return "flat", None, distinct, trailing


def review_forward_regimes(
    protocol: EvaluationProtocol,
    design: ForwardRegimeReview,
    primary: Comparison,
    index: PaperBenchmark,
    nav_marks: tuple[dict[str, str], ...],
    *,
    nav_basis: str,
    requested_nav_basis_available: bool,
) -> dict:
    levels = index.levels
    dates = tuple(level.day for level in levels)
    if len(levels) != len(nav_marks) or dates != protocol.expected_nav_dates[: len(nav_marks)]:
        raise ValueError("Regime observations and NAV must share a complete UTC calendar")
    test_start = protocol.splits.test_start + timedelta(days=protocol.splits.embargo_calendar_days)
    anchor = dates.index(test_start)
    index_values = tuple(Fraction(level.level) for level in levels)
    observed_times = tuple(_utc_nanoseconds(level.observed_at) for level in levels)
    rows = []
    daily = {name: [] for name in (*REGIMES, "unclassified")}
    strategy = tuple(Fraction(row[primary.strategy]) for row in nav_marks)
    comparison = tuple(Fraction(row[primary.benchmark]) for row in nav_marks)
    for offset in range(anchor, len(dates) - 1):
        regime, reason, distinct, trailing = _trend(offset, design, index_values, observed_times)
        outcome = _return_bps(strategy[offset], strategy[offset + 1])
        outcome -= _return_bps(comparison[offset], comparison[offset + 1])
        daily[regime].append(outcome)
        rows.append(
            {
                "from_utc": dates[offset].isoformat(),
                "through_utc": dates[offset + 1].isoformat(),
                "regime": regime,
                "reason": reason,
                "distinct_index_observations": distinct,
                "trailing_index_return_bps": f"{trailing:.6f}" if trailing is not None else None,
                "daily_excess_bps": f"{outcome:.6f}",
            }
        )
    counts = {
        name: {
            "paired_returns": len(values),
            "mean_daily_excess_bps": f"{sum(values) / len(values):.6f}" if values else None,
        }
        for name, values in daily.items()
    }
    return {
        "method": "trailing_index_return_known_at_pair_start",
        "interpretation": "descriptive_only",
        "comparison_id": primary.comparison_id,
        "benchmark_series_id": index.series_id,
        "benchmark_source_mode": index.source_mode,
        "benchmark_access_declared": primary.benchmark_access,
        "nav_basis": nav_basis,
        "requested_nav_basis_available": requested_nav_basis_available,
        "registered_design": msgspec.to_builtins(design),
        "total_paired_returns": len(rows),
        "all_registered_regimes_covered": all(
            len(daily[name]) >= design.minimum_pairs_per_regime for name in REGIMES
        ),
        "regimes": counts,
        "pairs": rows,
    }
