"""Assemble issuer archives and timestamped equity observations for research."""

from datetime import UTC, datetime
from pathlib import Path
from uuid import uuid4

from pydantic import TypeAdapter

from tradingbot.contracts import PortfolioSnapshot, Symbol
from tradingbot.market_capture import capture_market
from tradingbot.snapshots import ArchiveRecord, load_snapshot

_SYMBOLS = TypeAdapter(list[Symbol])


def prepare(
    symbols: list[str], sources: list[tuple[Path, str]], cash_usd: str
) -> tuple[PortfolioSnapshot, dict]:
    symbols = _SYMBOLS.validate_python(symbols)
    if not symbols or len(set(symbols)) != len(symbols):
        raise ValueError("prepare requires unique research symbols")
    started = datetime.now(UTC)
    book = PortfolioSnapshot(snapshot_id=uuid4(), as_of=started, cash_usd=cash_usd)
    issuer: dict[tuple, ArchiveRecord] = {}
    for path, digest in sources:
        snapshot = load_snapshot(
            path.parent, {"ref": path.name, "sha256": digest},
            started, symbols=symbols, visibility="locally_ingested",
        )
        if snapshot.source_mode != "local_archive":
            raise ValueError("issuer archives for real research must use local_archive")
        for item in snapshot.records:
            if item.kind not in {"filing", "news"} or item.symbol is None:
                continue
            key = (*item.key(), item.revision)
            existing = issuer.setdefault(key, item)
            if existing != item:
                raise ValueError(f"conflicting issuer record: {item.symbol}/{item.record_id}")
    coverage = {(item.symbol, item.kind) for item in issuer.values()}
    for symbol in symbols:
        for kind in ("filing", "news"):
            if (symbol, kind) not in coverage:
                raise ValueError(f"{symbol}: issuer archives are missing {kind} evidence")
    records = list(issuer.values())
    for symbol in symbols:
        records.extend(capture_market(symbol))
    book = book.model_copy(update={"as_of": datetime.now(UTC)})
    if any(item.ingested_at > book.as_of for item in records):
        raise ValueError("record receipt is later than the portfolio cutoff")
    return book, {
        "schema_version": "0.1",
        "source_mode": "local_archive",
        "records": [
            item.model_dump(mode="json")
            for item in sorted(records, key=lambda item: (item.key(), item.revision))
        ],
    }
