FROM --platform="${BUILDPLATFORM}" golang:1.19.3-alpine@sha256:8558ae624304387d18694b9ea065cc9813dd4f7f9bd5073edb237541f2d0561b AS build

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

FROM quay.io/prometheus/busybox@sha256:233584cadef54ec9ce40c9989a5bdbc04d8e5efeaf21c903cd77faa27d23de1f

LABEL \
  org.opencontainers.image.source="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.url="https://github.com/maxbrunet/prometheus-elasticache-sd" \
  org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /go/src/app/prometheus-elasticache-sd /bin/prometheus-elasticache-sd

USER 1000:1000

ENTRYPOINT ["/bin/prometheus-elasticache-sd"]
