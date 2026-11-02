# themis-core and themis-executor. Build one with --build-arg BIN=<name>.
FROM golang:1.27 AS build
ARG BIN=themis-core
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY gen gen
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
