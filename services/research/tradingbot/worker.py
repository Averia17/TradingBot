"""Run one leased research attempt at a time through the Control API."""

import json
import logging
import re
from collections.abc import Callable
from concurrent.futures import Future, ThreadPoolExecutor, wait
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from functools import partial
from http.client import HTTPConnection, HTTPException, HTTPSConnection
from pathlib import Path
from threading import Event, Lock, Timer
from time import monotonic
from typing import TYPE_CHECKING
from urllib.parse import urlsplit
from uuid import UUID, uuid4

from tradingbot.llm_usage import LLMCall
from tradingbot.snapshots import ArchiveReference

if TYPE_CHECKING:
    from tradingbot.adapters.prepared_provider import ProviderConfig

_LOG = logging.getLogger(__name__)
_JOBS_PATH = "/v1/research/jobs/"
_MAX_PROVIDER_CONFIG_BYTES = 4096
_MAX_REQUEST_BYTES = 256 * 1024
_MAX_RESPONSE_BYTES = 512 * 1024
_RESULT_KEYS = {"result_schema_version", "batch", "reports", "metadata"}
_PERMIT_KEYS = {"job_id", "call_id", "attempt", "remaining_llm_calls"}
_USAGE_KEYS = {"job_id", "call_id", "attempt", "recorded_at"}
_LEASE_KEYS = {
    "job_id",
    "status",
    "worker_id",
    "claim_id",
    "lease_token",
    "attempt",
    "lease_expires_at",
    "attempt_deadline",
}
Analyzer = Callable[[dict, int, datetime, Event], dict]


class ControlError(RuntimeError):
    def __init__(self, *, status: int | None = None, retryable: bool = False):
        self.status = status
        self.retryable = retryable
        super().__init__(
            f"Control returned HTTP {status}" if status else "Control response unavailable"
        )


def _encode(value: dict) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def _object(pairs: list[tuple[str, object]]) -> dict:
    value = dict(pairs)
    if len(value) != len(pairs):
        raise ValueError("duplicate JSON field")
    return value


def load_provider_config(path: Path) -> dict:
    with path.open("rb") as source:
        raw = source.read(_MAX_PROVIDER_CONFIG_BYTES + 1)
    if len(raw) > _MAX_PROVIDER_CONFIG_BYTES:
        raise ValueError("provider configuration exceeds file limit")
    try:
        config = json.loads(raw, object_pairs_hook=_object)
    except (ValueError, RecursionError):
        raise ValueError("invalid provider configuration") from None
    if not isinstance(config, dict):
        raise ValueError("provider configuration must be an object")
    return config


def _timestamp(value: str) -> datetime:
    parsed = datetime.fromisoformat(value)
    if parsed.utcoffset() is None:
        raise ValueError("timestamp must be offset-aware")
    return parsed.astimezone(UTC)


def _remaining(deadline: datetime) -> float:
    return (deadline - datetime.now(UTC)).total_seconds()


class ControlClient:
    def __init__(self, origin: str, timeout: float):
        try:
            parsed = urlsplit(origin)
            port = parsed.port
            if (
                parsed.scheme not in {"http", "https"}
                or not parsed.hostname
                or parsed.path not in {"", "/"}
                or parsed.username is not None
                or parsed.password is not None
                or "?" in origin
                or "#" in origin
            ):
                raise ValueError
        except ValueError:
            raise ValueError(
                "Control URL must be an HTTP or HTTPS origin without credentials"
            ) from None
        self._connection = HTTPSConnection if parsed.scheme == "https" else HTTPConnection
        self._host = parsed.hostname
        self._port = port
        self._timeout = timeout

    def post(
        self, endpoint: str, body: bytes, expected: tuple[int, ...], timeout: float | None = None
    ) -> dict | None:
        if len(body) > _MAX_REQUEST_BYTES:
            raise ValueError("Control request exceeds body limit")
        return self._request("POST", _JOBS_PATH + endpoint, body, expected, timeout)

    def get(self, path: str) -> dict | None:
        return self._request("GET", path, None, (200,), None)

    def _request(
        self,
        method: str,
        path: str,
        body: bytes | None,
        expected: tuple[int, ...],
        timeout: float | None,
    ) -> dict | None:
        budget = self._timeout if timeout is None else min(self._timeout, timeout)
        if budget <= 0:
            raise ControlError()
        # The direct HTTP connection does not consult proxies or follow redirects.
        connection = self._connection(self._host, self._port, timeout=budget)
        try:
            headers = {"Content-Type": "application/json"} if body is not None else {}
            connection.request(method, path, body, headers)
            with connection.getresponse() as response:
                status = response.status
                if status not in expected:
                    raise ControlError(
                        status=status,
                        retryable=status in (408, 429) or status >= 500 or 200 <= status < 300,
                    )
                if status == 204:
                    return None
                payload = response.read(_MAX_RESPONSE_BYTES + 1)
            if len(payload) > _MAX_RESPONSE_BYTES:
                raise ControlError(retryable=True)
            decoded = json.loads(payload, object_pairs_hook=_object)
            if not isinstance(decoded, dict):
                raise ControlError(retryable=True)
            return decoded
        except (OSError, HTTPException, ValueError, RecursionError):
            raise ControlError(retryable=True) from None
        finally:
            connection.close()


