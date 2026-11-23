import json
from datetime import UTC, datetime, timedelta
from threading import Event
from uuid import UUID

import pytest

from tradingbot import worker
from tradingbot.adapters import offline
from tradingbot.llm_usage import NO_USAGE, LLMCall

JOB_ID = "00000000-0000-4000-8000-000000000001"
TOKEN = "00000000-0000-4000-8000-000000000002"
WORKER_ID = "offline-worker-test"
REQUEST = {"job_id": JOB_ID, "job_schema_version": "0.1"}
RESULT = {"result_schema_version": "0.2", "batch": {}, "reports": {}, "metadata": {}}
CALL_ID = UUID("00000000-0000-4000-8000-000000000003")


def lease(claim_id, deadline, *, request=False, expires=None):
    now = datetime.now(UTC)
    value = {
        "job_id": JOB_ID,
        "status": "leased",
        "worker_id": WORKER_ID,
        "claim_id": claim_id,
        "lease_token": TOKEN,
        "attempt": 1,
        "lease_expires_at": (expires or now + timedelta(seconds=10)).isoformat(),
        "attempt_deadline": deadline.isoformat(),
    }
    if request:
        value["request"] = REQUEST
    return value


@pytest.mark.parametrize("denial", [None, 409, 422, "late_ack"])
def test_worker_requires_valid_replayed_permit_before_invoking_model(monkeypatch, denial):
    stop = Event()
    deadline = datetime.now(UTC) + timedelta(seconds=30)
    claim_id = None
    permits, model_calls, recordings = [], [], []
    cancellation = None

    def post(self, endpoint, body, expected, timeout=None):
        nonlocal claim_id, deadline
        if endpoint == "claim":
            if claim_id is not None:
                stop.set()
                return None
            claim_id = json.loads(body)["claim_id"]
            if denial == "late_ack":
                deadline = datetime.now(UTC) + timedelta(seconds=0.5)
            return lease(
                claim_id, deadline, request=True, expires=deadline if denial == "late_ack" else None
            )
        if endpoint.endswith("/heartbeat"):
            return lease(claim_id, deadline, expires=deadline if denial == "late_ack" else None)
        if endpoint.endswith("/authorize-call"):
            assert expected == (200, 201) and timeout > 0
            permits.append(body)
            if isinstance(denial, int):
                raise worker.ControlError(status=denial)
            receipt = {
                "job_id": JOB_ID,
                "call_id": str(CALL_ID),
                "attempt": 1,
                "remaining_llm_calls": 0,
            }
            if denial == "late_ack":
                assert cancellation.wait(2), "the confirmed lease should expire before delivery"
                return receipt
            if len(permits) == 1:
                raise worker.ControlError(retryable=True)
            return receipt if len(permits) > 2 else receipt | {"attempt": 2}
        if endpoint.endswith("/record-call"):
            assert expected == (200, 201) and timeout > 0
            assert model_calls == [CALL_ID]
            recordings.append(body)
            if len(recordings) == 1:
                raise worker.ControlError(retryable=True)
            return {
                "job_id": JOB_ID,
                "call_id": str(CALL_ID),
                "attempt": 1,
                "recorded_at": datetime.now(UTC).isoformat(),
            }
        assert endpoint.endswith("/complete") and not denial
        assert len(recordings) == 2
        stop.set()
        return {
            "job_id": JOB_ID,
            "status": "completed",
            "attempt": 1,
            "completed_at": datetime.now(UTC).isoformat(),
            "result": RESULT,
        }

    def analyze(request, attempt, deadline, cancelled, *, authorize_call, record_call):
        nonlocal cancellation
        assert authorize_call is not None
        cancellation = cancelled
        authorize_call(CALL_ID)
        model_calls.append(CALL_ID)
        record_call(
            LLMCall(
                run_id=CALL_ID,
                provider="offline",
                model=offline.MODEL_ID,
                role="portfolio_allocator",
                status="completed",
                started_at=datetime.now(UTC),
                **NO_USAGE.model_dump(),
            )
        )
        return RESULT

    monkeypatch.setattr(worker.ControlClient, "post", post)
    monkeypatch.setattr(
        worker.ControlClient,
        "get",
        lambda self, path: {"research_call_usage": True},
        raising=False,
    )
    monkeypatch.setattr(offline, "analyze_offline", analyze)
    worker.run_worker("http://127.0.0.1:8080", WORKER_ID, stop=stop, poll_interval=0.001)
    assert model_calls == ([] if denial else [CALL_ID])
    assert len(permits) == (1 if denial else 3)
    assert all(permit == permits[0] for permit in permits)
    assert len(recordings) == (0 if denial else 2)
    if recordings:
        assert recordings[0] == recordings[1]
        recorded = json.loads(recordings[0])
        assert recorded["call_usage_schema_version"] == "0.1"
        assert recorded["worker_id"] == WORKER_ID
        assert recorded["lease_token"] == TOKEN
        assert recorded["call"]["run_id"] == str(CALL_ID)
        assert recorded["call"]["status"] == "completed"
        assert recorded["call"]["input_tokens"] is None
    assert json.loads(permits[0]) == {
        "worker_id": WORKER_ID,
        "lease_token": TOKEN,
        "call_id": str(CALL_ID),
    }


