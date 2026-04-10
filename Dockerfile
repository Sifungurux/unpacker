FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o unpacker ./cmd/unpacker

FROM alpine:3.21
ARG UMOCI_VERSION=0.4.7
RUN apk add --no-cache ca-certificates wget && \
    wget -q -O /tmp/umoci \
      https://github.com/opencontainers/umoci/releases/download/v${UMOCI_VERSION}/umoci.amd64 && \
    wget -q -O /tmp/umoci.sha256sum \
      https://github.com/opencontainers/umoci/releases/download/v${UMOCI_VERSION}/umoci.sha256sum && \
    grep 'umoci\.amd64' /tmp/umoci.sha256sum | awk '{print $1 "  /tmp/umoci"}' | sha256sum -c - && \
    install -m 755 /tmp/umoci /usr/local/bin/umoci && \
    rm /tmp/umoci /tmp/umoci.sha256sum && \
    apk del wget
RUN addgroup -S unpacker && adduser -S -G unpacker unpacker
COPY --from=builder /build/unpacker /usr/local/bin/unpacker
USER unpacker
WORKDIR /home/unpacker
ENTRYPOINT ["unpacker"]