@dataclass(frozen=True)
class _Lease:
    job_id: str
    claim_id: str
    token: str
    attempt: int
    expires: datetime
    deadline: datetime

    @classmethod
    def from_response(cls, response: dict, worker_id: str, claim_id: str) -> "_Lease":
        try:
            job_id, token = response["job_id"], response["lease_token"]
            attempt = response["attempt"]
            expires, deadline = map(
                _timestamp, (response["lease_expires_at"], response["attempt_deadline"])
            )
            if (
                response["status"] != "leased"
                or response["worker_id"] != worker_id
                or response["claim_id"] != claim_id
                or str(UUID(job_id)) != job_id
                or str(UUID(token)) != token
                or type(attempt) is not int
                or not 1 <= attempt <= 3
                or expires > deadline
            ):
                raise ValueError
        except (AttributeError, ValueError, KeyError, TypeError, OverflowError):
            raise ControlError(retryable=True) from None
        return cls(job_id, claim_id, token, attempt, expires, deadline)


class _LeaseWatchdog:
    """Cancel the analyzer at the last confirmed expiry even during an HTTP request."""

    def __init__(self, expires: datetime):
        self.cancelled = Event()
        self._lock = Lock()
        self._expires = expires
        self._timer = self._start_timer(expires)

    def _start_timer(self, expires: datetime) -> Timer:
        timer = Timer(max(0, _remaining(expires)), self._expire, args=(expires,))
        timer.daemon = True
        timer.start()
        return timer

    def _expire(self, expires: datetime) -> None:
        with self._lock:
            if self._expires == expires:
                self.cancelled.set()

    def extend(self, expires: datetime) -> bool:
        with self._lock:
            if self.cancelled.is_set() or _remaining(self._expires) <= 0:
                self.cancelled.set()
                return False
            if expires > self._expires:
                self._timer.cancel()
                self._expires = expires
                self._timer = self._start_timer(expires)
            return True

    def remaining(self) -> float:
        with self._lock:
            remaining = _remaining(self._expires)
            if remaining <= 0:
                self.cancelled.set()
            return 0 if self.cancelled.is_set() else remaining

    def close(self) -> None:
        self._timer.cancel()


