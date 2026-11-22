ARG PYTHON_IMAGE=python:3.12.13-bookworm@sha256:3cd9086bdb30f7c9bc08a3fa621d9842e0d3f6f9291aeb4677e0547817c10b12
ARG UV_IMAGE=ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c
FROM ${UV_IMAGE} AS uv
FROM ${PYTHON_IMAGE} AS build
WORKDIR /opt/tradingbot
ENV UV_PYTHON_DOWNLOADS=never UV_LINK_MODE=copy GIT_TERMINAL_PROMPT=0
COPY --from=uv /uv /usr/local/bin/uv
COPY pyproject.toml uv.lock upstream.lock.json ./
COPY tradingbot ./tradingbot
RUN python -c 'from pathlib import Path; from tradingbot.upstream import bootstrap; bootstrap(Path.cwd(), distribution="tradingagents")'
RUN uv sync --locked --no-dev
RUN <<'SH'
set -eu
.venv/bin/python - <<'PY' > release-manifest.json
import hashlib
import json
from importlib.metadata import version
from pathlib import Path

from tradingbot.adapters.offline import _verified_upstream
from tradingbot.adapters.tradingagents import check_compatibility

upstream_version, upstream_sha = _verified_upstream()
print(json.dumps({
    "component": "research",
    "worker_version": version("self-custody-ai-trader"),
    "upstream_version": upstream_version,
    "upstream_sha": upstream_sha,
    "lock_sha256": hashlib.sha256(Path("uv.lock").read_bytes()).hexdigest(),
    "adapter_contract": check_compatibility()["adapter_contract"],
    "research_job_schema_version": "0.1",
    "research_job_schema_versions": ["0.1", "0.2", "0.3"],
    "research_result_schema_version": "0.2",
    "research_result_schema_versions": ["0.2", "0.3", "0.4"],
    "research_job_call_budget": True,
}, sort_keys=True))
PY
SH

FROM ${PYTHON_IMAGE} AS research-runtime
WORKDIR /opt/tradingbot
ENV PATH=/opt/tradingbot/.venv/bin:$PATH HOME=/tmp PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0
COPY --from=build /opt/tradingbot /opt/tradingbot
RUN git config --system --add safe.directory /opt/tradingbot/vendor/trading-agents \
    && install -d -o 65532 -g 65532 -m 0700 /opt/provider-home/.tradingagents
USER 65532:65532
ENTRYPOINT ["tradingbot"]
CMD ["worker"]

FROM research-runtime AS paper-entry
USER root
RUN install -d -o 65532 -g 65532 -m 0700 /journal
COPY --from=control-image /usr/local/bin/paper /usr/local/bin/paper
USER 65532:65532
ENTRYPOINT ["python", "-m", "tradingbot.entry_cycles"]
CMD ["--help"]

FROM research-runtime AS research
