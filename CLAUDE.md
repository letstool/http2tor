# CLAUDE.md — http2tor

This file provides context for AI-assisted development on the `http2tor` project.

---

## Project overview

`http2tor` is a single-binary HTTP gateway that exposes Tor network detection as a JSON REST API.
It is written entirely in Go and embeds all static assets (web UI, favicon, OpenAPI spec) at compile
time using `//go:embed` directives, so the resulting binary has zero runtime file dependencies.

The server accepts `POST /api/v1/istor` requests containing one or more IP addresses and returns
structured Tor relay data. The database is **built automatically** from a gzipped CSV fetched from
the **letstool CDN** (`https://cdn.letstool.net/tor/csv`) and refreshed at a configurable interval.
An optional `LICENSE_KEY` enables licensed (higher-quota) CDN access; the server works anonymously
without one.

---

## Repository layout

```
.
├── api/
│   └── swagger.yaml              # OpenAPI 3.1 source (human-editable)
├── build/
│   └── Dockerfile                # Two-stage Docker build (builder + scratch runtime)
├── cmd/
│   └── http2tor/
│       ├── main.go               # Entire application — single file
│       └── static/
│           ├── favicon.png       # Embedded at build time
│           ├── index.html        # Embedded web UI (dark/light, 15 languages, RTL support)
│           └── openapi.json      # Embedded OpenAPI spec (generated from swagger.yaml)
├── scripts/
│   ├── 000_init.sh               # go mod tidy
│   ├── 999_test.sh               # Integration smoke tests (curl + jq)
│   ├── linux_build.sh            # Native static binary build
│   ├── linux_run.sh              # Run binary on Linux
│   ├── docker_build.sh           # Build Docker image
│   ├── docker_run.sh             # Run Docker container
│   ├── windows_build.cmd         # Native build on Windows
│   └── windows_run.cmd           # Run binary on Windows
├── go.mod
├── go.sum
├── LICENSE                       # MIT
├── README.md
└── CLAUDE.md                     # This file
```

---

## Key design decisions

- **Single `main.go`**: the entire server logic lives in `cmd/http2tor/main.go`. There are no internal packages.
- **Embedded assets**: `favicon.png`, `index.html`, and `openapi.json` are embedded with `//go:embed`. Any change to these files is picked up at the next `go build`.
- **Static binary**: the build uses `CGO_ENABLED=0` and `-ldflags "-extldflags -static"`. Do not introduce `cgo` dependencies.
- **No framework**: the HTTP layer uses only the standard library (`net/http`). Do not add a router or web framework.
- **Custom mmdb**: the server builds its own MaxMind-compatible mmdb using `github.com/maxmind/mmdbwriter` with a custom `http2tor-TorDB` schema. Reading is done via `github.com/oschwald/maxminddb-golang` directly. **Important**: in `mmdbwriter v1.0.0` the insertion method is `writer.Insert(network, record)` — not `InsertNetwork` (which does not exist in this version).
- **Two update modes** controlled by `TOR_DB_URL`:
  - **CDN CSV mode** (`TOR_DB_URL` unset, default): `buildTorDBFromCSV` fetches a gzipped CSV from `https://cdn.letstool.net/tor/csv`, decompresses it on the fly via `compress/gzip`, parses it with `encoding/csv`, and compiles a fresh `tor.mmdb` via `mmdbwriter`. If `LICENSE_KEY` is set it is sent as `Authorization: Basic <token>`.
  - **Peer mode** (`TOR_DB_URL` set): `downloadFromPeer` downloads `tor.mmdb` directly from the `/db/tor` endpoint of another `http2tor` instance. No CDN access needed.
