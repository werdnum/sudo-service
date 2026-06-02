# syntax=docker/dockerfile:1
#
# sudo-service: human-approved privileged command execution for cluster agents.
# See README.md for the full design.

# Pin the builder to the runner's native platform and cross-compile to
# TARGETARCH (CGO is disabled), so multi-arch builds need no QEMU emulation —
# only the final COPY-only stage is per-target.
FROM --platform=$BUILDPLATFORM golang:1.24-bullseye AS builder
WORKDIR /app
ENV CGO_ENABLED=0
ARG TARGETARCH

# Cache module downloads independent of source so iterative builds are fast.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY *.go ./
COPY templates ./templates
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/sudo-service .

# Distroless static for a small, non-root final image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/sudo-service /sudo-service
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/sudo-service"]
