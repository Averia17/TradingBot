"""Build a bounded research job from an operator-pinned, locally ingested market archive."""

import argparse
import json
import math
import re
from collections import defaultdict
from datetime import timedelta
from decimal import Decimal, localcontext
from importlib.metadata import version
from itertools import pairwise
from pathlib import Path
from typing import Annotated, Literal
from uuid import UUID

from pydantic import Field, ValidationError

from tradingbot.contracts import Contract, Money, PortfolioSnapshot, Symbol
from tradingbot.snapshots import (
    MAX_ARCHIVE_BYTES,
    ArchiveRecord,
    ArchiveReference,
    Identifier,
    _utc,
    decode_pinned_json,
    load_snapshot,
)

PRICE = re.compile(r"(?:0|[1-9][0-9]{0,11})(?:\.[0-9]{1,8})?")
ALLOCATION_CALLS = 1


class Candidate(Contract):
    symbol: Symbol
    allowed: Annotated[bool, Field(strict=True)]


class SelectionRequest(Contract):
    schema_version: Literal["0.1"]
    job_id: UUID
    policy_version: Identifier
    portfolio: PortfolioSnapshot
    data_manifest: ArchiveReference
    candidates: Annotated[tuple[Candidate, ...], Field(min_length=1, max_length=100)]
    top_k: Annotated[int, Field(strict=True, ge=1, le=100)]
    remaining_analysis_slots: Annotated[int, Field(strict=True, ge=0, le=100)]
    calls_per_symbol: Annotated[int, Field(strict=True, ge=1, le=63)]
    max_llm_calls: Annotated[int, Field(strict=True, ge=1, le=64)]
    roc_periods: Annotated[int, Field(strict=True, ge=2, le=30)]
    max_bar_age_seconds: Annotated[int, Field(strict=True, ge=1, le=86400 * 7)]
    max_bar_gap_seconds: Annotated[int, Field(strict=True, ge=1, le=86400 * 7)]
    max_catalyst_age_seconds: Annotated[int, Field(strict=True, ge=1, le=86400 * 30)]
    min_bar_turnover_usd: Money
    max_spread_bps: Annotated[int, Field(strict=True, ge=0, le=10000)]


def _price(value) -> Decimal:
    if not isinstance(value, str) or PRICE.fullmatch(value) is None:
        raise ValueError("market price is not a decimal string")
    result = Decimal(value)
    if result <= 0:
        raise ValueError("market price is not positive")
    return result


def _momentum(closes: list[Decimal], periods: int) -> str:
    from pandas import DataFrame
    from stockstats import wrap

    # The pinned TradingAgents indicator library supplies ROC. The float is
    # used solely to rank, never to compute money, positions, or fee limits.
    latest = wrap(DataFrame({"close": [float(price) for price in closes]}))[
        f"close_{periods}_roc"
    ].iloc[-1]
    if not math.isfinite(latest):
        raise ValueError("nonfinite momentum indicator")
    return f"{latest * 100:.6f}"


def _actions_in_window(records: list[ArchiveRecord], first, last) -> bool:
    for item in records:
        if item.kind != "action":
            continue
        try:
            effective = _utc(item.payload.get("effective_at"))
        except ValueError:
            # A recently available corporate action without an effective
            # time leaves the raw-price window ambiguous.
            if item.event_time >= first:
                return True
            continue
        if first < effective <= last:
            return True
    return False