- **CDN protocol**: `fetchCSVFromCDN` sends `If-Modified-Since` (read from `.last_modified_tor`) on every request. The switch on the CDN status code handles five cases:
  - **304 Not Modified** — costs no quota; treated as success (timestamp refreshed, build skipped). Returns the sentinel `errNotModified`.
  - **429 Too Many Requests** — returns `*errRateLimited` containing the `Retry-After` unix timestamp.
  - **410 Gone** — product is disabled on the CDN side; returns `*errProductGone` with the JSON body message.
  - **401 Unauthorized** — license level insufficient; returns `*errUnauthorized` with the human-readable `message` field extracted from the CDN JSON body via `extractJSONMessage`.
  - **200 OK** — `Last-Modified` is stored in `.last_modified_tor` for subsequent `If-Modified-Since` requests; CSV stream returned to caller.
- **Hot database swap**: the active `*maxminddb.Reader` is stored in a `sync/atomic.Value` via `dbValue.Swap()`. Both modes call `swapDB()` which atomically replaces the reader and closes the old one — in-flight requests are never interrupted.
- **Node type classification**: read directly from the `node_type` column of the CDN CSV (pre-classified). The values are: `guard`, `exit`, `guard_and_exit`, `middle`, `authority`.
- **Periodic scheduler**: a `time.Timer`-based goroutine calls `updateDB` every **4 hours** (hardcoded `updateInterval` constant). The goroutine maintains a local `goneAttempt` counter (starts at 0) and dispatches on the error type returned by `updateDB`:
  - **nil (success)** — resets `goneAttempt` to 0; timer reset to normal 4-hour interval.
  - **`*errRateLimited` (CDN 429)** — timer reset to `Retry-After - now`; resumes normal cycle on the next success.
  - **`*errProductGone` (CDN 410)** — waits `goneRetrySchedule[goneAttempt]` (24 h, 48 h, 72 h, 96 h in order), increments `goneAttempt`; if all four slots are exhausted the goroutine returns permanently. A subsequent 200 resets the counter to 0.
  - **`*errUnauthorized` (CDN 401)** — logs the server message and the configuration hint, then the goroutine returns permanently (no further retries).
  - **Any other error** — logs the error and resets to the normal 4-hour interval.
  The same 4-hour interval applies in peer mode; CDN-specific error types are never produced by `downloadFromPeer`. `updateDB` dispatches to `downloadFromPeer` or `buildTorDBFromCSV` depending on whether `TOR_DB_URL` is set. `.last_update_tor` stores the Unix timestamp of the last successful update; `ensureDB` reuses the cached DB if its age is below `updateInterval`.
- **Marker files** in `TOR_DB_DIR`:
  - `.last_update_tor` — Unix timestamp of the last successful build/download.
  - `.last_modified_tor` — `Last-Modified` HTTP header value from the last CDN 200 response; sent as `If-Modified-Since` on the next request.
- **HTTP proxy support**: all outbound HTTP clients are created via `newHTTPClient(timeout)` which sets `Proxy: http.ProxyFromEnvironment` on the transport. This honours `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` (and their lowercase variants) identically to curl. The effective proxy URL is resolved and logged once at startup by `logProxyConfig()`; passwords are redacted to `***`.
- **Batch lookups**: a single request may contain either one IP (`ip` field) or a list (`ips` field). The two fields are mutually exclusive. Maximum batch size enforced by `TOR_MAX_IPS` / `-max-ips`.
- **`/db/tor` endpoint**: serves the current `tor.mmdb` file — used by peer instances in peer mode.

---

## Environment variables & CLI flags

Every configuration value can be set via an environment variable **or** a command-line flag. The flag always takes priority. Resolution order: **CLI flag → environment variable → hard-coded default**.

