"""Traces the investigator's work over OTLP when an endpoint is set.

Without OTEL_EXPORTER_OTLP_ENDPOINT (or its _TRACES_ form) nothing is set up:
the API's default provider records nothing and makes no network calls.
"""

import os
from collections.abc import Callable

from opentelemetry import trace
from opentelemetry.sdk.resources import SERVICE_NAME, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter

tracer = trace.get_tracer("themis_intelligence")


def endpoint_set() -> bool:
    env = os.environ.get
    return bool(env("OTEL_EXPORTER_OTLP_ENDPOINT") or env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))


def setup_tracing() -> Callable[[], None]:
    """Exports spans when an endpoint is set; returns what flushes them at exit."""
    if not endpoint_set():
        return lambda: None
    provider = TracerProvider(resource=Resource.create({SERVICE_NAME: "themis-intelligence"}))
    provider.add_span_processor(BatchSpanProcessor(_exporter()))
    trace.set_tracer_provider(provider)
    return provider.shutdown


def _exporter() -> SpanExporter:
    env = os.environ.get
    protocol = env("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL") or env("OTEL_EXPORTER_OTLP_PROTOCOL")
    if protocol in (None, "", "grpc"):
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter

        return OTLPSpanExporter()
    if protocol == "http/protobuf":
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
            OTLPSpanExporter as HTTPSpanExporter,
        )

        return HTTPSpanExporter()
    raise SystemExit("OTEL_EXPORTER_OTLP_PROTOCOL must be grpc or http/protobuf")
