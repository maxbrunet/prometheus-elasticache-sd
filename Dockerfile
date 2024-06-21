FROM --platform="${BUILDPLATFORM}" docker.io/library/golang:1.22.4-alpine@sha256:8274bcfe89f5989777cb759302a7ced08c8a712c81982ca3ecc9fce0626592f1 AS build

# renovate: datasource=go depName=github.com/prometheus/promu
ARG PROMU_VERSION=v0.17.0
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

FROM --platform="${TARGETPLATFORM}" quay.io/prometheus/busybox@sha256:dfa54ef35e438b9e71ac5549159074576b6382f95ce1a434088e05fd6b730bc4

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
