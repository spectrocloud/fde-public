# Multi-stage build for both binaries. The agent target installs util-linux
# because the default reboot command is `nsenter -t 1 ... systemctl reboot`.
FROM golang:1.24.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/
ARG TARGETOS=linux
ARG TARGETARCH=amd64
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
