#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p ./db

# LICENSE_KEY: your CDN license token (optional — anonymous if unset).
# TOR_DB_URL:  set to another http2tor instance to enable peer sync mode instead of CDN.

LISTEN_ADDR=127.0.0.1:8080 \
TOR_DB_DIR=./db \
TOR_MAX_IPS=100 \
LICENSE_KEY="${LICENSE_KEY:-}" \
./out/http2tor
