"""Validate a local archive and select observations available at a specified time."""

import argparse
import hashlib
import json
import re
from collections.abc import Iterable
from datetime import UTC, datetime
from pathlib import Path
from typing import Annotated, Literal, Never, Self

from pydantic import (
    BeforeValidator,
    Field,
    JsonValue,
    TypeAdapter,
    ValidationError,
    WithJsonSchema,
    field_validator,
    model_validator,
)

from tradingbot.contracts import Contract, Symbol
from tradingbot.paper_memory import PAPER_KIND, validate_record

MAX_ARCHIVE_BYTES = 16 * 1024 * 1024
Identifier = Annotated[str, Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")]
SHA256 = Annotated[str, Field(pattern=r"^[0-9a-f]{64}$")]
RecordKind = Literal["bar", "news", "filing", "action", "macro", "paper_outcome", "listing"]
SourceMode = Literal["synthetic_fixture", "local_archive"]
Visibility = Literal["source_published", "locally_ingested"]
_symbols = TypeAdapter(tuple[Symbol, ...])
_identifiers = TypeAdapter(tuple[Identifier, ...])
_identifier = TypeAdapter(Identifier)


def _utc(value: datetime | str) -> datetime:
    if isinstance(value, str):
        value = datetime.fromisoformat(value)
    if not isinstance(value, datetime) or value.utcoffset() is None:
        raise ValueError("timestamp must include a timezone")
    try:
        return value.astimezone(UTC)
    except OverflowError:
        raise ValueError("timestamp is outside the supported UTC range") from None


UTCDateTime = Annotated[datetime, BeforeValidator(_utc)]
_CAPTURE_TIME_PATTERN = (
    r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}"
    r"(?:\.[0-9]{1,6})?(?:Z|[+-][0-9]{2}:[0-9]{2})$"
)


def _capture_time(value: datetime | str) -> datetime:
    if isinstance(value, str) and re.fullmatch(_CAPTURE_TIME_PATTERN, value) is None:
        raise ValueError("capture timestamps require a timezone and at most six fractional digits")
    return _utc(value)


CaptureTime = Annotated[
    datetime,
    BeforeValidator(_capture_time),
    WithJsonSchema({"type": "string", "format": "date-time", "pattern": _CAPTURE_TIME_PATTERN}),
]