@dataclass(frozen=True)
class _Worker:
    control: ControlClient
    worker_id: str
    stop: Event
    analyze: Analyzer | None
    poll_interval: float
    heartbeat_interval: float
    archive_root: Path | None
    identity_registry: ArchiveReference | None = None
    provider_config: "ProviderConfig | None" = None

    def run(self) -> None:
        if self.analyze is None and not self.wait_for_call_usage():
            return
        claim_id = str(uuid4())
        while not self.stop.is_set():
            try:
                claimed = self.claim(claim_id)
            except ControlError as exc:
                if exc.status == 409:
                    claim_id = str(uuid4())
                elif not exc.retryable:
                    raise
                else:
                    _LOG.warning("Research claim response unavailable: %s", exc)
                claimed = None
            if claimed is None:
                self.stop.wait(self.poll_interval)
                continue
            if not self.run_attempt(*claimed):
                return
            claim_id = str(uuid4())

    def claim(self, claim_id: str) -> tuple[_Lease, dict] | None:
        identity = {"worker_id": self.worker_id, "claim_id": claim_id}
        if self.identity_registry is not None:
            identity["identity_registry_sha256"] = self.identity_registry.sha256
        if self.provider_config is not None:
            identity["analysis_mode"] = "prepared_provider"
        claim = self.control.post("claim", _encode(identity), (200, 204))
        if claim is None:
            return None
        lease = _Lease.from_response(claim, self.worker_id, claim_id)
        request = claim.get("request")
        if (
            claim.keys() != (_LEASE_KEYS | {"request"})
            or not isinstance(request, dict)
            or request.get("job_id") != lease.job_id
            or not self.supports_request(request)
        ):
            raise ControlError(retryable=True)
        return lease, request

    def run_attempt(self, lease: _Lease, request: dict) -> bool:
        try:
            self.process(lease, request)
        except Exception as exc:
            # Incomplete attempts recover after expiry. Log only the error type.
            _LOG.warning(
                "Research attempt incomplete job=%s attempt=%d error=%s",
                lease.job_id,
                lease.attempt,
                type(exc).__name__,
            )
            if self.analyze is None and isinstance(exc, ControlError) and exc.status == 404:
                return self.wait_for_call_usage()
        return True

    def supports_request(self, request: dict) -> bool:
        version = request.get("job_schema_version")
        if self.provider_config is not None:
            return (
                version == "0.3"
                and request.get("analysis_mode") == "prepared_provider"
                and self.identity_registry is not None
                and request.get("identity_registry") == self.identity_registry.model_dump(mode="json")
            )
        if "analysis_mode" in request:
            return False
        if version == "0.1":
            return self.identity_registry is None
        return (
            version == "0.2"
            and self.identity_registry is not None
            and request.get("identity_registry") == self.identity_registry.model_dump(mode="json")
        )

    def wait_for_call_usage(self) -> bool:
        while not self.stop.is_set():
            try:
                capabilities = self.control.get("/v1/capabilities")
            except ControlError as exc:
                if not exc.retryable:
                    raise
                _LOG.warning("Research capabilities response unavailable: %s", exc)
                self.stop.wait(self.poll_interval)
                continue
            if capabilities is not None and capabilities.get("research_call_usage") is True:
                if self.identity_registry is not None:
                    jobs = capabilities.get("research_job_schema_versions")
                    results = capabilities.get("research_result_schema_versions")
                    job_version, result_version = (
                        ("0.2", "0.3") if self.provider_config is None else ("0.3", "0.4")
                    )
                    if not (
                        isinstance(jobs, list) and job_version in jobs
                        and isinstance(results, list) and result_version in results
                    ):
                        if self.provider_config is not None:
                            raise RuntimeError("Control does not advertise prepared-provider research")
                        raise RuntimeError("Control does not advertise identity-bound research")
                return True
            message = "Control does not advertise durable research call usage"
            _LOG.error(message)
            raise RuntimeError(message)
        return False

    def renew(self, lease: _Lease) -> _Lease | None:
        identity = _encode({"worker_id": self.worker_id, "lease_token": lease.token})
        while not self.stop.is_set() and (remaining := _remaining(lease.expires)) > 0:
            try:
                response = self.control.post(
                    lease.job_id + "/heartbeat", identity, (200,), remaining
                )
                if response is None or response.keys() != _LEASE_KEYS:
                    raise ControlError(retryable=True)
                updated = _Lease.from_response(response, self.worker_id, lease.claim_id)
                if (
                    replace(updated, expires=lease.expires) != lease
                    or updated.expires < lease.expires
                ):
                    raise ControlError(retryable=True)
                return updated if _remaining(lease.expires) > 0 else None
            except ControlError as exc:
                if exc.status in (404, 409):
                    return None
                if not exc.retryable:
                    raise
                self.stop.wait(max(0, min(self.poll_interval, _remaining(lease.expires))))
        return None

    def process(self, lease: _Lease, request: dict) -> None:
        active = self.renew(lease)
        if active is None or self.stop.is_set():
            return
        watchdog = _LeaseWatchdog(active.expires)
        try:
            analyze = self.attempt_analyzer(active, watchdog)
            with ThreadPoolExecutor(max_workers=1, thread_name_prefix="research") as executor:
                pending = executor.submit(
                    analyze, request, active.attempt, active.deadline, watchdog.cancelled
                )
                try:
                    completed = self.await_result(pending, active, watchdog)
                finally:
                    if not pending.done():
                        watchdog.cancelled.set()
        finally:
            watchdog.close()
        if completed is None:
            return
        active, result = completed
        if self.stop.is_set() or watchdog.cancelled.is_set() or _remaining(active.expires) <= 0:
            return
        self.publish(
            active,
            result,
            expected_version={"0.2": "0.3", "0.3": "0.4"}.get(request.get("job_schema_version"), "0.2"),
        )

    def attempt_analyzer(self, lease: _Lease, watchdog: _LeaseWatchdog) -> Analyzer:
        analyze = self.analyze
        if analyze is None:
            if self.provider_config is None:
                from tradingbot.adapters.offline import analyze_offline

                analyze = analyze_offline
            else:
                from tradingbot.adapters.prepared_provider import analyze_prepared_provider

                analyze = partial(analyze_prepared_provider, provider_config=self.provider_config)
            analyze = partial(
                analyze,
                authorize_call=partial(self.authorize_call, lease, watchdog),
                record_call=partial(self.record_call, lease, watchdog),
            )
        if self.archive_root is not None:
            analyze = partial(analyze, archive_root=self.archive_root)
        if self.identity_registry is not None:
            analyze = partial(analyze, owner_identity_registry=self.identity_registry)
        return analyze

    def await_result(
        self, pending: Future[dict], lease: _Lease, watchdog: _LeaseWatchdog
    ) -> tuple[_Lease, dict] | None:
        next_heartbeat = self.heartbeat_at(lease)
        while not self.stop.is_set() and not watchdog.cancelled.is_set():
            if pending.done():
                return lease, pending.result()
            if monotonic() >= next_heartbeat:
                updated = self.renew(lease)
                if updated is None or not watchdog.extend(updated.expires):
                    return None
                lease = updated
                next_heartbeat = self.heartbeat_at(lease)
            wait(
                (pending,),
                timeout=max(0, min(0.1, next_heartbeat - monotonic(), _remaining(lease.expires))),
            )
        return None

    def heartbeat_at(self, lease: _Lease) -> float:
        remaining = _remaining(lease.expires)
        return monotonic() + (
            remaining
            if lease.expires == lease.deadline
            else min(self.heartbeat_interval, remaining / 2)
        )

    def authorize_call(self, lease: _Lease, watchdog: _LeaseWatchdog, run_id: UUID) -> None:
        self._post_call(
            lease, watchdog, "/authorize-call", run_id, {"call_id": str(run_id)}, _valid_permit
        )

    def record_call(self, lease: _Lease, watchdog: _LeaseWatchdog, call: LLMCall) -> None:
        self._post_call(
            lease,
            watchdog,
            "/record-call",
            call.run_id,
            {
                "call_usage_schema_version": "0.2" if self.provider_config is not None else "0.1",
                "call": call.model_dump(mode="json"),
            },
            _valid_usage_receipt,
        )

    def _post_call(
        self,
        lease: _Lease,
        watchdog: _LeaseWatchdog,
        suffix: str,
        run_id: UUID,
        fields: dict,
        validate: Callable[[dict], bool],
    ) -> None:
        body = _encode(fields | {"worker_id": self.worker_id, "lease_token": lease.token})
        for transmission in range(3):
            if self.stop.is_set() or (remaining := watchdog.remaining()) <= 0:
                raise ControlError()
            try:
                response = self.control.post(lease.job_id + suffix, body, (200, 201), remaining)
                if (
                    response is None
                    or response.get("job_id") != lease.job_id
                    or response.get("call_id") != str(run_id)
                    or type(response.get("attempt")) is not int
                    or response.get("attempt") != lease.attempt
                    or not validate(response)
                ):
                    raise ControlError(retryable=True)
            except ControlError as exc:
                if exc.status in (404, 409):
                    watchdog.cancelled.set()
                if not exc.retryable or transmission == 2:
                    raise
                watchdog.cancelled.wait(min(self.poll_interval, watchdog.remaining()))
            else:
                if self.stop.is_set() or watchdog.remaining() <= 0:
                    raise ControlError()
                return

    def publish(self, lease: _Lease, result: dict, *, expected_version: str = "0.2") -> None:
        if (
            not isinstance(result, dict)
            or result.keys() != _RESULT_KEYS
            or result["result_schema_version"] != expected_version
        ):
            raise ValueError("invalid research result envelope")
        expected = _encode(result)
        body = _encode(result | {"worker_id": self.worker_id, "lease_token": lease.token})
        remaining = _remaining(lease.expires)
        if remaining <= 0:
            return
        for transmission in range(3):
            if self.stop.is_set():
                return
            try:
                # Control can replay a committed response after the original lease expires.
                response = self.control.post(
                    lease.job_id + "/complete",
                    body,
                    (200, 201),
                    remaining if transmission == 0 else None,
                )
                if (
                    response is None
                    or response.keys() != {"job_id", "status", "attempt", "completed_at", "result"}
                    or response["job_id"] != lease.job_id
                    or response["status"] != "completed"
                    or type(response["attempt"]) is not int
                    or response["attempt"] != lease.attempt
                    or not _valid_receipt(response, expected)
                ):
                    raise ControlError(retryable=True)
            except ControlError as exc:
                if not exc.retryable or transmission == 2:
                    raise
                self.stop.wait(self.poll_interval)
            else:
                _LOG.info("Research result saved job=%s attempt=%d", lease.job_id, lease.attempt)
                return


