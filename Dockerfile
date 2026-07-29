# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# No third-party dependencies, so this is a no-op today. It is kept so that
# adding one later does not silently invalidate the source cache.
COPY go.mod ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# VERSION is injected from the git tag by CI and matches the Makefile's
# -X main.version.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/o365-log-extractor ./cmd/o365-log-extractor

# The state file's *directory* must be writable by the runtime UID: state.Save
# writes a temp file alongside the target and renames over it. Creating /data
# here with the right ownership also fixes the ownership of a named volume
# mounted over it on first use.
RUN mkdir -p /data && chown 65532:65532 /data


FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="o365-log-extractor" \
      org.opencontainers.image.description="Imports Office 365 unified audit logs into VictoriaLogs" \
      org.opencontainers.image.source="https://github.com/alex-j-butler/o365-log-extractor" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/o365-log-extractor /usr/local/bin/o365-log-extractor
COPY --from=build --chown=65532:65532 /data /data

USER 65532:65532

# -state-file defaults to the relative "o365-extractor.state.json", so WORKDIR
# alone lands state on the volume with no flag needed.
WORKDIR /data

# Exec form: the binary becomes PID 1 and receives SIGTERM directly, which
# signal.NotifyContext already handles. No init shim required.
ENTRYPOINT ["/usr/local/bin/o365-log-extractor"]
CMD ["-mode", "api", "-follow"]