@pytest.mark.parametrize(
    "capabilities", [{}, {"research_call_usage": False}, {"research_call_usage": "true"}]
)
def test_builtin_worker_rejects_incompatible_control_before_claim(monkeypatch, capabilities):
    requests = []

    def get(self, path):
        requests.append(path)
        return capabilities

    def unexpected(*args):
        pytest.fail("incompatible Control must not receive a claim")

    monkeypatch.setattr(worker.ControlClient, "get", get, raising=False)
    monkeypatch.setattr(worker.ControlClient, "post", unexpected)
    with pytest.raises(RuntimeError, match="durable research call usage"):
        worker.run_worker("http://127.0.0.1:8080", WORKER_ID)
    assert requests == ["/v1/capabilities"]


def test_builtin_worker_waits_for_capability_response_before_claim(monkeypatch):
    stop = Event()
    endpoints = []

    def get(self, path):
        endpoints.append(path)
        if len(endpoints) == 1:
            raise worker.ControlError(status=503, retryable=True)
        return {"research_call_usage": True}

    def post(self, endpoint, *args):
        endpoints.append(endpoint)
        stop.set()
        return None

    monkeypatch.setattr(worker.ControlClient, "get", get)
    monkeypatch.setattr(worker.ControlClient, "post", post)
    worker.run_worker("http://127.0.0.1:8080", WORKER_ID, stop=stop, poll_interval=0.001)
    assert endpoints == ["/v1/capabilities", "/v1/capabilities", "claim"]


def test_replays_identical_claim_and_completion_after_uncertain_responses(monkeypatch):
    stop = Event()
    claims, completions, analyses = [], [], []
    deadline = datetime.now(UTC) + timedelta(seconds=30)

    def post(self, endpoint, body, expected, timeout=None):
        payload = json.loads(body)
        if endpoint == "claim":
            claims.append(body)
            assert len(claims) <= 2, "completion recovery started another claim"
            if len(claims) == 1:
                raise worker.ControlError(retryable=True)
            return lease(payload["claim_id"], deadline, request=True)
        if endpoint.endswith("/heartbeat"):
            return lease(json.loads(claims[-1])["claim_id"], deadline)
        completions.append(body)
        if len(completions) == 1:
            raise worker.ControlError(retryable=True)
        if len(completions) == 2:
            result = {}
        else:
            result = RESULT
            stop.set()
        return {
            "job_id": JOB_ID,
            "status": "completed",
            "attempt": 1,
            "completed_at": datetime.now(UTC).isoformat(),
            "result": result,
        }

    def analyze(request, attempt, deadline, cancelled):
        analyses.append((request, attempt, cancelled.is_set()))
        return RESULT

    monkeypatch.setattr(worker.ControlClient, "post", post)
    worker.run_worker(
        "http://127.0.0.1:8080", WORKER_ID, stop=stop, analyze=analyze, poll_interval=0.001
    )

    assert len(claims) == 2 and claims[0] == claims[1]
    assert UUID(json.loads(claims[0])["claim_id"]).version == 4
    assert analyses == [(REQUEST, 1, False)]
    assert completions == [completions[0]] * 3
    assert json.loads(completions[0]) == RESULT | {"worker_id": WORKER_ID, "lease_token": TOKEN}


def test_cancels_running_analyzer_and_discards_result_after_lease_conflict(monkeypatch):
    stop = Event()
    heartbeat_count = 0
    claim_id = None
    cancelled_analysis = Event()
    deadline = datetime.now(UTC) + timedelta(seconds=30)
    endpoints = []

    def post(self, endpoint, body, expected, timeout=None):
        nonlocal heartbeat_count, claim_id
        endpoints.append(endpoint)
        if endpoint == "claim":
            assert claim_id is None, "the first analyzer did not stop the worker"
            claim_id = json.loads(body)["claim_id"]
            return lease(claim_id, deadline, request=True)
        assert endpoint.endswith("/heartbeat"), "completion must not be sent for a lost lease"
        heartbeat_count += 1
        if heartbeat_count == 1:
            return lease(claim_id, deadline)
        raise worker.ControlError(status=409)

    def analyze(request, attempt, deadline, cancelled):
        assert cancelled.wait(1)
        cancelled_analysis.set()
        stop.set()
        return RESULT

    monkeypatch.setattr(worker.ControlClient, "post", post)
    worker.run_worker(
        "http://127.0.0.1:8080", WORKER_ID, stop=stop, analyze=analyze, heartbeat_interval=0.01
    )

    assert heartbeat_count == 2
    assert cancelled_analysis.is_set()
    assert endpoints == ["claim", JOB_ID + "/heartbeat", JOB_ID + "/heartbeat"]


