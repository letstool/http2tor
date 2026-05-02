# syntax=docker/dockerfile:1.4

##############################
#  Stage 1: Build (golang)
##############################
FROM golang:1.24-alpine AS builder

# Go build environment variables
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GO111MODULE=on

WORKDIR /src

# Download dependencies (cached as a separate layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree
COPY . .

# Compile the static binary
# The output path is fixed: /out/http2tor
RUN go build \
    -trimpath \
    -ldflags="-s -w -extldflags -static" \
    -o /out/http2tor ./cmd/http2tor

##############################
#  Stage 2: Runtime (scratch)
##############################
FROM scratch AS runtime

# Copy only the binary from the builder stage
COPY --from=builder /out/http2tor /http2tor

# OCI image metadata
LABEL org.opencontainers.image.title="http2tor" \
      org.opencontainers.image.description="HTTP Server → tor (Go)" \
      org.opencontainers.image.source="https://github.com/letstool/http2tor" \
      org.opencontainers.image.authors="LetsTool.net" \
      org.opencontainers.image.licenses="Apache-2.0"

VOLUME ["/data"]

# Default runtime environment variables (all overridable at docker run time)
ENV LISTEN_ADDR="0.0.0.0:8080" \
    TOR_DB_DIR="/data" \
    TOR_DB_URL="" \
    TOR_MAX_IPS="100" \
    LICENSE_KEY=""

EXPOSE 8080

USER 65534:65534

# Entry point
ENTRYPOINT ["/http2tor"]
