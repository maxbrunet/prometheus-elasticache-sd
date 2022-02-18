FROM golang:1.17.7-alpine@sha256:d030a987c28ca403007a69af28ba419fca00fc15f08e7801fc8edee77c00b8ee AS build

# renovate: datasource=go depName=github.com/prometheus/promu
ARG PROMU_VERSION=v0.13.0

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /go/src/app

RUN apk add --no-cache git \
    && go install "github.com/prometheus/promu@${PROMU_VERSION}"

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build promu build --verbose

FROM quay.io/prometheus/busybox@sha256:a29e40943fc35e4a0ee9c9c039b10580119b1a7299f1e3ce2fd3fe7cb4c8913c

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
