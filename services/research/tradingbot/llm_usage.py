"""Local, bounded evidence from the research model callback boundary."""

from collections.abc import Mapping
from typing import Annotated, Literal, Self
from uuid import UUID

from pydantic import AwareDatetime, Field, model_validator

from tradingbot.contracts import Contract

MAX_CAPTURE_CALLS = 4096
MAX_TOKEN_COUNT = 1_000_000_000
TokenCount = Annotated[int, Field(strict=True, ge=0, le=MAX_TOKEN_COUNT)]
Identity = Annotated[str, Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9_.:/@-]{0,191}$")]
CallRole = Literal["research", "portfolio_allocator", "unknown"]
CaptureSource = Literal["synthetic_fixture", "provider_reported"]


class TokenUsage(Contract):
    input_tokens: TokenCount | None
    output_tokens: TokenCount | None
    cache_read_tokens: TokenCount | None
    cache_write_tokens: TokenCount | None
    reasoning_tokens: TokenCount | None
    usage_source: Literal["langchain_usage_metadata", "unavailable"]

    @model_validator(mode="after")
    def check_inclusive_counts(self) -> Self:
        cached = (self.cache_read_tokens or 0) + (self.cache_write_tokens or 0)
        if self.input_tokens is None:
            if self.cache_read_tokens is not None or self.cache_write_tokens is not None:
                raise ValueError("cache subsets require a known inclusive input count")
        elif cached > self.input_tokens:
            raise ValueError("cache subsets must fit within known inclusive input tokens")
        if self.reasoning_tokens is not None and (
            self.output_tokens is None or self.reasoning_tokens > self.output_tokens
        ):
            raise ValueError("reasoning must fit within known inclusive output tokens")
        unavailable = self.input_tokens is None and self.output_tokens is None
        if unavailable != (self.usage_source == "unavailable"):
            raise ValueError("usage source must distinguish missing token totals")
        return self


class LLMCall(TokenUsage):
    run_id: UUID
    provider: Identity | None
    model: Identity | None
    role: CallRole
    status: Literal["completed", "failed", "unfinished"]
    started_at: AwareDatetime

    @model_validator(mode="after")
    def check_identity(self) -> Self:
        if (self.provider is None) != (self.model is None):
            raise ValueError("provider and model identity must be present together")
        if self.status == "unfinished" and self.usage_source != "unavailable":
            raise ValueError("unfinished calls cannot declare terminal usage")
        return self


class LLMUsageReport(Contract):
    schema_version: Literal["0.1"]
    source_mode: CaptureSource
    llm_calls: Annotated[int, Field(strict=True, ge=0, le=MAX_CAPTURE_CALLS)]
    capture_status: Literal["complete", "partial"]
    calls: Annotated[tuple[LLMCall, ...], Field(max_length=MAX_CAPTURE_CALLS)]

    @model_validator(mode="after")
    def check_call_count(self) -> Self:
        if self.llm_calls != len(self.calls) or len({call.run_id for call in self.calls}) != len(
            self.calls
        ):
            raise ValueError("model invocation identifiers and counts must reconcile")
        if self.capture_status == "complete" and any(
            call.status == "unfinished" for call in self.calls
        ):
            raise ValueError("unfinished calls require partial capture")
        return self


NO_USAGE = TokenUsage(
    input_tokens=None,
    output_tokens=None,
    cache_read_tokens=None,
    cache_write_tokens=None,
    reasoning_tokens=None,
    usage_source="unavailable",
)


def _token_count(record: Mapping, key: str) -> int | None:
    value = record.get(key)
    return value if type(value) is int and 0 <= value <= MAX_TOKEN_COUNT else None


def observe_usage(response, provider: str | None) -> tuple[TokenUsage, bool]:
    generations = getattr(response, "generations", None)
    if not generations or len(generations) != 1 or len(generations[0]) != 1:
        return NO_USAGE, True
    message = getattr(generations[0][0], "message", None)
    usage = getattr(message, "usage_metadata", None)
    if not isinstance(usage, Mapping):
        return NO_USAGE, True
    input_tokens = _token_count(usage, "input_tokens")
    output_tokens = _token_count(usage, "output_tokens")
    total = _token_count(usage, "total_tokens")
    if (
        input_tokens is None
        and output_tokens is None
        or (
            total is not None
            and input_tokens is not None
            and output_tokens is not None
            and total != input_tokens + output_tokens
        )
    ):
        return NO_USAGE, True

    input_details = usage.get("input_token_details")
    output_details = usage.get("output_token_details")
    standard_tariff = True
    response_metadata = getattr(message, "response_metadata", None)
    if isinstance(response_metadata, Mapping) and response_metadata.get("service_tier") in (
        "priority",
        "flex",
    ):
        standard_tariff = False
    for details in (input_details, output_details):
        if isinstance(details, Mapping) and (
            "priority" in details or "flex" in details or _token_count(details, "audio")
        ):
            standard_tariff = False
    cache_read = cache_write = reasoning = None
    if input_tokens is not None and isinstance(input_details, Mapping):
        cache_read = _token_count(input_details, "cache_read")
        cache_write = _token_count(input_details, "cache_creation")
        # Anthropic's normalized aggregate becomes a zero placeholder when TTL
        # buckets are present. A single cache-write rate cannot price that split.
        if any(
            input_details.get(field)
            for field in ("ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")
        ):
            cache_write = None
            standard_tariff = False
        if (cache_read or 0) + (cache_write or 0) > input_tokens:
            cache_read = cache_write = None
    if output_tokens is not None and isinstance(output_details, Mapping):
        reasoning = _token_count(output_details, "reasoning")
        if reasoning is not None and reasoning > output_tokens:
            reasoning = None
    if provider == "google":
        # The pinned adapter turns absent cache/think counts into zero.
        cache_read = cache_read or None
        reasoning = reasoning or None
    return (
        TokenUsage(
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cache_read_tokens=cache_read,
            cache_write_tokens=cache_write,
            reasoning_tokens=reasoning,
            usage_source="langchain_usage_metadata",
        ),
        standard_tariff,
    )
