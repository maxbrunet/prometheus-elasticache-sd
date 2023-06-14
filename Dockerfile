FROM --platform="${BUILDPLATFORM}" docker.io/library/golang:1.20.5-alpine@sha256:a5634ef8fa1d21bf8062c5c000060c31cd24e14ccc3ee69f08751e0ffd01e758 AS build

# renovate: datasource=go depName=github.com/prometheus/promu
ARG PROMU_VERSION=v0.14.0
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/app

# hadolint ignore=DL3018
RUN apk add --no-cache git \
    && go install "github.com/prometheus/promu@${PROMU_VERSION}"

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" promu build --verbose

FROM --platform="${TARGETPLATFORM}" quay.io/prometheus/busybox@sha256:9a02a4a070d63298c2d3aaaa2551871e1fb40bba8c032f879a19246a273c2819

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
