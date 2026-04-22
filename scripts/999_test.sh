#!/usr/bin/env bash
set -euo pipefail
BASE="${1:-http://localhost:8080}"
echo "=== http2tor smoke tests against $BASE ==="

echo "--- [1] Single IP (known Tor exit) ---"
curl -sf -X POST "$BASE/api/v1/istor" \
  -H "Content-Type: application/json" \
  -d '{"ip":"185.220.101.1"}' | jq .

echo "--- [2] Single IP (not Tor) ---"
curl -sf -X POST "$BASE/api/v1/istor" \
  -H "Content-Type: application/json" \
  -d '{"ip":"8.8.8.8"}' | jq .

echo "--- [3] Batch IPs ---"
curl -sf -X POST "$BASE/api/v1/istor" \
  -H "Content-Type: application/json" \
  -d '{"ips":["185.220.101.1","78.109.18.140","1.1.1.1"]}' | jq .

echo "--- [4] IPv6 ---"
curl -sf -X POST "$BASE/api/v1/istor" \
  -H "Content-Type: application/json" \
  -d '{"ip":"2a0b:f4c2::1"}' | jq .

echo "--- [5] GET /openapi.json ---"
curl -sf "$BASE/openapi.json" | jq .info.title

echo "--- [6] GET /db/tor (header only) ---"
curl -sf -I "$BASE/db/tor" | head -3

echo "=== All tests complete ==="
