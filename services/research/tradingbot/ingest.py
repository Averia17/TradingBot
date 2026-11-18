"""Build a point-in-time archive from pinned local market and economic JSON."""

import argparse
import json
import re
import sys
from collections import defaultdict
from datetime import UTC, date, datetime, time
from decimal import Decimal
from itertools import pairwise
from pathlib import Path
from typing import Annotated, Literal, Self

from pydantic import Field, JsonValue, model_validator

from tradingbot.contracts import DECIMAL_PATTERN, Contract, Symbol
from tradingbot.snapshots import (
    MAX_ARCHIVE_BYTES,
    ArchiveRecord,
    ArchiveReference,
    CaptureTime,
    Identifier,
    Listing,
    SourceMode,
    UTCDateTime,
    _utc,
    decode_pinned_json,
    read_pinned_json,
)

MAX_CAPTURES = 32
MAX_RECORDS = 10_000
Accession = Annotated[str, Field(pattern=r"^[0-9]{10}-[0-9]{2}-[0-9]{6}$")]
CalendarDate = Annotated[str, Field(pattern=r"^[0-9]{4}-[0-9]{2}-[0-9]{2}$")]
VALUE = re.compile(r"-?[0-9]{1,32}(?:\.[0-9]{1,24})?")
MarketAmount = Annotated[str, Field(pattern=DECIMAL_PATTERN)]
Shares = Annotated[int, Field(strict=True, ge=1, le=1_000_000)]


class SECCapture(Contract):
    kind: Literal["sec_submissions"]
    source: ArchiveReference
    captured_at: UTCDateTime
    cik: Annotated[str, Field(pattern=r"^[0-9]{10}$")]
    symbol: Symbol
    accessions: Annotated[tuple[Accession, ...], Field(min_length=1, max_length=1000)]


class ALFREDCapture(Contract):
    kind: Literal["alfred_observations"]
    source: ArchiveReference
    captured_at: UTCDateTime
    series_id: Annotated[str, Field(pattern=r"^[A-Z0-9][A-Z0-9_]{0,59}$")]
    vintage_date: CalendarDate


class BLSCapture(Contract):
    kind: Literal["bls_release_review"]
    source: ArchiveReference
    reviewed_at: CaptureTime


class BLSReleaseReview(Contract):
    schema_version: Literal["0.1"]
    source_url: Annotated[
        str, Field(pattern=r"^https://www\.bls\.gov/news\.release/archives/empsit_[0-9]{8}\.htm$")
    ]
    released_at: CaptureTime
    observation_date: CalendarDate
    series_id: Literal["CES_TOTAL_NONFARM_PAYROLL_CHANGE"]
    value: Annotated[str, Field(pattern=r"^(?:0|-?[1-9][0-9]{0,11})$")]
    unit: Literal["jobs"]


class MarketCapture(Contract):
    kind: Literal["normalized_market"]
    source: ArchiveReference
    captured_at: CaptureTime
    provider: Identifier


class _PublishedRecord(Contract):
    record_id: Identifier
    symbol: Symbol
    revision: Annotated[int, Field(strict=True, ge=1, le=1_000_000)]
    event_time: CaptureTime
    available_at: CaptureTime


class _RawBar(Contract):
    security_id: Identifier
    price_basis: Literal["raw"]
    currency: Literal["USD"]
    open: MarketAmount
    high: MarketAmount
    low: MarketAmount
    close: MarketAmount
    volume: Annotated[int, Field(strict=True, ge=0, lt=10**20)]
    bid: MarketAmount | None = None
    ask: MarketAmount | None = None

    @model_validator(mode="after")
    def coherent_prices(self) -> Self:
        low, opening, closing, high = map(Decimal, (self.low, self.open, self.close, self.high))
        if not (0 < low <= min(opening, closing) <= max(opening, closing) <= high):
            raise ValueError("invalid unadjusted OHLC prices")
        if (self.bid is None) != (self.ask is None):
            raise ValueError("bar must supply both sides of the quote")
        if (
            self.bid is not None
            and self.ask is not None
            and not (0 < Decimal(self.bid) <= Decimal(self.ask))
        ):
            raise ValueError("invalid bar bid/ask")
        return self


