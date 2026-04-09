FROM golang:1.25-alpine AS builder
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
    chmod +x /usr/local/bin/umoci
COPY --from=builder /build/unpacker /usr/local/bin/unpacker
ENTRYPOINT ["unpacker"]
