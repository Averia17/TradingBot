"""Versioned single-step boundary for the offline historical evaluator."""

import re
from datetime import datetime
from pathlib import Path

from pydantic import TypeAdapter, field_validator

from tradingbot.adapters.offline import HistoricalVariant, OfflineJob, analyze_historical
from tradingbot.contracts import DECIMAL_PATTERN, Contract, PortfolioSnapshot
from tradingbot.snapshots import CaptureTime, decode_pinned_json

MAX_STEP_BYTES = 256 * 1024
CAPTURE_TIME = TypeAdapter(CaptureTime)
UTC_SUFFIX = ("Z", "+00:00")


def _check_wire_amounts(values: dict, fields: tuple[str, ...]) -> None:
    for field in fields:
        if field in values:
            value = values[field]
            if not isinstance(value, str) or re.fullmatch(DECIMAL_PATTERN, value) is None:
                raise ValueError(f"{field} must be a decimal string")


class HistoricalStep(Contract):
    job: OfflineJob
    completed_at: datetime
    variant: HistoricalVariant = "full_graph"

    @field_validator("completed_at", mode="before")
    @classmethod
    def exact_clock(cls, value: str) -> datetime:
        if not isinstance(value, str) or not value.endswith(UTC_SUFFIX):
            raise ValueError("historical completion must be a UTC RFC 3339 string")
        return CAPTURE_TIME.validate_python(value)

    @field_validator("job", mode="before")
    @classmethod
    def exact_snapshot(cls, job: dict) -> dict:
        if not isinstance(job, dict) or not isinstance(job.get("portfolio"), dict):
            raise ValueError("historical step requires a research job and snapshot")
        portfolio = job["portfolio"]
        if portfolio.keys() != PortfolioSnapshot.model_fields.keys():
            raise ValueError("historical snapshot must include all serialized contract fields")
        if not isinstance(portfolio.get("as_of"), str):
            raise ValueError("snapshot cutoff must be an RFC 3339 string")
        _check_wire_amounts(portfolio, ("cash_usd", "reserved_cash_usd", "max_position_pct"))
        positions = portfolio.get("positions", [])
        if not isinstance(positions, list) or any(not isinstance(item, dict) for item in positions):
            raise ValueError("historical positions must be a JSON array of objects")
        for position in positions:
            _check_wire_amounts(position, ("quantity", "mark_price_usd"))
        as_of = CAPTURE_TIME.validate_python(portfolio["as_of"])
        return job | {"portfolio": portfolio | {"as_of": as_of}}


def run_historical_step(data: bytes, archive_root: Path) -> dict:
    if len(data) > MAX_STEP_BYTES:
        raise ValueError("historical step exceeds the input size limit")
    step = HistoricalStep.model_validate(decode_pinned_json(data))
    return analyze_historical(step.job, step.completed_at, archive_root, variant=step.variant)
