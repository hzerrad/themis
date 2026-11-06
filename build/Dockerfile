# themis-core and themis-executor. Build one with --build-arg BIN=<name>.
# Go builds for the target platform from the build machine's own, so an arm64
# image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG BIN=themis-core
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# Caches keep modules and builds between runs, so a rebuild fetches nothing.
# The module proxy sometimes fails for a moment: retry before failing the build.
RUN --mount=type=cache,target=/go/pkg/mod \
    for i in 1 2 3 4 5; do \
      go mod download && exit 0; \
      echo "module download failed (attempt $i of 5)" >&2; \
      [ $i -lt 5 ] && sleep $((i * 10)); \
    done; exit 1
COPY cmd cmd
COPY gen gen
COPY internal internal
COPY scenarios scenarios
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
