ARG EVALUATION_PYTHON_IMAGE=python:3.12-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df
ARG EVALUATION_UV_IMAGE=ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c
FROM ${EVALUATION_UV_IMAGE} AS uv

FROM ${EVALUATION_PYTHON_IMAGE} AS build
WORKDIR /opt/evaluation
ENV UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
COPY --from=uv /uv /usr/local/bin/uv
COPY pyproject.toml uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-dev --no-install-project --no-build-package nautilus-trader
COPY tradingbot_evaluation ./tradingbot_evaluation
# Build the application wheel from the copied source each time this layer runs.
RUN uv sync --locked --no-dev --no-editable --no-build-package nautilus-trader --no-cache

FROM ${EVALUATION_PYTHON_IMAGE}
WORKDIR /opt/evaluation
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY --from=build /opt/evaluation/.venv ./.venv
COPY --from=build /opt/evaluation/uv.lock ./uv.lock
USER 65532:65532
ENTRYPOINT ["/opt/evaluation/.venv/bin/tradingbot-evaluation"]
CMD ["probe-engine"]
