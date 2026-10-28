# themis-intelligence: the investigator. It holds no cluster or database
# credentials.
FROM python:3.13-slim AS build
COPY --from=ghcr.io/astral-sh/uv:0.12 /uv /usr/local/bin/uv
WORKDIR /app
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy
COPY intelligence/pyproject.toml intelligence/uv.lock intelligence/README.md ./
RUN uv sync --frozen --no-dev --no-install-project
COPY intelligence/src src
COPY intelligence/gen gen
RUN uv sync --frozen --no-dev

FROM python:3.13-slim
RUN useradd --uid 65532 --no-create-home themis
COPY --from=build /app /app
ENV PATH=/app/.venv/bin:$PATH PYTHONUNBUFFERED=1
USER 65532
ENTRYPOINT ["python", "-m", "themis_intelligence"]
