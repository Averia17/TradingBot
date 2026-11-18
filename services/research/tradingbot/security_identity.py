"""Project archived research by an independently pinned stable security identity."""

from collections import defaultdict
from collections.abc import Iterable, Sequence
from datetime import datetime
from pathlib import Path
from typing import Annotated, Literal, Self

from pydantic import Field, model_validator

from tradingbot.contracts import Contract, Position, Symbol
from tradingbot.paper_memory import PAPER_KIND, PaperInstrument
from tradingbot.snapshots import (
    ArchiveRecord,
    ArchiveReference,
    CaptureTime,
    Identifier,
    PointInTimeSnapshot,
    _project_universe,
    load_snapshot,
    read_pinned_json,
)


class SecurityBinding(Contract):
    symbol: Symbol
    security_id: Identifier


class RegistryEntry(SecurityBinding):
    available_at: CaptureTime
    effective_at: CaptureTime
    expires_at: CaptureTime | None = None
    record_providers: Annotated[tuple[Identifier, ...], Field(max_length=32)]
    sec_cik: Annotated[str, Field(pattern=r"^[0-9]{10}$")] | None = None
    instrument: PaperInstrument | None = None

    @model_validator(mode="after")
    def coherent_sources(self) -> Self:
        if self.expires_at is not None and self.expires_at <= self.effective_at:
            raise ValueError("identity expiry must follow its effective time")
        if len(set(self.record_providers)) != len(self.record_providers):
            raise ValueError("duplicate provider in an identity registry entry")
        return self

    def active_at(self, cutoff: datetime) -> bool:
        return (
            self.available_at <= cutoff
            and self.effective_at <= cutoff
            and (self.expires_at is None or cutoff < self.expires_at)
        )


class SecurityRegistry(Contract):
    schema_version: Literal["0.1"]
    universe_provider: Identifier
    entries: Annotated[tuple[RegistryEntry, ...], Field(min_length=1, max_length=1000)]


class StablePointInTimeSnapshot(PointInTimeSnapshot):
    schema_version: Literal["0.2"] = "0.2"
    identity_registry: ArchiveReference
    security_bindings: tuple[SecurityBinding, ...]


class IdentityProjectionError(ValueError):
    def __init__(self, reason: str, binding: SecurityBinding):
        super().__init__(f"{reason}: {binding.symbol}")
        self.reason = reason


def load_identity_registry(root: Path, reference: ArchiveReference | dict) -> SecurityRegistry:
    pin = ArchiveReference.model_validate(reference)
    return SecurityRegistry.model_validate(read_pinned_json(root, pin))


def _resolve_entry(
    entries: tuple[RegistryEntry, ...], binding: SecurityBinding, cutoff: datetime
) -> RegistryEntry:
    scope = [
        entry
        for entry in entries
        if (entry.security_id == binding.security_id or entry.symbol == binding.symbol)
        and entry.active_at(cutoff)
    ]
    if len(scope) > 1:
        raise IdentityProjectionError("identity_registry_ambiguous", binding)
    if (
        not scope
        or scope[0].security_id != binding.security_id
        or scope[0].symbol != binding.symbol
    ):
        raise IdentityProjectionError("identity_registry_entry_missing", binding)
    return scope[0]


def _matches(record: ArchiveRecord, entry: RegistryEntry) -> bool:
    if record.event_time < entry.effective_at:
        return False
    if record.kind == PAPER_KIND:
        return entry.instrument is not None and record.payload.get("instrument") == (
            entry.instrument.model_dump(mode="json")
        )
    identity = record.payload.get("security_id")
    if record.kind == "filing" and record.provider == "sec-edgar":
        return (
            entry.sec_cik is not None
            and record.payload.get("cik") == entry.sec_cik
            and (identity is None or identity == entry.security_id)
        )
    return (
        record.kind in {"action", "bar", "filing", "news"}
        and record.provider in entry.record_providers
        and identity == entry.security_id
    )