| Environment variable | CLI flag        | Default          | Description                                              |
|----------------------|-----------------|------------------|----------------------------------------------------------|
| `LISTEN_ADDR`        | `-listen-addr`  | `127.0.0.1:8080` | Listen address and port for the HTTP server.             |
| `TOR_DB_DIR`         | `-db-dir`       | `/data`          | Directory used to store and cache the `tor.mmdb` file.  |
| `TOR_DB_URL`         | `-db-url`       | *(none)*         | Base URL of a peer http2tor instance. When set, enables peer mode: the DB is downloaded from `{TOR_DB_URL}/db/tor` every 4 hours instead of being built from the CDN CSV. |
| `LICENSE_KEY`        | `-license-key`  | *(none)*         | CDN license token. Sent as `Authorization: Basic <token>`. Optional — anonymous if unset. |
| `TOR_MAX_IPS`        | `-max-ips`      | `100`            | Maximum IPs accepted in a single batch request.          |

**Proxy variables** (no CLI flag — curl-compatible convention, no sentinel needed):

| Variable | Description |
|---|---|
| `HTTPS_PROXY` / `https_proxy` | Proxy URL for HTTPS traffic (CDN and peer). Supports `http://`, `https://`, `socks5://`. |
| `HTTP_PROXY` / `http_proxy`   | Proxy URL for plain HTTP traffic. |
| `NO_PROXY` / `no_proxy`       | Comma-separated bypass list (hostnames, IPs, CIDRs). |

Proxy is applied via `http.ProxyFromEnvironment` inside `newHTTPClient()`. The resolved proxy URL (or "none") is logged once at startup by `logProxyConfig()`. Password in proxy URL is redacted to `***` in logs.

CLI flags are parsed with the standard library `flag` package using a sentinel default (`"\x00"` for strings, `-1` for integers) to distinguish "flag not provided" from an explicit value. `TOR_MAX_IPS` uses `-1` as the integer sentinel.

---

## Data model

### mmdb record schema (`TorRecord`)

Each IP is stored in `tor.mmdb` with the following fields:

| mmdb key           | Go type  | Description                                               |
|--------------------|----------|-----------------------------------------------------------|
| `is_tor`           | bool     | Always `true` for IPs in the database                    |
| `node_type`        | string   | `guard`, `exit`, `guard_and_exit`, `middle`, `authority` |
| `flags`            | []string | Raw Tor consensus flags (pipe-separated in CSV source)   |
| `nickname`         | string   | Relay nickname                                            |
| `fingerprint`      | string   | 40-hex SHA-1 relay fingerprint                            |
| `country`          | string   | ISO 3166-1 alpha-2 country code (lowercase in mmdb)      |
| `latitude`         | float64  | Approximate relay latitude                                |
| `longitude`        | float64  | Approximate relay longitude                               |
| `as`               | string   | Autonomous System number                                  |
| `as_name`          | string   | Autonomous System name                                    |
| `first_seen`       | string   | UTC datetime string (`YYYY-MM-DD HH:MM:SS`)               |
| `last_seen`        | string   | UTC datetime string (`YYYY-MM-DD HH:MM:SS`)               |
| `consensus_weight` | uint64   | Relay bandwidth weight                                    |
| `is_guard`         | bool     | Has Guard flag                                            |
| `is_exit`          | bool     | Has Exit flag                                             |
| `is_middle`        | bool     | Neither Guard nor Exit nor Authority                      |
| `is_authority`     | bool     | Has Authority flag                                        |
| `is_hsdir`         | bool     | Has HSDir flag                                            |

IPs not in the database return zero-valued `TorRecord` structs — `is_tor` will be `false`.

### CSV source format

The CDN serves a gzipped CSV at `https://cdn.letstool.net/tor/csv` with exactly 18 columns (no deduplication needed — one row per IP):

```
ip,node_type,flags,nickname,fingerprint,country,latitude,longitude,as,as_name,
first_seen,last_seen,consensus_weight,is_guard,is_exit,is_middle,is_authority,is_hsdir
```

- `flags` uses `|` as separator within the field (e.g. `Fast|Running|Valid`)
- `as_name` may contain commas and is properly CSV-quoted
- Boolean fields are the strings `true` or `false`

---

## API contract

### Endpoint

```
POST /api/v1/istor
Content-Type: application/json
```

Exactly one of `ip` or `ips` must be provided per request.

