"""Conditional sensitivity of the frozen primary comparison to a declared effect."""

from datetime import timedelta
from decimal import Decimal, localcontext
from fractions import Fraction
from itertools import pairwise
from math import fsum, sqrt
from random import Random
from statistics import NormalDist

import msgspec

from tradingbot_evaluation.statistics import (
    GAUSSIAN_PAIRED_MEAN,
    EvaluationProtocol,
    ForwardPaperPowerReview,
    _bootstrap_insufficiency,
    _moving_block_interval,
    _outward_excess_bounds,
    _paired_excess_bps,
)


def _wilson(successes: int, trials: int) -> tuple[float, float]:
    rate = successes / trials
    z = 1.959963984540054
    correction = 1 + z**2 / trials
    center = (rate + z**2 / (2 * trials)) / correction
    margin = z * sqrt((rate * (1 - rate) + z**2 / (4 * trials)) / trials) / correction
    return max(0.0, center - margin), min(1.0, center + margin)


def _rate_report(successes: int, trials: int, bounds: tuple[float, float]) -> dict:
    return {
        "success_count": successes,
        "probability": f"{successes / trials:.6f}",
        "monte_carlo_95_lower": f"{bounds[0]:.6f}",
        "monte_carlo_95_upper": f"{bounds[1]:.6f}",
    }


def _ar1_noise(n: int, sigma: int, phi: float, rng: Random) -> tuple[float, ...]:
    # Start in the stationary distribution. sigma is the marginal scale.
    innovation_sd = sigma * sqrt(1 - phi**2)
    values = [rng.gauss(0, sigma)]
    for _ in range(n - 1):
        values.append(phi * values[-1] + rng.gauss(0, innovation_sd))
    return tuple(values)


def _known_covariance_error(n: int, sigma: int, phi: float) -> float:
    # Cov(x_t, x_(t+k)) = sigma² * phi**k for a stationary AR(1).
    # Averaging all n² pairs weights lag k by 2 * (n-k).
    covariance_weight = fsum([n, *(2 * (n - k) * phi**k for k in range(1, n))])
    return sigma * sqrt(covariance_weight) / n


def forward_model_interval(
    protocol: EvaluationProtocol,
    *,
    nav_marks: tuple[dict[str, str], ...] | None,
    unavailable_reason: str | None,
) -> dict:
    design = protocol.paper_forward_source.power_review
    primary = next(
        row for row in protocol.comparisons if row.comparison_id == protocol.primary_comparison_id
    )
    result = {
        "method": design.method,
        "comparison_id": primary.comparison_id,
        "nav_basis": protocol.nav_basis,
        "minimum_daily_excess_bps": primary.minimum_daily_excess_bps,
        "model_assumptions_verified": False,
        "scope": "conditional_on_preregistered_gaussian_ar1_covariance",
        "daily_excess_bps_interval": None,
    }
    if nav_marks is None:
        return result | {"status": "unavailable", "reason": unavailable_reason}

    anchor = protocol.splits.test_start + timedelta(days=protocol.splits.embargo_calendar_days)
    start = protocol.expected_nav_dates.index(anchor)
    window = nav_marks[start:]
    with localcontext() as context:
        context.prec = 50
        strategy = tuple(Decimal(row[primary.strategy]) for row in window)
        benchmark = tuple(Decimal(row[primary.benchmark]) for row in window)
        excess = _paired_excess_bps(
            tuple(end / begin for begin, end in pairwise(strategy)),
            tuple(end / begin for begin, end in pairwise(benchmark)),
        )
    n = len(excess)
    mean = fsum(map(float, excess)) / n
    tail = Fraction(protocol.bootstrap.family_alpha_bps, 20_000 * len(protocol.comparisons))
    error = _known_covariance_error(
        n, design.daily_excess_sd_bps, design.lag_one_correlation_bps / 10_000
    )
    margin = NormalDist().inv_cdf(1 - float(tail)) * error
    lower, upper = _outward_excess_bounds(mean - margin, mean + margin)
    status = "inconclusive_under_model"
    if lower > primary.minimum_daily_excess_bps:
        status = "above_threshold_under_model"
    elif upper < primary.minimum_daily_excess_bps:
        status = "below_threshold_under_model"
    return result | {
        "status": status,
        "reason": None,
        "paired_return_count": n,
        "mean_daily_excess_bps": f"{mean:.6f}",
        "mean_standard_error_bps": f"{error:.6f}",
        "nominal_two_sided_confidence_under_model": f"{float(1 - 2 * tail):.6f}",
        "daily_excess_bps_interval": {"lower": format(lower, "f"), "upper": format(upper, "f")},
    }