def _digest(value: Contract) -> str:
    encoded = json.dumps(value.model_dump(mode="json"), sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


class ArchiveReference(Contract):
    """Reference shape shared with the research job's data_manifest field."""

    ref: Annotated[str, Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$")]
    sha256: SHA256

    @field_validator("ref")
    @classmethod
    def relative_file(cls, value: str) -> str:
        if any(part in {"", ".", ".."} for part in value.split("/")):
            raise ValueError("archive reference must use literal relative path components")
        return value


class Listing(Contract):
    security_id: Identifier
    status: Literal["listed", "delisted"]
    effective_at: CaptureTime


class ArchivedListing(Listing):
    source_sha256: SHA256


class ArchiveRecord(Contract):
    kind: RecordKind
    provider: Identifier
    record_id: Identifier
    symbol: Symbol | None
    revision: Annotated[int, Field(strict=True, ge=1)]
    event_time: UTCDateTime
    available_at: UTCDateTime
    ingested_at: UTCDateTime
    payload: dict[str, JsonValue]

    @model_validator(mode="after")
    def ordered_times(self) -> Self:
        if not self.event_time <= self.available_at <= self.ingested_at:
            raise ValueError("timestamps must satisfy event_time <= available_at <= ingested_at")
        if self.kind == "bar" and self.symbol is None:
            raise ValueError("bar records require a symbol")
        if self.kind == "listing":
            if self.symbol is None:
                raise ValueError("listing records require a symbol")
            ArchivedListing.model_validate(self.payload)
        validate_record(self)
        return self

    def key(self) -> tuple[str, str, str, str]:
        return self.kind, self.provider, self.symbol or "", self.record_id

    @property
    def sha256(self) -> str:
        return _digest(self)


class _Archive(Contract):
    schema_version: Literal["0.1", "0.2"]
    source_mode: SourceMode
    records: tuple[ArchiveRecord, ...]

    @model_validator(mode="after")
    def coherent_revisions(self) -> Self:
        previous: dict[tuple[str, str, str, str], ArchiveRecord] = {}
        ledger_ids = set()
        paper_account = None
        for item in sorted(self.records, key=lambda record: (record.key(), record.revision)):
            if item.kind == PAPER_KIND:
                if self.schema_version != "0.2":
                    raise ValueError("paper outcomes require archive version 0.2")
                account = item.payload["account_id"]
                if paper_account is not None and paper_account != account:
                    raise ValueError("archive contains multiple paper accounts")
                paper_account = account
                if item.record_id in ledger_ids:
                    raise ValueError("duplicate paper outcome")
                ledger_ids.add(item.record_id)
            key = item.key()
            older = previous.get(key)
            if older is not None:
                if item.revision == older.revision:
                    raise ValueError("duplicate record revision")
                if item.available_at < older.available_at:
                    raise ValueError("revision timestamps must follow revision order")
            previous[key] = item
        return self


class PointInTimeSnapshot(Contract):
    schema_version: Literal["0.1"] = "0.1"
    source_mode: SourceMode
    archive: ArchiveReference
    as_of: UTCDateTime
    symbols: tuple[Symbol, ...] | None
    records: tuple[ArchiveRecord, ...]

    @property
    def sha256(self) -> str:
        return _digest(self)


class UniverseEntry(Contract):
    security_id: Identifier
    status: Literal["listed", "delisted", "missing"]
    evidence: ArchiveRecord | None


class PointInTimeUniverse(Contract):
    schema_version: Literal["0.1"] = "0.1"
    source_mode: SourceMode
    archive: ArchiveReference
    as_of: UTCDateTime
    provider: Identifier
    entries: tuple[UniverseEntry, ...]


def _unique_keys(pairs: list[tuple[str, JsonValue]]) -> dict[str, JsonValue]:
    value = dict(pairs)
    if len(value) != len(pairs):
        raise ValueError("duplicate JSON field in archive")
    return value


def _reject_fraction(_: str) -> Never:
    raise ValueError("archive numbers with fractions must use exact decimal strings")


def decode_pinned_json(raw: bytes) -> JsonValue:
    return json.loads(
        raw.decode("utf-8-sig"),
        object_pairs_hook=_unique_keys,
        parse_float=_reject_fraction,
        parse_constant=_reject_fraction,
    )


def read_pinned_bytes(root: Path, reference: ArchiveReference) -> bytes:
    root = root.resolve(strict=True)
    path = (root / reference.ref).resolve(strict=True)
    if not path.is_relative_to(root):
        raise ValueError("archive reference resolves outside the archive root")
    if not path.is_file():
        raise ValueError("archive reference must resolve to a regular file")
    with path.open("rb") as stream:
        raw = stream.read(MAX_ARCHIVE_BYTES + 1)
    if len(raw) > MAX_ARCHIVE_BYTES:
        raise ValueError("archive exceeds the per-document byte limit")
    if hashlib.sha256(raw).hexdigest() != reference.sha256:
        raise ValueError("archive SHA-256 mismatch")
    return raw


def read_pinned_json(root: Path, reference: ArchiveReference) -> JsonValue:
    return decode_pinned_json(read_pinned_bytes(root, reference))


def load_snapshot(
    root: Path,
    manifest: ArchiveReference | dict,
    as_of: datetime,
    *,
    symbols: Iterable[str] | None = None,
    visibility: Visibility = "source_published",
) -> PointInTimeSnapshot:
    """Read verified local bytes and select the latest available revision of each fact.

    An empty symbol filter retains only records explicitly marked as general.
    Corporate-action announcements use event_time for announcement time and
    carry any later effective timestamp inside payload. Historical inspection
    uses source publication; the worker also requires local receipt before cutoff.
    """
    cutoff = _utc(as_of)
    if visibility not in ("source_published", "locally_ingested"):
        raise ValueError("unknown archive visibility")
    reference = ArchiveReference.model_validate(manifest)
    universe = None if symbols is None else tuple(sorted(set(_symbols.validate_python(symbols))))
    archive = _Archive.model_validate(read_pinned_json(root, reference))
    selected: dict[tuple[str, str, str, str], ArchiveRecord] = {}
    for item in archive.records:
        locally_known = visibility == "locally_ingested" or item.kind == PAPER_KIND
        known_at = item.ingested_at if locally_known else item.available_at
        if known_at <= cutoff and (
            universe is None or item.symbol is None or item.symbol in universe
        ):
            key = item.key()
            prior = selected.get(key)
            if prior is None or prior.revision < item.revision:
                selected[key] = item
    return PointInTimeSnapshot(
        source_mode=archive.source_mode,
        archive=reference,
        as_of=cutoff,
        symbols=universe,
        records=tuple(item for _, item in sorted(selected.items())),
    )


def load_universe(
    root: Path,
    manifest: ArchiveReference | dict,
    as_of: datetime,
    provider: str,
    *,
    security_ids: Iterable[str] | None = None,
) -> PointInTimeUniverse:
    """Resolve effective listing status using only revisions received by cutoff."""
    snapshot = load_snapshot(root, manifest, as_of, visibility="locally_ingested")
    return _project_universe(snapshot, provider, security_ids)


def _project_universe(
    snapshot: PointInTimeSnapshot,
    provider: str,
    security_ids: Iterable[str] | None = None,
) -> PointInTimeUniverse:
    """Project listing events from a locally ingested, unfiltered snapshot."""
    provider = _identifier.validate_python(provider)
    requested = None if security_ids is None else set(_identifiers.validate_python(security_ids))
    states: dict[str, tuple[ArchivedListing, ArchiveRecord]] = {}
    effective = set()
    for record in snapshot.records:
        if record.kind != "listing" or record.provider != provider:
            continue
        listing = ArchivedListing.model_validate(record.payload)
        key = listing.security_id, listing.effective_at
        if listing.effective_at > snapshot.as_of or (
            requested is not None and listing.security_id not in requested
        ):
            continue
        if key in effective:
            raise ValueError("ambiguous listing events for the same security and effective instant")
        effective.add(key)
        prior = states.get(listing.security_id)
        if prior is None or listing.effective_at > prior[0].effective_at:
            states[listing.security_id] = listing, record

    entries = []
    for security_id in sorted(states.keys() if requested is None else requested):
        if security_id in states:
            listing, record = states[security_id]
            entries.append(
                UniverseEntry(security_id=security_id, status=listing.status, evidence=record)
            )
        else:
            entries.append(UniverseEntry(security_id=security_id, status="missing", evidence=None))
    return PointInTimeUniverse(
        source_mode=snapshot.source_mode,
        archive=snapshot.archive,
        as_of=snapshot.as_of,
        provider=provider,
        entries=tuple(entries),
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--ref", required=True, help="Relative file under the archive root")
    parser.add_argument("--sha256", required=True, help="Expected SHA-256 of exact file bytes")
    parser.add_argument("--as-of", type=_utc, required=True, help="ISO timestamp with timezone")
    parser.add_argument("--visibility", choices=("source_published", "locally_ingested"))
    parser.add_argument("--symbol", action="append", help="Limit to a symbol; repeat as needed")
    parser.add_argument("--universe-provider", help="Project the locally known listing roster")
    parser.add_argument("--security-id", action="append", help="Limit roster by stable ID")
    args = parser.parse_args()
    if args.security_id and not args.universe_provider:
        parser.error("--security-id requires --universe-provider")
    if args.universe_provider and (args.symbol or args.visibility == "source_published"):
        parser.error("universe projections use security IDs and local receipt")
    reference = {"ref": args.ref, "sha256": args.sha256}
    try:
        if args.universe_provider:
            universe = load_universe(
                args.root,
                reference,
                args.as_of,
                args.universe_provider,
                security_ids=args.security_id,
            )
            payload = universe.model_dump(mode="json")
        else:
            snapshot = load_snapshot(
                args.root,
                reference,
                args.as_of,
                symbols=args.symbol,
                visibility=args.visibility or "source_published",
            )
            payload = snapshot.model_dump(mode="json") | {"snapshot_sha256": snapshot.sha256}
    except ValidationError:
        parser.exit(2, "archive rejected: invalid archive or manifest schema\n")
    except (OSError, ValueError, RecursionError) as exc:
        parser.exit(2, f"archive rejected: {exc}\n")
    print(json.dumps(payload, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