### Response status values

| Value      | Meaning                                                               |
|------------|-----------------------------------------------------------------------|
| `SUCCESS`  | At least one Tor node found; `answers` contains Tor entries          |
| `NOTFOUND` | All IPs checked but none are Tor nodes                               |
| `ERROR`    | Request malformed, invalid IP, or database not yet initialised       |

### Other endpoints

| Method | Path            | Description                                            |
|--------|-----------------|--------------------------------------------------------|
| `GET`  | `/`             | Embedded interactive web UI                            |
| `GET`  | `/openapi.json` | OpenAPI 3.1 specification                              |
| `GET`  | `/favicon.png`  | Application icon                                       |
| `GET`  | `/db/tor`       | Serves the current `tor.mmdb` for peer download        |

---

## Web UI

The UI is a self-contained single-file HTML/JS/CSS application embedded in the binary.

- **Themes**: dark and light, switchable via a toggle button. Dark theme uses a cyan-blue accent (`#00d4ff`) matching the letstool project palette.
- **Languages**: 15 locales built in — Arabic (`ar`), Bengali (`bn`), German (`de`), English (`en`), Spanish (`es`), French (`fr`), Hindi (`hi`), Indonesian (`id`), Japanese (`ja`), Korean (`ko`), Portuguese (`pt-BR`), Russian (`ru`), Urdu (`ur`), Vietnamese (`vi`), Chinese (`zh-CN`). Language is auto-detected from `navigator.languages` and selectable via dropdown.
- **RTL support**: Arabic (`ar`) and Urdu (`ur`) automatically switch the layout to right-to-left via `[dir="rtl"]` CSS rules (header alignment, table text direction, arrow positions, skip link placement, tab arrow keys).
- The UI calls `POST /api/v1/istor` and renders results in a table with:
  - IP address (colored purple for Tor, gray for non-Tor)
  - Status badge (TOR / NOT TOR)
  - Node type pill (color-coded: green=guard, red=exit, orange=guard+exit, purple=middle, yellow=authority)
  - Relay flags as color-coded chips
  - Nickname + shortened fingerprint
  - Country / AS
  - First seen date
  - Consensus weight
- A summary stats bar shows breakdown by node type.
- Copy-to-CSV button exports all results in pipe-delimited format.

To modify the UI, edit `cmd/http2tor/static/index.html` and rebuild.
To update the API spec, edit `api/swagger.yaml` and update `openapi.json` accordingly.

---

## Adding a new configuration parameter

1. Declare the global variable in the `var` block in `main.go`.
2. Add a `TOR_` prefixed environment variable name and document it in this file and in `README.md`.
3. Add a corresponding CLI flag in `main()` using the `flag` package, with a sentinel default.
4. Apply the resolution logic: flag wins if non-sentinel, then env var, then hard-coded default.

---

## Constraints & conventions

- Go version: **1.24+**
- No `cgo`. Keep `CGO_ENABLED=0`.
- No additional HTTP frameworks or routers.
- All logic stays in `cmd/http2tor/main.go` unless there is a strong reason to split it.
- Error responses always return a `TorResponse` JSON body — never a plain-text error.
- The server never logs request bodies; avoid adding logging that could expose queried IP addresses.
- All code, identifiers, comments, and documentation must be written in **English**.
- Every configuration environment variable must have a corresponding command-line flag. The flag always takes priority.

---

## Build & run commands

```bash
# Initialise / tidy dependencies
bash scripts/000_init.sh

# Build native static binary -> ./out/http2tor
bash scripts/linux_build.sh

# Run
bash scripts/linux_run.sh

# Build Docker image -> letstool/http2tor:latest
bash scripts/docker_build.sh

# Run Docker container
bash scripts/docker_run.sh

# Smoke tests (server must be running)
bash scripts/999_test.sh
```

---

## AI-assisted development

This project was developed with the assistance of **Claude Sonnet 4.6** by Anthropic.