class _Headline(Contract):
    headline: Annotated[str, Field(min_length=1, max_length=500)]
    security_id: Identifier | None = None


class _StockSplit(Contract):
    security_id: Identifier
    action: Literal["split"]
    effective_at: CaptureTime
    new_shares: Shares
    old_shares: Shares

    @model_validator(mode="after")
    def quantity_changes(self) -> Self:
        if self.new_shares == self.old_shares:
            raise ValueError("split must change the share ratio")
        return self


class _CashDividend(Contract):
    security_id: Identifier
    action: Literal["cash_dividend"]
    effective_at: CaptureTime
    paid_at: CaptureTime
    cash_per_share_usd: MarketAmount

    @model_validator(mode="after")
    def valid_payout(self) -> Self:
        if Decimal(self.cash_per_share_usd) <= 0 or self.paid_at < self.effective_at:
            raise ValueError("invalid cash dividend timing or amount")
        return self


class MarketBar(_PublishedRecord):
    kind: Literal["bar"]
    payload: _RawBar


class MarketNews(_PublishedRecord):
    kind: Literal["news"]
    symbol: Symbol | None
    payload: _Headline


class MarketAction(_PublishedRecord):
    kind: Literal["action"]
    payload: Annotated[_StockSplit | _CashDividend, Field(discriminator="action")]


class MarketListing(_PublishedRecord):
    kind: Literal["listing"]
    payload: Listing


MarketRecord = Annotated[
    MarketBar | MarketNews | MarketAction | MarketListing, Field(discriminator="kind")
]


class MarketResponse(Contract):
    schema_version: Literal["0.1"]
    records: Annotated[tuple[MarketRecord, ...], Field(min_length=1, max_length=MAX_RECORDS)]


class CaptureManifest(Contract):
    schema_version: Literal["0.1"]
    source_mode: SourceMode
    captures: Annotated[
        tuple[
            Annotated[
                SECCapture | ALFREDCapture | BLSCapture | MarketCapture, Field(discriminator="kind")
            ],
            ...,
        ],
        Field(min_length=1, max_length=MAX_CAPTURES),
    ]


def _day(value: JsonValue) -> date:
    if not isinstance(value, str) or re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", value) is None:
        raise ValueError("source did not provide an ISO calendar date")
    return date.fromisoformat(value)


def _sec_records(capture: SECCapture, response: JsonValue) -> list[ArchiveRecord]:
    if not isinstance(response, dict):
        raise ValueError("expected a SEC submissions object")
    filings = response.get("filings")
    recent = filings.get("recent") if isinstance(filings, dict) else None
    if str(response.get("cik", "")).zfill(10) != capture.cik or not isinstance(recent, dict):
        raise ValueError("SEC response does not match the declared filer or recent filings")
    columns = [
        recent.get(name) for name in ("accessionNumber", "acceptanceDateTime", "form", "reportDate")
    ]
    if not all(isinstance(column, list) for column in columns):
        raise ValueError("SEC filing columns are missing")
    selected = set(capture.accessions)
    if len(selected) != len(capture.accessions):
        raise ValueError("duplicate requested SEC accession")
    records = []
    seen = set()
    for accession, accepted, form, period in zip(*columns, strict=True):
        if not isinstance(accession, str) or accession not in selected:
            continue
        if accession in seen:
            raise ValueError("duplicate requested SEC accession in the pinned response")
        seen.add(accession)
        if (
            not isinstance(accepted, str)
            or not isinstance(form, str)
            or not form.strip()
            or len(form) > 24
        ):
            raise ValueError("SEC filing lacks an acceptance timestamp or form")
        accepted_at = _utc(accepted)
        if accepted_at > capture.captured_at:
            raise ValueError("SEC response was captured before acceptance")
        records.append(
            ArchiveRecord(
                kind="filing",
                provider="sec-edgar",
                record_id=accession,
                symbol=capture.symbol,
                revision=1,
                event_time=accepted_at,
                available_at=capture.captured_at,
                ingested_at=capture.captured_at,
                payload={
                    "cik": capture.cik,
                    "accession": accession,
                    "form": form,
                    "report_date": None if period == "" else _day(period).isoformat(),
                    "availability_basis": "local_capture",
                    "source_sha256": capture.source.sha256,
                },
            )
        )
    if seen != selected:
        raise ValueError("requested SEC accession missing from the pinned response")
    return records


