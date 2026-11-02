# themis-core and themis-executor. Build one with --build-arg BIN=<name>.
FROM golang:1.27 AS build
ARG BIN=themis-core
WORKDIR /src
COPY go.mod go.sum ./
# Caches keep modules and builds between runs, so a rebuild fetches nothing.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY gen gen
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