def _score(
    config: SelectionRequest, candidate: Candidate, records: list[ArchiveRecord], held: set[str]
) -> dict:
    row = {"symbol": candidate.symbol}
    if not candidate.allowed:
        return row | {"status": "not_allowed"}
    bars = sorted(
        (record for record in records if record.kind == "bar"), key=lambda item: item.event_time
    )
    bars = bars[-(config.roc_periods + 1) :]
    if len(bars) <= config.roc_periods:
        return row | {"status": "insufficient_bars"}
    if (
        len({item.event_time for item in bars}) != len(bars)
        or len({item.provider for item in bars}) != 1
    ):
        return row | {"status": "ambiguous_bar_series"}
    if any(
        after.event_time - before.event_time > timedelta(seconds=config.max_bar_gap_seconds)
        for before, after in pairwise(bars)
    ):
        return row | {"status": "bar_gap_above_limit"}
    as_of = _utc(config.portfolio.as_of)
    latest = bars[-1]
    if as_of - latest.event_time > timedelta(seconds=config.max_bar_age_seconds):
        return row | {"status": "stale_bar"}
    if _actions_in_window(records, bars[0].event_time, latest.event_time):
        return row | {"status": "corporate_action_in_window"}
    if any(
        row.payload.get("currency") != "USD" or row.payload.get("price_basis") != "raw"
        for row in bars
    ):
        return row | {"status": "unverified_bar_basis"}
    try:
        closes = [_price(item.payload.get("close")) for item in bars]
        bid = _price(latest.payload.get("bid"))
        ask = _price(latest.payload.get("ask"))
        volume = latest.payload.get("volume")
        if ask < bid or type(volume) is not int or not 0 < volume < 10**20:
            raise ValueError("invalid quote or volume")
        with localcontext() as exact:
            exact.prec = 60
            turnover = closes[-1] * volume
            if turnover < config.min_bar_turnover_usd:
                return row | {"status": "turnover_below_limit"}
            # Compare the fraction exactly without dividing by the midpoint.
            spread = (ask - bid) * 20000
            if spread > config.max_spread_bps * (ask + bid):
                return row | {"status": "spread_above_limit"}
    except ValueError:
        return row | {"status": "invalid_market_inputs"}
    catalyst = any(
        item.kind in ("news", "filing")
        and as_of - item.ingested_at <= timedelta(seconds=config.max_catalyst_age_seconds)
        for item in records
    )
    return row | {
        "status": "ranked",
        "priority": "held_catalyst" if candidate.symbol in held and catalyst else "momentum",
        "momentum_bps": _momentum(closes, config.roc_periods),
        "latest_bar_sha256": latest.sha256,
    }


def plan_research(root: Path, request: dict) -> dict:
    config = SelectionRequest.model_validate(request)
    symbols = sorted(candidate.symbol for candidate in config.candidates)
    if len(set(symbols)) != len(symbols):
        raise ValueError("duplicate research candidate symbol")
    snapshot = load_snapshot(
        root,
        config.data_manifest,
        config.portfolio.as_of,
        symbols=symbols,
        visibility="locally_ingested",
    )
    records: dict[str, list[ArchiveRecord]] = defaultdict(list)
    for item in snapshot.records:
        if item.symbol is not None:
            records[item.symbol].append(item)
    held = {item.symbol for item in config.portfolio.positions}
    ranked, rejected = [], []
    for candidate in config.candidates:
        row = _score(config, candidate, records[candidate.symbol], held)
        if row["status"] == "ranked":
            ranked.append(row)
        else:
            rejected.append(row)
    ranked.sort(
        key=lambda row: (
            row["priority"] != "held_catalyst",
            -Decimal(row["momentum_bps"]),
            row["symbol"],
        )
    )
    capacity = min(
        config.top_k,
        config.remaining_analysis_slots,
        (config.max_llm_calls - ALLOCATION_CALLS) // config.calls_per_symbol,
    )
    selected = [row["symbol"] for row in ranked[:capacity]]
    for index, row in enumerate(ranked):
        row["status"] = "selected" if index < capacity else "capacity_exhausted"
    job = None
    if selected:
        job = {
            "job_schema_version": "0.1",
            "job_id": str(config.job_id),
            "policy_version": config.policy_version,
            "portfolio": config.portfolio.model_dump(mode="json"),
            "data_manifest": config.data_manifest.model_dump(mode="json"),
            "requested_symbols": selected,
            "max_llm_calls": ALLOCATION_CALLS + len(selected) * config.calls_per_symbol,
        }
    return {
        "selection_schema_version": "0.1",
        "source_mode": snapshot.source_mode,
        "snapshot_sha256": snapshot.sha256,
        "stockstats_version": version("stockstats"),
        "job": job,
        "candidates": ranked + sorted(rejected, key=lambda row: row["symbol"]),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", action="store_true", help="Print the selection-request schema")
    parser.add_argument("--root", type=Path)
    parser.add_argument("--request", type=Path)
    args = parser.parse_args()
    if args.schema:
        print(json.dumps(SelectionRequest.model_json_schema(mode="serialization"), indent=2))
        return 0
    if args.root is None or args.request is None:
        parser.error("--root and --request are required")
    try:
        with args.request.open("rb") as stream:
            raw = stream.read(MAX_ARCHIVE_BYTES + 1)
        if len(raw) > MAX_ARCHIVE_BYTES:
            raise ValueError("selection request exceeds the per-document byte limit")
        result = plan_research(args.root, decode_pinned_json(raw))
    except ValidationError:
        parser.exit(2, "selection rejected: invalid request or archive schema\n")
    except (OSError, ValueError, RecursionError) as exc:
        parser.exit(2, f"selection rejected ({type(exc).__name__})\n")
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
