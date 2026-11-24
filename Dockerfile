# Build a static controller, following the CAPI/CAPO multi-stage image layout.
# make docker-build passes GO_VERSION from go.mod; Buildx supplies platform args.
ARG GO_VERSION
ARG BUILDPLATFORM
FROM --platform=${BUILDPLATFORM} docker.io/library/golang:${GO_VERSION} AS builder
WORKDIR /workspace

# Fail when a dependency requires a newer compiler instead of downloading one.
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/manager ./cmd && \
    mkdir -p /out/usr/bin && ln -s /manager /out/usr/bin/manager

# Includes system CA certificates for OpenStack HTTPS connections.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
WORKDIR /
# Debian's /bin points to /usr/bin; add the compatibility link without replacing it.
COPY --from=builder /out/ /
USER 65532:65532
EXPOSE 8081
LABEL org.opencontainers.image.source="https://github.com/azimuth-cloud/capi-janitor-openstack-go" \
      org.opencontainers.image.licenses="Apache-2.0"
ENTRYPOINT ["/manager"]