def project_identity_snapshot(
    snapshot: PointInTimeSnapshot,
    registry: SecurityRegistry,
    registry_reference: ArchiveReference | dict,
    bindings: Sequence[SecurityBinding | dict],
    *,
    positions: Iterable[Position] = (),
) -> StablePointInTimeSnapshot:
    """Select exact identities from an unfiltered locally ingested snapshot."""
    pin = ArchiveReference.model_validate(registry_reference)
    if pin.ref == snapshot.archive.ref or pin.sha256 == snapshot.archive.sha256:
        raise ValueError("identity registry and market archive require independent pins")
    if snapshot.symbols is not None:
        raise ValueError("identity projection requires an unfiltered snapshot")
    bindings = tuple(SecurityBinding.model_validate(value) for value in bindings)
    if (
        not bindings
        or len({binding.symbol for binding in bindings}) != len(bindings)
        or len({binding.security_id for binding in bindings}) != len(bindings)
    ):
        raise ValueError("security bindings require distinct symbols and security IDs")

    known = tuple(record for record in snapshot.records if record.ingested_at <= snapshot.as_of)
    universe = _project_universe(
        snapshot.model_copy(update={"records": known}), registry.universe_provider
    )
    roster = {entry.security_id: entry for entry in universe.entries}
    live: dict[str, set[str]] = defaultdict(set)
    for state in universe.entries:
        if state.status == "listed" and state.evidence is not None:
            live[state.evidence.symbol].add(state.security_id)
    by_symbol: dict[str, list[ArchiveRecord]] = defaultdict(list)
    selected = []
    for record in known:
        if record.symbol is None:
            if record.kind == "macro" and record.payload.get("security_id") is None:
                selected.append(record)
        else:
            by_symbol[record.symbol].append(record)
    held: dict[str, set[str]] = defaultdict(set)
    for position in positions:
        held[position.symbol].add(position.instrument_id)
    instrument_history: dict[str, set[tuple[str, PaperInstrument]]] = defaultdict(set)
    for entry in registry.entries:
        if (
            entry.instrument is not None
            and entry.available_at <= snapshot.as_of
            and entry.effective_at <= snapshot.as_of
        ):
            instrument_history[entry.instrument.instrument_id].add((entry.security_id, entry.instrument))

    for binding in bindings:
        entry = _resolve_entry(registry.entries, binding, snapshot.as_of)
        state = roster.get(binding.security_id)
        if state is None or state.evidence is None:
            raise IdentityProjectionError("universe_evidence_missing", binding)
        if state.status == "delisted":
            raise IdentityProjectionError("security_delisted", binding)
        if state.evidence.symbol != binding.symbol:
            raise IdentityProjectionError("universe_symbol_mismatch", binding)
        if len(live[binding.symbol]) != 1:
            raise IdentityProjectionError("ambiguous_universe_symbol", binding)
        history = () if entry.instrument is None else instrument_history[entry.instrument.instrument_id]
        if held[binding.symbol]:
            # A portfolio position carries only instrument_id. It cannot prove
            # which issuer or ledger tuple was originally bought after reuse.
            if (
                entry.instrument is None
                or held[binding.symbol] != {entry.instrument.instrument_id}
                or history != {(entry.security_id, entry.instrument)}
            ):
                raise IdentityProjectionError("ambiguous_held_security", binding)
        # A sale observation carries the full ledger tuple, but cannot prove
        # which issuer its lots belonged to if the exact tuple was reused.
        reused_tuple = any(
            instrument == entry.instrument and security_id != entry.security_id
            for security_id, instrument in history
        )
        selected.append(state.evidence)
        selected.extend(
            record
            for record in by_symbol[binding.symbol]
            if (record.kind != PAPER_KIND or not reused_tuple) and _matches(record, entry)
        )

    return StablePointInTimeSnapshot(
        source_mode=snapshot.source_mode,
        archive=snapshot.archive,
        as_of=snapshot.as_of,
        symbols=tuple(binding.symbol for binding in bindings),
        records=tuple(sorted(selected, key=lambda record: record.key())),
        identity_registry=pin,
        security_bindings=bindings,
    )


def load_identity_snapshot(
    root: Path,
    manifest: ArchiveReference | dict,
    registry_reference: ArchiveReference | dict,
    as_of: datetime,
    bindings: Sequence[SecurityBinding | dict],
    *,
    positions: Iterable[Position] = (),
) -> StablePointInTimeSnapshot:
    registry = load_identity_registry(root, registry_reference)
    snapshot = load_snapshot(root, manifest, as_of, visibility="locally_ingested")
    return project_identity_snapshot(
        snapshot, registry, registry_reference, bindings, positions=positions
    )