def review_forward_power(protocol: EvaluationProtocol) -> dict | None:
    """Evaluate the design at the registered final sample size, without using observed returns."""
    source = protocol.paper_forward_source
    if source is msgspec.UNSET or source.power_review is msgspec.UNSET:
        return None
    design = source.power_review
    config = protocol.bootstrap
    anchor = protocol.splits.test_start + timedelta(days=protocol.splits.embargo_calendar_days)
    n = (protocol.expected_nav_dates[-1] - anchor).days
    tail = Fraction(config.family_alpha_bps, 20_000 * len(protocol.comparisons))
    primary = next(
        row for row in protocol.comparisons if row.comparison_id == protocol.primary_comparison_id
    )
    threshold = primary.minimum_daily_excess_bps
    alternative_mean = threshold + design.alternative_increment_bps
    result = {
        "method": design.method,
        "scope": "preregistered_final_primary_comparison",
        "comparison_id": primary.comparison_id,
        "registered_nav_basis": protocol.nav_basis,
        "planned_paired_return_count": n,
        "nominal_upper_tail_probability": f"{float(tail):.6f}",
        "null_mean_excess_bps": threshold,
        "alternative_mean_excess_bps": alternative_mean,
        "registered_scenario": msgspec.to_builtins(design),
        "empirical_returns_used": False,
        "model_assumptions_verified": False,
        "monte_carlo_interval_method": "wilson_95",
    }
    if design.method == GAUSSIAN_PAIRED_MEAN:
        return result | _review_known_covariance(design, n, tail, threshold)
    result.update(
        bootstrap_replications=config.replications,
        bootstrap_seed=config.seed,
        block_marks=config.block_marks,
        planned_complete_blocks=n // config.block_marks,
    )
    reason = _bootstrap_insufficiency(n, config, tail)
    if reason is not None:
        return result | {
            "status": "unavailable",
            "reason": reason,
            "null_upper_rejection": None,
            "alternative_detection": None,
            "modeled_sensitivity": None,
            "modeled_null_calibration": None,
        }

    rng = Random(f"tradingbot-power-review-v1:{design.seed}")
    phi = design.lag_one_correlation_bps / 10_000
    null_rejections = 0
    detections = 0
    for _ in range(design.trials):
        noise = _ar1_noise(n, design.daily_excess_sd_bps, phi, rng)
        low, high = _moving_block_interval(noise, config, tail)
        if low == high:
            continue
        # The percentile procedure is translation-equivariant, so both
        # hypotheses use the same random noise and bootstrap block indices.
        null_lower, _ = _outward_excess_bounds(low + threshold, high + threshold)
        alternative_lower, _ = _outward_excess_bounds(
            low + alternative_mean, high + alternative_mean
        )
        null_rejections += null_lower > threshold
        detections += alternative_lower > threshold

    return result | _modeled_results(
        null_rejections, detections, design.trials, float(tail), design.target_detection_bps
    )


def _review_known_covariance(
    design: ForwardPaperPowerReview, n: int, tail: Fraction, threshold: int
) -> dict:
    phi = design.lag_one_correlation_bps / 10_000
    sigma = design.daily_excess_sd_bps
    delta = design.alternative_increment_bps
    error = _known_covariance_error(n, sigma, phi)
    normal = NormalDist()
    cutoff = normal.inv_cdf(1 - float(tail))
    margin = cutoff * error
    counts = []
    for population, population_mean in (("null", threshold), ("alternative", threshold + delta)):
        rng = Random(f"tradingbot-gaussian-ar1-scenario-v1:{design.seed}:{population}")
        crossed = 0
        for _ in range(design.trials):
            mean = fsum(_ar1_noise(n, sigma, phi, rng)) / n + population_mean
            lower, _ = _outward_excess_bounds(mean - margin, mean + margin)
            crossed += lower > threshold
        counts.append(crossed)
    null_rejections, detections = counts
    return _modeled_results(
        null_rejections, detections, design.trials, float(tail), design.target_detection_bps
    ) | {
        "mean_standard_error_bps": f"{error:.6f}",
        "analytic_null_upper_rejection_probability": f"{float(tail):.6f}",
        "analytic_alternative_detection_probability": f"{normal.cdf(delta / error - cutoff):.6f}",
        "analytic_rule_before_outward_rounding": True,
        "null_and_alternative_share_trials": False,
    }


def _modeled_results(
    null_rejections: int, detections: int, trials: int, tail: float, target_bps: int
) -> dict:
    null_bounds = _wilson(null_rejections, trials)
    power_bounds = _wilson(detections, trials)
    target = target_bps / 10_000
    sensitivity = "monte_carlo_inconclusive"
    if power_bounds[0] >= target:
        sensitivity = "target_met_under_model"
    elif power_bounds[1] < target:
        sensitivity = "below_target_under_model"
    calibration = "inconclusive"
    if null_bounds[0] > tail:
        calibration = "above_nominal_under_model"
    elif null_bounds[1] < tail:
        calibration = "below_nominal_under_model"
    return {
        "status": "simulated",
        "reason": None,
        "null_upper_rejection": _rate_report(null_rejections, trials, null_bounds),
        "alternative_detection": _rate_report(detections, trials, power_bounds),
        "modeled_sensitivity": sensitivity,
        "modeled_null_calibration": calibration,
    }
