FROM --platform="${BUILDPLATFORM}" golang:1.18.2-alpine@sha256:e6b729ae22a2f7b6afcc237f7b9da3a27151ecbdcd109f7ab63a42e52e750262 AS build

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

FROM quay.io/prometheus/busybox@sha256:a91bcb2a92b4293e6fd5c78c6ef138e054e28f9cc013e6c9e23661507aa596d0

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
