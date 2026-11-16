"""Run a single offline research attempt at a time through the Control API."""

import json
import logging
import re
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor, wait
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from functools import partial
from http.client import HTTPConnection, HTTPException, HTTPSConnection
from pathlib import Path
from threading import Event, Lock, Timer
from time import monotonic
from urllib.parse import urlsplit
from uuid import UUID, uuid4

_LOG = logging.getLogger(__name__)
_JOBS_PATH = "/v1/research/jobs/"
_MAX_REQUEST_BYTES = 256 * 1024
_MAX_RESPONSE_BYTES = 512 * 1024
_RESULT_KEYS = {"result_schema_version", "batch", "reports", "metadata"}
_PERMIT_KEYS = {"job_id", "call_id", "attempt", "remaining_llm_calls"}
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
        budget = self._timeout if timeout is None else min(self._timeout, timeout)
        if budget <= 0:
            raise ControlError()
        # The direct HTTP connection does not consult proxies or follow redirects.
        connection = self._connection(self._host, self._port, timeout=budget)
        try:
            connection.request(
                "POST", _JOBS_PATH + endpoint, body, {"Content-Type": "application/json"}
            )
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

    def run(self) -> None:
        claim_id = str(uuid4())
        while not self.stop.is_set():
            try:
                claim = self.control.post(
                    "claim",
                    _encode({"worker_id": self.worker_id, "claim_id": claim_id}),
                    (200, 204),
                )
                if claim is not None:
                    lease = _Lease.from_response(claim, self.worker_id, claim_id)
                    request = claim.get("request")
                    if (
                        claim.keys() != (_LEASE_KEYS | {"request"})
                        or not isinstance(request, dict)
                        or request.get("job_id") != lease.job_id
                        or request.get("job_schema_version") != "0.1"
                    ):
                        raise ControlError(retryable=True)
            except ControlError as exc:
                if exc.status == 409:
                    claim_id = str(uuid4())
                elif not exc.retryable:
                    raise
                else:
                    _LOG.warning("Research claim response unavailable: %s", exc)
            else:
                if claim is not None:
                    try:
                        self.process(lease, request)
                    except Exception as exc:
                        # The queue can recover this attempt after expiry. Record
                        # only error types so provider data never reaches logs.
                        _LOG.warning(
                            "Research attempt incomplete job=%s attempt=%d error=%s",
                            lease.job_id,
                            lease.attempt,
                            type(exc).__name__,
                        )
                    claim_id = str(uuid4())
                    continue
            self.stop.wait(self.poll_interval)

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
        analyze = self.analyze
        if analyze is None:
            from tradingbot.adapters.offline import analyze_offline

            analyze = partial(
                analyze_offline, authorize_call=partial(self.authorize_call, active, watchdog)
            )
        if self.archive_root is not None:
            analyze = partial(analyze, archive_root=self.archive_root)
        with ThreadPoolExecutor(max_workers=1, thread_name_prefix="research") as executor:
            pending = executor.submit(
                analyze, request, active.attempt, active.deadline, watchdog.cancelled
            )
            next_heartbeat = self.heartbeat_at(active)
            try:
                while not self.stop.is_set() and not watchdog.cancelled.is_set():
                    if pending.done():
                        result = pending.result()
                        break
                    if monotonic() >= next_heartbeat:
                        updated = self.renew(active)
                        if updated is None or not watchdog.extend(updated.expires):
                            return
                        active = updated
                        next_heartbeat = self.heartbeat_at(active)
                    wait(
                        (pending,),
                        timeout=max(
                            0, min(0.1, next_heartbeat - monotonic(), _remaining(active.expires))
                        ),
                    )
                else:
                    return
            finally:
                if not pending.done():
                    watchdog.cancelled.set()
                watchdog.close()
        if (
            not self.stop.is_set()
            and not watchdog.cancelled.is_set()
            and _remaining(active.expires) > 0
        ):
            self.publish(active, result)

    def heartbeat_at(self, lease: _Lease) -> float:
        remaining = _remaining(lease.expires)
        return monotonic() + (
            remaining
            if lease.expires == lease.deadline
            else min(self.heartbeat_interval, remaining / 2)
        )

    def authorize_call(self, lease: _Lease, watchdog: _LeaseWatchdog, run_id: UUID) -> None:
        call_id = str(run_id)
        body = _encode({"worker_id": self.worker_id, "lease_token": lease.token, "call_id": call_id})
        for transmission in range(3):
            if self.stop.is_set() or (remaining := watchdog.remaining()) <= 0:
                raise ControlError()
            try:
                response = self.control.post(
                    lease.job_id + "/authorize-call", body, (200, 201), remaining
                )
                if (
                    response is None
                    or response.keys() != _PERMIT_KEYS
                    or response["job_id"] != lease.job_id
                    or response["call_id"] != call_id
                    or type(response["attempt"]) is not int
                    or response["attempt"] != lease.attempt
                    or type(response["remaining_llm_calls"]) is not int
                    or not 0 <= response["remaining_llm_calls"] < 64
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

    def publish(self, lease: _Lease, result: dict) -> None:
        if (
            not isinstance(result, dict)
            or result.keys() != _RESULT_KEYS
            or result["result_schema_version"] != "0.2"
        ):
            raise ValueError("invalid offline research result envelope")
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
) -> None:
    """Poll Control and publish offline results until the stop event is set."""
    worker_id = worker_id if worker_id is not None else f"research-offline-{uuid4()}"
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
    control = ControlClient(control_url, request_timeout)
    _Worker(
        control,
        worker_id,
        stop if stop is not None else Event(),
        analyze,
        poll_interval,
        heartbeat_interval,
        archive_root,
    ).run()