def _alfred_records(capture: ALFREDCapture, response: JsonValue) -> list[ArchiveRecord]:
    vintage = _day(capture.vintage_date)
    observations = response.get("observations") if isinstance(response, dict) else None
    if (
        not isinstance(response, dict)
        or response.get("realtime_start") != capture.vintage_date
        or response.get("realtime_end") != capture.vintage_date
        or type(response.get("output_type")) is not int
        or response.get("output_type") != 1
        or response.get("units") != "lin"
        or type(response.get("offset")) is not int
        or response.get("offset") != 0
        or not isinstance(observations, list)
        or type(response.get("count")) is not int
        or response.get("count") != len(observations)
        or not 0 < len(observations) <= MAX_RECORDS
        or capture.captured_at.date() < vintage
    ):
        raise ValueError("ALFRED response must contain a complete single declared vintage")
    records = []
    for row in observations:
        if not isinstance(row, dict) or not (
            row.get("realtime_start") == row.get("realtime_end") == capture.vintage_date
        ):
            raise ValueError("ALFRED observation belongs to a different vintage")
        period = _day(row.get("date"))
        value = row.get("value")
        if (
            period > vintage
            or not isinstance(value, str)
            or (value != "." and VALUE.fullmatch(value) is None)
        ):
            raise ValueError("invalid ALFRED period or value")
        records.append(
            ArchiveRecord(
                kind="macro",
                provider="alfred",
                record_id=f"{capture.series_id}.{period}",
                symbol=None,
                revision=1,
                event_time=datetime.combine(period, time.min, UTC),
                available_at=capture.captured_at,
                ingested_at=capture.captured_at,
                payload={
                    "series_id": capture.series_id,
                    "observation_date": period.isoformat(),
                    "vintage_date": capture.vintage_date,
                    "value": None if value == "." else value,
                    "availability_basis": "local_capture",
                    "source_sha256": capture.source.sha256,
                },
            )
        )
    return records


def _bls_record(capture: BLSCapture, response: JsonValue) -> ArchiveRecord:
    release = BLSReleaseReview.model_validate(response)
    period = _day(release.observation_date)
    release_date = release.released_at.strftime("%m%d%Y")
    if (
        period.day != 1
        or period >= release.released_at.date()
        or not release.source_url.endswith(f"empsit_{release_date}.htm")
    ):
        raise ValueError("BLS observation must identify the reported month and its release")
    if capture.reviewed_at < release.released_at:
        raise ValueError("BLS observation was reviewed before its official release")
    return ArchiveRecord(
        kind="macro",
        provider="bls",
        record_id=f"{release.series_id}.{period}",
        symbol=None,
        revision=1,
        event_time=datetime.combine(period, time.min, UTC),
        available_at=capture.reviewed_at,
        ingested_at=capture.reviewed_at,
        payload=release.model_dump(mode="json", exclude={"schema_version"})
        | {"availability_basis": "local_document_review", "source_sha256": capture.source.sha256},
    )


def _market_records(capture: MarketCapture, response: JsonValue) -> list[ArchiveRecord]:
    records = []
    for item in MarketResponse.model_validate(response).records:
        if item.available_at > capture.captured_at:
            raise ValueError("market response was captured before publication")
        records.append(
            ArchiveRecord(
                kind=item.kind,
                provider=capture.provider,
                record_id=item.record_id,
                symbol=item.symbol,
                revision=item.revision,
                event_time=item.event_time,
                available_at=item.available_at,
                ingested_at=capture.captured_at,
                payload=item.payload.model_dump(mode="json", exclude_none=True)
                | {"source_sha256": capture.source.sha256},
            )
        )
    return records


def _market_revisions(records: list[ArchiveRecord]) -> list[ArchiveRecord]:
    ordered = sorted(records, key=lambda item: item.revision)
    for earlier, later in pairwise(ordered):
        if earlier.revision == later.revision:
            raise ValueError("duplicate market record revision")
        if earlier.available_at > later.available_at:
            raise ValueError("market publications must follow revision order")
        if earlier.kind == "bar" and earlier.event_time != later.event_time:
            raise ValueError("revised bars must keep their original close time")
    return ordered


