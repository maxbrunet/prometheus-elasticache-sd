FROM --platform="${BUILDPLATFORM}" docker.io/library/golang:1.21.1-alpine@sha256:4bc6541af94a67d9dcabba9826c36e4e9497dacf4e8755ac503000b6ff75318f AS build

# renovate: datasource=go depName=github.com/prometheus/promu
ARG PROMU_VERSION=v0.15.0
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

FROM --platform="${TARGETPLATFORM}" quay.io/prometheus/busybox@sha256:928297c21d095c250b260459bd7860d610d98ee9b283a81a3883bd3806583384

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