def test_failed_analyzer_leaves_job_for_lease_recovery_without_publishing(monkeypatch, caplog):
    stop = Event()
    claim_id = None
    deadline = datetime.now(UTC) + timedelta(seconds=30)
    endpoints = []

    def post(self, endpoint, body, expected, timeout=None):
        nonlocal claim_id
        endpoints.append(endpoint)
        if endpoint == "claim":
            if claim_id is not None:
                stop.set()
                return None
            claim_id = json.loads(body)["claim_id"]
            return lease(claim_id, deadline, request=True)
        assert endpoint.endswith("/heartbeat"), "failed analysis must not publish a result"
        return lease(claim_id, deadline)

    def analyze(request, attempt, deadline, cancelled):
        raise ValueError("example-sensitive-provider-response")

    monkeypatch.setattr(worker.ControlClient, "post", post)
    worker.run_worker("http://127.0.0.1:8080", WORKER_ID, stop=stop, analyze=analyze)
    assert endpoints == ["claim", JOB_ID + "/heartbeat", "claim"]
    assert "attempt incomplete" in caplog.text
    assert "ValueError" in caplog.text
    assert "example-sensitive-provider-response" not in caplog.text


def test_lease_deadline_cancels_analyzer_while_renewal_is_stalled(monkeypatch):
    stop = Event()
    analyzer_cancelled = Event()
    claim_id = None
    expiry = None
    heartbeat_count = 0
    seen_during_renewal = False
    deadline = datetime.now(UTC) + timedelta(seconds=30)

    def post(self, endpoint, body, expected, timeout=None):
        nonlocal claim_id, expiry, heartbeat_count, seen_during_renewal
        if endpoint == "claim":
            assert claim_id is None
            claim_id = json.loads(body)["claim_id"]
            expiry = datetime.now(UTC) + timedelta(seconds=0.3)
            return lease(claim_id, deadline, request=True, expires=expiry)
        assert endpoint.endswith("/heartbeat")
        heartbeat_count += 1
        if heartbeat_count == 1:
            return lease(claim_id, deadline, expires=expiry)
        seen_during_renewal = analyzer_cancelled.wait(1)
        stop.set()
        # A late acknowledgement cannot restart a cancelled attempt.
        return lease(claim_id, deadline)

    def analyze(request, attempt, deadline, cancelled):
        if cancelled.wait(2):
            analyzer_cancelled.set()
        return RESULT

    monkeypatch.setattr(worker.ControlClient, "post", post)
    worker.run_worker(
        "http://127.0.0.1:8080", WORKER_ID, stop=stop, analyze=analyze, heartbeat_interval=0.01
    )
    assert heartbeat_count == 2
    assert seen_during_renewal


def test_claim_redirect_is_not_followed_or_retried(monkeypatch):
    requests = []

    class RedirectResponse:
        status = 302

        def __enter__(self):
            return self

        def __exit__(self, *_exc):
            pass

    class Connection:
        def __init__(self, host, port, *, timeout):
            assert (host, port, timeout) == ("127.0.0.1", 8080, 5)

        def request(self, method, path, body, headers):
            requests.append((method, path))

        def getresponse(self):
            return RedirectResponse()

        def close(self):
            pass

    monkeypatch.setattr(worker, "HTTPConnection", Connection)
    with pytest.raises(worker.ControlError, match="302"):
        worker.run_worker("http://127.0.0.1:8080", WORKER_ID, analyze=lambda *a: RESULT)
    assert requests == [("POST", "/v1/research/jobs/claim")]


@pytest.mark.parametrize(
    "url",
    [
        "http://user:password@localhost",
        "http://localhost/#secret",
        "ftp://localhost",
        "http://localhost/jobs",
    ],
)
def test_control_url_rejects_credentials_and_non_origins(url):
    with pytest.raises(ValueError, match="Trading API URL"):
        worker.run_worker(url, WORKER_ID, analyze=lambda *a: RESULT)