def build_archive(root: Path, manifest: JsonValue) -> dict:
    inputs = CaptureManifest.model_validate(manifest)
    by_key: dict[tuple[str, str, str, str], list[ArchiveRecord]] = defaultdict(list)
    market: dict[tuple[str, str], list[ArchiveRecord]] = defaultdict(list)
    total = 0
    for capture in inputs.captures:
        raw = read_pinned_json(root, capture.source)
        if isinstance(capture, SECCapture):
            records = _sec_records(capture, raw)
        elif isinstance(capture, ALFREDCapture):
            records = _alfred_records(capture, raw)
        elif isinstance(capture, BLSCapture):
            records = [_bls_record(capture, raw)]
        else:
            records = _market_records(capture, raw)
        total += len(records)
        if total > MAX_RECORDS:
            raise ValueError("combined archive has too many observations")
        for item in records:
            if isinstance(capture, MarketCapture):
                market[item.provider, item.record_id].append(item)
            else:
                by_key[item.key()].append(item)
    output = []
    for _, revisions in sorted(by_key.items()):
        if len(revisions) > 1:
            provider = revisions[0].provider
            if revisions[0].kind == "filing":
                raise ValueError("duplicate SEC accession in the requested archive")
            vintage_key = "released_at" if provider == "bls" else "vintage_date"
            if provider == "bls":
                revisions.sort(key=lambda row: _utc(str(row.payload["released_at"])))
            else:
                revisions.sort(key=lambda row: str(row.payload["vintage_date"]))
            for earlier, later in pairwise(revisions):
                if earlier.payload[vintage_key] == later.payload[vintage_key]:
                    raise ValueError(
                        f"duplicate {provider.upper()} observation in the same vintage"
                    )
                if earlier.available_at > later.available_at:
                    raise ValueError(f"{provider.upper()} capture times must follow vintage order")
        for revision, item in enumerate(revisions, start=1):
            output.append(item.model_copy(update={"revision": revision}))
    for revisions in market.values():
        identities = {
            (item.kind, item.symbol, item.payload.get("security_id")) for item in revisions
        }
        if len(identities) != 1:
            raise ValueError("market record revisions must preserve instrument and record identity")
        output.extend(_market_revisions(revisions))
    output.sort(key=lambda item: (item.key(), item.revision))
    return {
        "schema_version": "0.1",
        "source_mode": inputs.source_mode,
        "records": [item.model_dump(mode="json") for item in output],
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    schema = parser.add_mutually_exclusive_group()
    schema.add_argument("--schema", action="store_true", help="Output the capture-manifest schema")
    schema.add_argument(
        "--market-schema", action="store_true", help="Output the market input schema"
    )
    schema.add_argument("--bls-schema", action="store_true", help="Output the BLS review schema")
    parser.add_argument("--root", type=Path, help="Root for pinned local source paths")
    parser.add_argument("--manifest", type=Path, help="Capture-manifest JSON")
    args = parser.parse_args()
    if args.schema:
        print(json.dumps(CaptureManifest.model_json_schema(), indent=2))
        return 0
    if args.market_schema:
        print(json.dumps(MarketResponse.model_json_schema(), indent=2))
        return 0
    if args.bls_schema:
        print(json.dumps(BLSReleaseReview.model_json_schema(), indent=2))
        return 0
    if args.root is None or args.manifest is None:
        parser.error("--root and --manifest are required")
    try:
        with args.manifest.open("rb") as stream:
            raw = stream.read(MAX_ARCHIVE_BYTES + 1)
        if len(raw) > MAX_ARCHIVE_BYTES:
            raise ValueError("capture manifest exceeds the per-document byte limit")
        archive = build_archive(args.root, decode_pinned_json(raw))
        output = json.dumps(archive, separators=(",", ":"))
        if len(output) + 2 > MAX_ARCHIVE_BYTES:
            raise ValueError("resulting archive exceeds the per-document byte limit")
    except (OSError, ValueError, RecursionError) as exc:
        print(f"archive import failed ({type(exc).__name__})", file=sys.stderr)
        return 2
    print(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
