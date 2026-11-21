"""Build allocator evidence from local observations received before a portfolio cutoff."""

import json
from pathlib import Path

from tradingbot.contracts import PortfolioSnapshot
from tradingbot.snapshots import PointInTimeSnapshot, load_snapshot

REPORTS_BY_KIND = {"bar": "market_report", "filing": "fundamentals_report", "news": "news_report"}
MAX_REPORT_BYTES = 64 * 1024


def load_prepared_reports(
    path: Path, digest: str, book: PortfolioSnapshot, symbols: list[str]
) -> tuple[PointInTimeSnapshot, dict[str, dict[str, str]]]:
    snapshot = load_snapshot(
        path.parent, {"ref": path.name, "sha256": digest}, book.as_of,
        symbols=symbols, visibility="locally_ingested",
    )
    return snapshot, build_prepared_reports(snapshot, symbols)


def build_prepared_reports(
    snapshot: PointInTimeSnapshot, symbols: list[str], *, latest_per_kind: int | None = None,
) -> dict[str, dict[str, str]]:
    reports: dict[str, dict[str, str]] = {}
    for symbol in symbols:
        reports[symbol] = {}
        for kind, field in REPORTS_BY_KIND.items():
            records = [
                item for item in snapshot.records
                if item.kind == kind and item.symbol == symbol
            ]
            if not records:
                raise ValueError(f"{symbol}: prepared data is missing an available {kind} record")
            if latest_per_kind is not None:
                records = sorted(records, key=lambda item: (item.available_at, item.key()))
                records = records[-latest_per_kind:]
            evidence = json.dumps(
                {
                    "as_of": snapshot.as_of.isoformat(),
                    "archive_source_mode": snapshot.source_mode,
                    "records": [item.model_dump(mode="json") for item in records],
                },
                ensure_ascii=True, separators=(",", ":"), sort_keys=True,
            )
            if len(evidence) > MAX_REPORT_BYTES:
                raise ValueError(f"{symbol}: prepared {kind} evidence exceeds the report byte limit")
            reports[symbol][field] = evidence
    return reports
