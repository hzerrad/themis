"""The contract between investigation code and models, whatever the provider."""

from typing import Any, Literal

from pydantic import BaseModel, Field

Classification = Literal["public", "internal", "sensitive"]

CLASSIFICATION_RANK: dict[Classification, int] = {"public": 0, "internal": 1, "sensitive": 2}


class Message(BaseModel):
    role: Literal["system", "user", "assistant"]
    content: str


class ModelRequest(BaseModel):
    task_class: str
    incident_id: str
    trace_id: str
    schema_version: str
    data_classification: Classification
    prompt_version: str
    messages: list[Message]
    deadline_s: float
    max_output_tokens: int
    allow_repair: bool = True


class Usage(BaseModel):
    input_tokens: int = 0
    output_tokens: int = 0

    def __add__(self, other: "Usage") -> "Usage":
        return Usage(
            input_tokens=self.input_tokens + other.input_tokens,
            output_tokens=self.output_tokens + other.output_tokens,
        )


ErrorKind = Literal[
    "validation_failed", "budget_exceeded", "ineligible", "deadline_exceeded", "unavailable"
]


class TypedError(BaseModel):
    kind: ErrorKind
    message: str


class ModelResult[T](BaseModel):
    output: T | None = None
    error: TypedError | None = None
    model: str
    endpoint: str
    route: str
    prompt_version: str
    schema_version: str
    usage: Usage = Field(default_factory=Usage)
    latency_ms: int = 0
    repair_attempts: int = 0


class ProviderRequest(BaseModel):
    model: str
    messages: list[Message]
    max_output_tokens: int
    json_schema: dict[str, Any]


class ProviderResponse(BaseModel):
    content: str
    usage: Usage
    model: str
