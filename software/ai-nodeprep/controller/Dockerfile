# Multi-stage, multi-arch (amd64 + arm64) build for both binaries:
#
#   docker buildx build --platform linux/amd64,linux/arm64 --target agent -t ... --push .
#
# The build stage is pinned to $BUILDPLATFORM so the Go toolchain always runs
# natively and cross-compiles via GOARCH — no QEMU for the compile. Only the
# runtime stage's apk layer emulates when building for the foreign arch
# (needs binfmt: docker run --privileged tonistiigi/binfmt --install all).
# The agent target installs util-linux because the default reboot command is
# `nsenter -t 1 -m -u -i -n -- systemctl reboot`.
FROM --platform=$BUILDPLATFORM golang:1.24.1-alpine AS build
WORKDIR /src
# TARGETOS/TARGETARCH are injected by buildx per requested platform; the
# defaults keep a plain (non-buildx) `docker build` working too.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /out/controller ./cmd/controller && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /out/agent ./cmd/agent

FROM alpine:3.20 AS base
RUN apk add --no-cache util-linux ca-certificates
COPY --from=build /out/ /usr/local/bin/

FROM base AS controller
ENTRYPOINT ["/usr/local/bin/controller"]

FROM base AS agent
ENTRYPOINT ["/usr/local/bin/agent"]
