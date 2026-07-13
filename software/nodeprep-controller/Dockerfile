# Build
FROM --platform=$BUILDPLATFORM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /out/nodeprep-controller main.go

# Run
FROM gcr.io/distroless/static:nonroot
USER nonroot:nonroot
COPY --from=build /out/nodeprep-controller /nodeprep-controller
ENTRYPOINT ["/nodeprep-controller"]
