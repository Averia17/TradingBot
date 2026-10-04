"""Financial contracts. All intents in this milestone are research-only."""

from datetime import datetime
from decimal import Decimal
from typing import Annotated, Literal, Self
from uuid import UUID

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

Money = Annotated[Decimal, Field(ge=0, max_digits=20, decimal_places=8)]
Symbol = Annotated[str, Field(pattern=r"^[A-Z][A-Z0-9.\-]{0,15}$")]
NonEmpty = Annotated[str, Field(min_length=1)]


class Contract(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False, frozen=True)


class Position(Contract):
    symbol: Symbol
    instrument_id: NonEmpty
    quantity: Annotated[Decimal, Field(gt=0, max_digits=20, decimal_places=8)]
    mark_price_usd: Annotated[Decimal, Field(gt=0, max_digits=20, decimal_places=8)]

    @property
    def value_usd(self) -> Decimal:
        return self.quantity * self.mark_price_usd


class PortfolioSnapshot(Contract):
    snapshot_id: UUID
    as_of: datetime
    currency: Literal["USD"] = "USD"
    cash_asset: Literal["USDC", "USD"] = "USDC"
    cash_usd: Money
    reserved_cash_usd: Money = Decimal("0")
    max_position_pct: Annotated[Decimal, Field(gt=0, le=1)] = Decimal("0.25")
    positions: tuple[Position, ...] = ()

    @field_validator("as_of")
    @classmethod
    def timezone_required(cls, value: datetime) -> datetime:
        if value.utcoffset() is None:
            raise ValueError("as_of must include a timezone")
        return value

    @model_validator(mode="after")
    def coherent_book(self) -> Self:
        if self.reserved_cash_usd > self.cash_usd:
            raise ValueError("reserved cash exceeds cash balance")
        if len({p.symbol for p in self.positions}) != len(self.positions):
            raise ValueError("duplicate positions; consolidate by underlying before analysis")
        return self

    @property
    def equity_usd(self) -> Decimal:
        return self.cash_usd + sum((p.value_usd for p in self.positions), Decimal("0"))

    @property
    def available_cash_usd(self) -> Decimal:
        return self.cash_usd - self.reserved_cash_usd


class Horizon(Contract):
    min_hours: Annotated[int, Field(gt=0, strict=True)]
    max_hours: Annotated[int, Field(gt=0, strict=True)]

    @model_validator(mode="after")
    def ordered(self) -> Self:
        if self.min_hours > self.max_hours:
            raise ValueError("minimum horizon exceeds maximum")
        return self


class IntentProposal(Contract):
    symbol: Symbol
    action: Literal["BUY", "SELL", "HOLD", "ADD", "REDUCE"]
    notional_usd: Money
    confidence: Annotated[float, Field(ge=0, le=1, strict=True)] | None
    expected_return_pct: Annotated[float, Field(strict=True)] | None
    horizon: Horizon
    thesis: Annotated[tuple[NonEmpty, ...], Field(min_length=1)]
    invalidation_conditions: Annotated[tuple[NonEmpty, ...], Field(min_length=1)]
    evidence_ids: tuple[NonEmpty, ...]
    max_execution_cost_bps: Annotated[int, Field(ge=0, le=1000, strict=True)]

    @model_validator(mode="after")
    def action_amount(self) -> Self:
        if self.action == "HOLD":
            if self.notional_usd != 0:
                raise ValueError("HOLD must have zero notional")
        elif self.notional_usd <= 0 or self.confidence is None or self.expected_return_pct is None:
            raise ValueError("non-HOLD requires positive notional and explicit model estimates")
        return self


class IntentProposals(Contract):
    intents: Annotated[tuple[IntentProposal, ...], Field(min_length=1)]


class TradeIntent(IntentProposal):
    decision_id: UUID


class DecisionBatch(Contract):
    schema_version: Literal["0.1"] = "0.1"
    execution_mode: Literal["research"] = "research"
    run_id: UUID
    portfolio_snapshot_id: UUID
    as_of: datetime
    created_at: datetime
    expires_at: datetime
    evidence_hashes: dict[str, str]
    intents: tuple[TradeIntent, ...]


def validate_proposals(
    proposals: list[IntentProposal] | tuple[IntentProposal, ...],
    portfolio: PortfolioSnapshot,
    symbols: list[str] | tuple[str, ...],
) -> None:
    """Reject inconsistent proposals; never silently turn them into another action."""
    proposed = [p.symbol for p in proposals]
    if len(set(proposed)) != len(proposed):
        raise ValueError("duplicate decisions for the same underlying")
    if set(proposed) != set(symbols):
        raise ValueError("decision symbols must exactly match requested symbols")
    holdings = {p.symbol: p for p in portfolio.positions}
    total_buys = Decimal("0")
    for intent in proposals:
        held = holdings.get(intent.symbol)
        if intent.action == "HOLD":
            continue
        if not intent.evidence_ids:
            raise ValueError("non-HOLD requires evidence")
        if intent.action == "BUY" and held is not None:
            raise ValueError("BUY for a held asset must be ADD")
        if intent.action in {"ADD", "SELL", "REDUCE"} and held is None:
            raise ValueError("action requires a held position")
        if intent.action in {"BUY", "ADD"}:
            existing = held.value_usd if held else Decimal("0")
            if existing + intent.notional_usd > portfolio.equity_usd * portfolio.max_position_pct:
                raise ValueError("position concentration limit exceeded")
            total_buys += intent.notional_usd
        elif held is not None:
            if intent.action == "SELL" and intent.notional_usd != held.value_usd:
                raise ValueError("SELL must close the full held marked value; otherwise REDUCE")
            if intent.action == "REDUCE" and intent.notional_usd >= held.value_usd:
                raise ValueError("REDUCE must be smaller than the held marked value")
    if total_buys > portfolio.available_cash_usd:
        raise ValueError("batch exceeds available cash; future sale proceeds are unavailable")
