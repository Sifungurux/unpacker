FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o unpacker ./cmd/unpacker

FROM alpine:3.21
ARG UMOCI_VERSION=0.4.7
RUN apk add --no-cache ca-certificates wget && \
    wget -O /usr/local/bin/umoci \
      https://github.com/opencontainers/umoci/releases/download/v${UMOCI_VERSION}/umoci.amd64 && \
    echo "f28b37c38479f2b6e928bfb83f0b0e19f5f68c0b99c7e0c47c16abc75c47cde7  /usr/local/bin/umoci" | sha256sum -c - && \
    chmod +x /usr/local/bin/umoci && \
    apk del wget
RUN addgroup -S unpacker && adduser -S -G unpacker unpacker
COPY --from=builder /build/unpacker /usr/local/bin/unpacker
USER unpacker
ENTRYPOINT ["unpacker"]
