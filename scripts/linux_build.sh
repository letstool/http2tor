#!/bin/bash

go build \
    -trimpath \
    -ldflags="-extldflags -static -s -w" \
    -o ./out/http2tor ./cmd/http2tor
