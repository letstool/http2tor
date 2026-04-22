#!/usr/bin/env bash
set -euo pipefail

# Mode 1 (default): fetch gzipped CSV from the CDN and compile tor.mmdb.
# Set LICENSE_KEY to your token for licensed (higher quota) access.
docker run -it --rm \
  -p 8080:8080 \
  -v "$(pwd)/db:/data:rw" \
  -e LISTEN_ADDR=0.0.0.0:8080 \
  -e TOR_MAX_IPS=100 \
  -e LICENSE_KEY="${LICENSE_KEY:-}" \
  letstool/http2tor:latest

# Mode 2 (peer): download tor.mmdb from another running http2tor instance.
# Uncomment and set TOR_DB_URL to use this mode:
#
# docker run -it --rm \
#   -p 8080:8080 \
#   -v "$(pwd)/db:/data:rw" \
#   -e LISTEN_ADDR=0.0.0.0:8080 \
#   -e TOR_DB_URL=http://upstream-host:8080 \
#   letstool/http2tor:latest
