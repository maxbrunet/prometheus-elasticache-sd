FROM --platform="${BUILDPLATFORM}" golang:1.19.3-alpine@sha256:dc4f4756a4fb91b6f496a958e11e00c0621130c8dfbb31ac0737b0229ad6ad9c AS build

# renovate: datasource=go depName=github.com/prometheus/promu
ARG PROMU_VERSION=v0.13.0
ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/app

RUN apk add --no-cache git \
    && go install "github.com/prometheus/promu@${PROMU_VERSION}"

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" promu build --verbose

FROM quay.io/prometheus/busybox@sha256:536a924e8cd83a8c6254e6d4a05d4913f28b883c13bcf0573998907eb791783e

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
