"""Build a point-in-time archive from pinned local SEC and ALFRED JSON responses."""

import argparse
import json
import re
import sys
from collections import defaultdict
from datetime import UTC, date, datetime, time
from itertools import pairwise
from pathlib import Path
from typing import Annotated, Literal

from pydantic import Field, JsonValue

from tradingbot.contracts import Contract, Symbol
from tradingbot.snapshots import (
    MAX_ARCHIVE_BYTES,
    ArchiveRecord,
    ArchiveReference,
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


class CaptureManifest(Contract):
    schema_version: Literal["0.1"]
    source_mode: SourceMode
    captures: Annotated[
        tuple[Annotated[SECCapture | ALFREDCapture, Field(discriminator="kind")], ...],
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


def build_archive(root: Path, manifest: JsonValue) -> dict:
    inputs = CaptureManifest.model_validate(manifest)
    by_key: dict[tuple[str, str, str, str], list[ArchiveRecord]] = defaultdict(list)
    total = 0
    for capture in inputs.captures:
        raw = read_pinned_json(root, capture.source)
        if isinstance(capture, SECCapture):
            records = _sec_records(capture, raw)
        else:
            records = _alfred_records(capture, raw)
        total += len(records)
        if total > MAX_RECORDS:
            raise ValueError("combined archive has too many observations")
        for item in records:
            by_key[item.key()].append(item)
    output = []
    for _, revisions in sorted(by_key.items()):
        if len(revisions) > 1:
            if revisions[0].kind == "filing":
                raise ValueError("duplicate SEC accession in the requested archive")
            revisions.sort(key=lambda row: row.payload["vintage_date"])
            for earlier, later in pairwise(revisions):
                if earlier.payload["vintage_date"] == later.payload["vintage_date"]:
                    raise ValueError("duplicate ALFRED observation in the same vintage")
                if earlier.available_at > later.available_at:
                    raise ValueError("ALFRED capture times must follow vintage order")
        for revision, item in enumerate(revisions, start=1):
            output.append(item.model_copy(update={"revision": revision}).model_dump(mode="json"))
    return {"schema_version": "0.1", "source_mode": inputs.source_mode, "records": output}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", action="store_true", help="Output the capture-manifest schema")
    parser.add_argument("--root", type=Path, help="Root for pinned local source paths")
    parser.add_argument("--manifest", type=Path, help="Capture-manifest JSON")
    args = parser.parse_args()
    if args.schema:
        print(json.dumps(CaptureManifest.model_json_schema(), indent=2))
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
