# Cross-compiled from the build platform rather than emulated on the target:
# CGO is off, so GOARCH is all that is needed.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o unpacker ./cmd/unpacker

FROM alpine:3.21
ARG TARGETARCH
ARG UMOCI_VERSION=0.6.0
# Hashes are pinned here rather than taken from the checksum file published
# alongside the binary: fetching both from the same release means a compromised
# release verifies against itself. Recorded 2026-08-17 from the PGP-signed
# https://github.com/opencontainers/umoci/releases/download/v0.6.0/umoci.sha256sum
ARG UMOCI_SHA256_AMD64=b51c267ec394499e42c6fde47f240b7b7dba57ea49df0b5acd304378b82a3b71
ARG UMOCI_SHA256_ARM64=5cfd17f2e7a4bcf9ed67ea1b955ca893d200349b9ce6a3d3707dba415f458a1f

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) umoci_sha="${UMOCI_SHA256_AMD64}" ;; \
      arm64) umoci_sha="${UMOCI_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH '${TARGETARCH}': add its umoci hash to the Dockerfile" >&2; exit 1 ;; \
    esac; \
    apk add --no-cache ca-certificates wget; \
    wget -q -O /tmp/umoci \
      "https://github.com/opencontainers/umoci/releases/download/v${UMOCI_VERSION}/umoci.linux.${TARGETARCH}"; \
    echo "${umoci_sha}  /tmp/umoci" | sha256sum -c -; \
    install -m 755 /tmp/umoci /usr/local/bin/umoci; \
    rm /tmp/umoci; \
    apk del wget

RUN addgroup -S unpacker && adduser -S -G unpacker unpacker
COPY --from=builder /build/unpacker /usr/local/bin/unpacker
USER unpacker
WORKDIR /home/unpacker
ENTRYPOINT ["unpacker"]
