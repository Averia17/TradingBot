"""Validate a local archive and select observations available at a specified time."""

import argparse
import hashlib
import json
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
    field_validator,
    model_validator,
)

from tradingbot.contracts import Contract, Symbol
from tradingbot.paper_memory import PAPER_KIND, validate_record

MAX_ARCHIVE_BYTES = 16 * 1024 * 1024
Identifier = Annotated[str, Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")]
RecordKind = Literal["bar", "news", "filing", "action", "macro", "paper_outcome"]
SourceMode = Literal["synthetic_fixture", "local_archive"]
Visibility = Literal["source_published", "locally_ingested"]
_symbols = TypeAdapter(tuple[Symbol, ...])


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


def _digest(value: Contract) -> str:
    encoded = json.dumps(value.model_dump(mode="json"), sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


class ArchiveReference(Contract):
    """Reference shape shared with the research job's data_manifest field."""

    ref: Annotated[str, Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$")]
    sha256: Annotated[str, Field(pattern=r"^[0-9a-f]{64}$")]

    @field_validator("ref")
    @classmethod
    def relative_file(cls, value: str) -> str:
        if any(part in {"", ".", ".."} for part in value.split("/")):
            raise ValueError("archive reference must use literal relative path components")
        return value


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


def read_pinned_json(root: Path, reference: ArchiveReference) -> JsonValue:
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
    return decode_pinned_json(raw)


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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--ref", required=True, help="Relative file under the archive root")
    parser.add_argument("--sha256", required=True, help="Expected SHA-256 of exact file bytes")
    parser.add_argument("--as-of", type=_utc, required=True, help="ISO timestamp with timezone")
    parser.add_argument(
        "--visibility", choices=("source_published", "locally_ingested"), default="source_published"
    )
    parser.add_argument("--symbol", action="append", help="Limit to a symbol; repeat as needed")
    args = parser.parse_args()
    try:
        snapshot = load_snapshot(
            args.root,
            {"ref": args.ref, "sha256": args.sha256},
            args.as_of,
            symbols=args.symbol,
            visibility=args.visibility,
        )
    except ValidationError:
        parser.exit(2, "archive rejected: invalid archive or manifest schema\n")
    except (OSError, ValueError, RecursionError) as exc:
        parser.exit(2, f"archive rejected: {exc}\n")
    payload = snapshot.model_dump(mode="json") | {"snapshot_sha256": snapshot.sha256}
    print(json.dumps(payload, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
