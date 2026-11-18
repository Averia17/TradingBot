"""Resolve a synthetic replay's native keys through the independently pinned registry."""

import json
from dataclasses import dataclass
from datetime import datetime
from hashlib import sha256
from typing import Annotated, Literal

import msgspec

from tradingbot_evaluation.replay import MAX_PREFIX_SYMBOLS, Replay, Ticker, _unique_fields

MAX_IDENTITY_REGISTRY_BYTES = 2 * 1024 * 1024
Hash = Annotated[str, msgspec.Meta(pattern=r"^[a-f0-9]{64}$")]
SecurityID = Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")]
DisplaySymbol = Annotated[str, msgspec.Meta(pattern=r"^[A-Z][A-Z0-9.-]{0,15}$")]
_InstrumentField = Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$")]
_RegistryTime = Annotated[
    str,
    msgspec.Meta(
        pattern=r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,6})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$"
    ),
]
_CIK = Annotated[str, msgspec.Meta(pattern=r"^[0-9]{10}$")]


class Reference(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    ref: Annotated[str, msgspec.Meta(pattern=r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$")]
    sha256: Hash

    def valid_path(self) -> bool:
        return all(part not in {"", ".", ".."} for part in self.ref.split("/"))


class SecurityBinding(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    symbol: DisplaySymbol
    security_id: SecurityID


class ExactInstrument(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    instrument_id: _InstrumentField
    registry_version: _InstrumentField
    decimals: Annotated[int, msgspec.Meta(ge=0, le=18)]


class ReplayInstrument(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    key: Ticker
    security_id: SecurityID
    instrument: ExactInstrument


class IdentityTape(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.2"]
    identity_registry: Reference
    instruments: Annotated[
        tuple[ReplayInstrument, ...], msgspec.Meta(min_length=1, max_length=MAX_PREFIX_SYMBOLS)
    ]
    replay: dict


class _RegistryEntry(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    symbol: DisplaySymbol
    security_id: SecurityID
    available_at: _RegistryTime
    effective_at: _RegistryTime
    record_providers: Annotated[tuple[SecurityID, ...], msgspec.Meta(max_length=32)]
    expires_at: _RegistryTime | None = None
    sec_cik: _CIK | None = None
    instrument: ExactInstrument | None = None


class _Registry(msgspec.Struct, frozen=True, forbid_unknown_fields=True):
    schema_version: Literal["0.1"]
    universe_provider: SecurityID
    entries: Annotated[tuple[_RegistryEntry, ...], msgspec.Meta(min_length=1, max_length=1000)]


@dataclass(frozen=True)
class _RegistryWindow:
    entry: _RegistryEntry
    available_at: datetime
    effective_at: datetime
    expires_at: datetime | None

    def active_at(self, cutoff: datetime) -> bool:
        return (
            self.available_at <= cutoff
            and self.effective_at <= cutoff
            and (self.expires_at is None or cutoff < self.expires_at)
        )


@dataclass(frozen=True)
class ActiveInstrument:
    mapping: ReplayInstrument
    effective_at: datetime
    expires_at: datetime | None


@dataclass(frozen=True)
class ReplayIdentity:
    registry: Reference
    instruments: tuple[ReplayInstrument, ...]
    _by_security: dict[str, ReplayInstrument]
    _by_instrument_id: dict[str, ReplayInstrument]
    _windows: tuple[_RegistryWindow, ...]

    def resolve(self, binding: SecurityBinding, cutoff: datetime) -> ActiveInstrument:
        choices = [
            row
            for row in self._windows
            if (row.entry.symbol == binding.symbol or row.entry.security_id == binding.security_id)
            and row.active_at(cutoff)
        ]
        key = self._by_security.get(binding.security_id)
        if (
            len(choices) != 1
            or choices[0].entry.symbol != binding.symbol
            or choices[0].entry.security_id != binding.security_id
            or key is None
            or choices[0].entry.instrument != key.instrument
        ):
            raise ValueError(
                "No unambiguous registry instrument for the requested identity at cutoff"
            )
        return ActiveInstrument(key, choices[0].effective_at, choices[0].expires_at)

    def holding(self, symbol: str, instrument_id: str, cutoff: datetime) -> ActiveInstrument:
        key = self._by_instrument_id.get(instrument_id)
        if key is None:
            raise ValueError("Research snapshot contains an unmapped held instrument")
        return self.resolve(SecurityBinding(symbol, key.security_id), cutoff)

    def describe_holding(self, native_key: str, cutoff: datetime) -> tuple[str, ActiveInstrument]:
        mapping = next((item for item in self.instruments if item.key == native_key), None)
        if mapping is None:
            raise ValueError("Native account contains an unmapped held instrument")
        symbols = {
            row.entry.symbol for row in self._windows
            if row.entry.security_id == mapping.security_id and row.active_at(cutoff)
        }
        if len(symbols) != 1:
            raise ValueError("Native account holds a security without unambiguous point-in-time eligibility")
        symbol = symbols.pop()
        return symbol, self.resolve(SecurityBinding(symbol, mapping.security_id), cutoff)

    def validate_tape_keys(self, replay: Replay) -> None:
        calendar = replay.execution_calendar
        records = (
            *replay.quotes,
            *replay.cash_dividends,
            *replay.stock_splits,
            *(() if calendar is msgspec.UNSET else calendar.windows),
        )
        if {row.symbol for row in records} - {instrument.key for instrument in self.instruments}:
            raise ValueError("Synthetic identity tape contains an undeclared replay instrument key")


def identity_profile(
    tape: IdentityTape, raw: bytes | None, expected_sha256: str | None
) -> ReplayIdentity:
    if raw is None or expected_sha256 is None:
        raise ValueError("Identity replay requires independently pinned identity registry bytes")
    if len(raw) > MAX_IDENTITY_REGISTRY_BYTES:
        raise ValueError("Identity registry exceeds the byte limit")
    actual = sha256(raw).hexdigest()
    if actual != expected_sha256 or actual != tape.identity_registry.sha256:
        raise ValueError("Identity registry SHA-256 does not match the owner and tape pins")
    if not tape.identity_registry.valid_path():
        raise ValueError("Identity registry must have a local relative reference")
    registry = msgspec.convert(json.loads(raw, object_pairs_hook=_unique_fields), type=_Registry)
    windows = tuple(
        _RegistryWindow(
            row,
            datetime.fromisoformat(row.available_at),
            datetime.fromisoformat(row.effective_at),
            None if row.expires_at is None else datetime.fromisoformat(row.expires_at),
        )
        for row in registry.entries
    )
    if any(
        (row.expires_at is not None and row.expires_at <= row.effective_at)
        or len(set(row.entry.record_providers)) != len(row.entry.record_providers)
        for row in windows
    ):
        raise ValueError("Identity registry contains an invalid time window or duplicate provider")
    by_security = {item.security_id: item for item in tape.instruments}
    by_instrument = {item.instrument.instrument_id: item for item in tape.instruments}
    if len({item.key for item in tape.instruments}) != len(by_security) or not (
        len(by_security) == len(by_instrument) == len(tape.instruments)
    ):
        raise ValueError("Identity tape requires distinct security, instrument and native keys")
    for item in tape.instruments:
        mapped = {
            (row.security_id, row.instrument)
            for row in registry.entries
            if row.instrument is not None
            and row.instrument.instrument_id == item.instrument.instrument_id
        }
        if mapped != {(item.security_id, item.instrument)}:
            raise ValueError(
                "Identity tape instrument differs from the exact owner registry binding"
            )
    return ReplayIdentity(
        tape.identity_registry, tape.instruments, by_security, by_instrument, windows
    )
