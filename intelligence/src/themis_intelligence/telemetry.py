"""Logs and metrics, with the field names and label rules of the Go components."""

import json
import logging
import traceback
from collections.abc import Generator, Iterable
from contextlib import contextmanager
from contextvars import ContextVar
from datetime import UTC, datetime
from typing import Any

from prometheus_client import Counter, Histogram, start_http_server

COMPONENT = "intelligence"

# What a line is about. asyncio.create_task and asyncio.to_thread copy the
# context, so an investigation's tasks keep its fields; a raw thread would not.
incident_id: ContextVar[str | None] = ContextVar("incident_id", default=None)
lease_id: ContextVar[str | None] = ContextVar("lease_id", default=None)

_CONTEXT = (("incident_id", incident_id), ("lease_id", lease_id))

# Go's slog names for Python's levels.
_LEVELS = {"WARNING": "WARN", "CRITICAL": "ERROR"}

# The attributes every LogRecord has; anything else came in `extra`.
_RECORD_ATTRS = frozenset(vars(logging.LogRecord("", 0, "", 0, "", None, None))) | {
    "message",
    "asctime",
    "taskName",
}


@contextmanager
def fields(*, incident: str, lease: str) -> Generator[None]:
    """Logs the lines of the block, and of the tasks it creates, with these fields."""
    tokens = [incident_id.set(incident), lease_id.set(lease)]
    try:
        yield
    finally:
        lease_id.reset(tokens[1])
        incident_id.reset(tokens[0])


class JSONFormatter(logging.Formatter):
    """One JSON object per line: time, level, msg, component, then fields."""

    def format(self, record: logging.LogRecord) -> str:
        line: dict[str, Any] = {
            "time": datetime.fromtimestamp(record.created, UTC).isoformat(timespec="milliseconds"),
            "level": _LEVELS.get(record.levelname, record.levelname),
            "msg": record.getMessage(),
            "component": COMPONENT,
        }
        for name, var in _CONTEXT:
            if (value := var.get()) is not None:
                line[name] = value
        for key, value in vars(record).items():
            if key not in _RECORD_ATTRS:
                line[key] = value
        if record.exc_info and record.exc_info[1] is not None:
            e = record.exc_info[1]
            line["error"] = f"{type(e).__name__}: {e}"
            line["stack"] = "".join(traceback.format_exception(e))
        return json.dumps(line, default=str)


def configure_logging(level: int | str = logging.INFO) -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(JSONFormatter())
    logging.basicConfig(level=level, handlers=[handler], force=True)


# Label names metrics may use: closed sets only, never an incident ID,
# namespace, workload name or error text.
METRIC_LABELS = frozenset({"route", "outcome", "kind", "method", "code"})

# The most distinct values a label may take.
LABEL_CEILING = 20

OTHER = "other"

_routes: set[str] = set()


def configure_routes(names: Iterable[str]) -> None:
    """Bounds the route label to the configured routes; any other is OTHER."""
    _routes.clear()
    _routes.update(names)


def route_label(name: str) -> str:
    return name if name in _routes else OTHER


# Outcomes of a model request: ok, or the kind of its error.
MODEL_OUTCOMES = (
    "ok",
    "validation_failed",
    "budget_exceeded",
    "ineligible",
    "deadline_exceeded",
    "unavailable",
)

# Budgets an investigation can run out of.
BUDGET_KINDS = ("tokens", "steps", "evidence", "deadline")

model_requests = Counter(
    "themis_model_requests_total",
    "Model requests, by route and outcome: ok, or the kind of error.",
    ["route", "outcome"],
)
model_request_duration = Histogram(
    "themis_model_request_duration_seconds",
    "Time to a model request's result, repairs included, by route.",
    ["route"],
    buckets=(0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300),
)
model_tokens = Counter(
    "themis_model_tokens_total",
    "Tokens the model reported using, by route and kind: input or output.",
    ["route", "kind"],
)
model_cost = Counter(
    "themis_model_cost_usd_total",
    "Cost of model requests in US dollars, at the route's configured price. "
    "Routes without a price report nothing.",
    ["route"],
)
model_validation_failures = Counter(
    "themis_model_validation_failures_total",
    "Model replies that did not match the required schema, repaired or not, by route.",
    ["route"],
)
investigation_steps = Histogram(
    "themis_investigation_steps",
    "Model steps an investigation used before concluding.",
    buckets=(1, 2, 3, 4, 5, 6, 8, 10, 12, 16),
)
budget_exhausted = Counter(
    "themis_budget_exhausted_total",
    "Investigations that ran out of a budget, by kind: tokens, steps, evidence or deadline.",
    ["kind"],
)
core_requests = Counter(
    "themis_core_requests_total",
    "Calls to themis-core, by gRPC method and status code.",
    ["method", "code"],
)


def init_route(name: str, priced: bool) -> None:
    """Reports zero for a route's series until it is used."""
    for outcome in MODEL_OUTCOMES:
        model_requests.labels(name, outcome)
    model_request_duration.labels(name)
    for kind in ("input", "output"):
        model_tokens.labels(name, kind)
    model_validation_failures.labels(name)
    if priced:
        model_cost.labels(name)


for _kind in BUDGET_KINDS:
    budget_exhausted.labels(_kind)


def serve_metrics(port: int) -> None:
    """Serves /metrics on port, on a daemon thread; port 0 serves nothing."""
    if port:
        start_http_server(port)
