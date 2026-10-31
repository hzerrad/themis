"""Logs with the field names of the Go components."""

import json
import logging
import traceback
from collections.abc import Generator
from contextlib import contextmanager
from contextvars import ContextVar
from datetime import UTC, datetime
from typing import Any

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