def _valid_receipt(response: dict, expected: bytes) -> bool:
    try:
        _timestamp(response["completed_at"])
        return _encode(response["result"]) == expected
    except (TypeError, ValueError, OverflowError, RecursionError):
        return False


def _valid_permit(response: dict) -> bool:
    return (
        response.keys() == _PERMIT_KEYS
        and type(response["remaining_llm_calls"]) is int
        and 0 <= response["remaining_llm_calls"] < 64
    )


def _valid_usage_receipt(response: dict) -> bool:
    if response.keys() != _USAGE_KEYS or not isinstance(response["recorded_at"], str):
        return False
    try:
        _timestamp(response["recorded_at"])
    except (ValueError, OverflowError):
        return False
    return True


def run_worker(
    control_url: str,
    worker_id: str | None = None,
    *,
    stop: Event | None = None,
    analyze: Analyzer | None = None,
    poll_interval: float = 1,
    heartbeat_interval: float = 10,
    request_timeout: float = 5,
    archive_root: Path | None = None,
    identity_registry: ArchiveReference | dict | None = None,
    provider_config: dict | None = None,
) -> None:
    """Poll Control with offline models by default, or explicitly use prepared-provider jobs."""
    provider = None
    profile = "offline"
    if provider_config is not None:
        if archive_root is None or identity_registry is None:
            raise ValueError("prepared-provider research requires an archive root and identity registry")
        if analyze is not None:
            raise ValueError("prepared-provider research uses the built-in analyzer")
        from tradingbot.adapters.prepared_provider import ProviderConfig

        provider = ProviderConfig.model_validate(provider_config)
        profile = "provider"
    worker_id = worker_id if worker_id is not None else f"research-{profile}-{uuid4()}"
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", worker_id):
        raise ValueError("invalid worker identity")
    if not all(
        0 < interval < float("inf")
        for interval in (poll_interval, heartbeat_interval, request_timeout)
    ):
        raise ValueError("worker intervals and request timeout must be finite and positive")
    if archive_root is not None:
        archive_root = archive_root.resolve(strict=True)
        if not archive_root.is_dir():
            raise ValueError("archive root must be a directory")
    if identity_registry is not None:
        if archive_root is None:
            raise ValueError("an identity registry requires an archive root")
        from tradingbot.security_identity import load_identity_registry

        identity_registry = ArchiveReference.model_validate(identity_registry)
        load_identity_registry(archive_root, identity_registry)
    control = ControlClient(control_url, request_timeout)
    _Worker(
        control,
        worker_id,
        stop if stop is not None else Event(),
        analyze,
        poll_interval,
        heartbeat_interval,
        archive_root,
        identity_registry,
        provider,
    ).run()
