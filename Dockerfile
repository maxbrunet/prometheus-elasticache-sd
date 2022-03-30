FROM --platform="${BUILDPLATFORM}" golang:1.18.0-alpine@sha256:3afd220509acf9866e91932a3a41bf341b8bada82107ef3ecce3422826b98064 AS build

# renovate: datasource=go depName=github.com/prometheus/promu
ARG PROMU_VERSION=v0.13.0
ARG TARGETOS
ARG TARGETARCH

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
RUN --mount=type=cache,target=/root/.cache/go-build GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" promu build --verbose

FROM quay.io/prometheus/busybox@sha256:bb097b0c990ff3f84affd8263a101988b8b8a69964e4dca31d9a0903aee8cec3

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
