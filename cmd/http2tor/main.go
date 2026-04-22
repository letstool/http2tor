package main

import (
	_ "embed"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/breml/rootcerts" // embed Mozilla CA bundle as fallback for scratch containers

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/favicon.png
var faviconPNG []byte

//go:embed static/openapi.json
var openapiJSON []byte

/* ---------- mmdb record type ---------- */

// TorRecord is the data stored per IP in the mmdb database.
// Fields use maxminddb tags that match the keys written by mmdbwriter.
type TorRecord struct {
	IsTor           bool     `maxminddb:"is_tor"`
	NodeType        string   `maxminddb:"node_type"`
	Flags           []string `maxminddb:"flags"`
	Nickname        string   `maxminddb:"nickname"`
	Fingerprint     string   `maxminddb:"fingerprint"`
	Country         string   `maxminddb:"country"`
	Latitude        float64  `maxminddb:"latitude"`
	Longitude       float64  `maxminddb:"longitude"`
	AS              string   `maxminddb:"as"`
	ASName          string   `maxminddb:"as_name"`
	FirstSeen       string   `maxminddb:"first_seen"`
	LastSeen        string   `maxminddb:"last_seen"`
	ConsensusWeight uint64   `maxminddb:"consensus_weight"`
	IsGuard         bool     `maxminddb:"is_guard"`
	IsExit          bool     `maxminddb:"is_exit"`
	IsMiddle        bool     `maxminddb:"is_middle"`
	IsAuthority     bool     `maxminddb:"is_authority"`
	IsHSDir         bool     `maxminddb:"is_hsdir"`
}

/* ---------- API types ---------- */

// TorAnswer is one result item in the API response.
type TorAnswer struct {
	IP              string   `json:"ip"`
	IsTor           bool     `json:"is_tor"`
	NodeType        string   `json:"node_type,omitempty"`
	Flags           []string `json:"flags,omitempty"`
	Nickname        string   `json:"nickname,omitempty"`
	Fingerprint     string   `json:"fingerprint,omitempty"`
	Country         string   `json:"country,omitempty"`
	Latitude        *float64 `json:"latitude,omitempty"`
	Longitude       *float64 `json:"longitude,omitempty"`
	AS              string   `json:"as,omitempty"`
	ASName          string   `json:"as_name,omitempty"`
	FirstSeen       string   `json:"first_seen,omitempty"`
	LastSeen        string   `json:"last_seen,omitempty"`
	ConsensusWeight *uint64  `json:"consensus_weight,omitempty"`
	IsGuard         bool     `json:"is_guard,omitempty"`
	IsExit          bool     `json:"is_exit,omitempty"`
	IsMiddle        bool     `json:"is_middle,omitempty"`
	IsAuthority     bool     `json:"is_authority,omitempty"`
	IsHSDir         bool     `json:"is_hsdir,omitempty"`
}

// TorResponse is the JSON body returned by the API.
type TorResponse struct {
	Status  string      `json:"status"`
	Answers []TorAnswer `json:"answers"`
}

// TorRequest is the JSON body expected by the API.
type TorRequest struct {
	IP  *string  `json:"ip"`
	IPs []string `json:"ips"`
}

/* ---------- Configuration ---------- */

var (
	maxIPs     int
	dbDir      string
	dbURL      string // base URL of a peer instance; empty = build from CDN CSV
	licenseKey string // LICENSE_KEY token for the CDN; may be empty (anonymous)
	listenAddr string
	dbValue    atomic.Value // (*maxminddb.Reader)
)

const (
	lastUpdateFile   = ".last_update_tor"
	lastModifiedFile = ".last_modified_tor" // stores the Last-Modified header from the CDN
	dbFileName       = "tor.mmdb"
	cdnCSVURL        = "https://cdn.letstool.net/tor/csv"
	updateInterval   = 4 * time.Hour // fixed refresh interval for both CDN and peer modes
)

/* ---------- Helpers ---------- */

// writeTimestamp persists the current Unix time to the .last_update_tor marker file.
func writeTimestamp() {
	p := filepath.Join(dbDir, lastUpdateFile)
	if err := os.WriteFile(p, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0644); err != nil {
		log.Printf("Warning: could not write %s: %v", lastUpdateFile, err)
	}
}

// readAge returns how long ago the database was last built/downloaded.
// Returns max duration if the marker file is missing or unreadable.
func readAge() time.Duration {
	data, err := os.ReadFile(filepath.Join(dbDir, lastUpdateFile))
	if err != nil {
		return 1<<63 - 1
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 1<<63 - 1
	}
	return time.Since(time.Unix(ts, 0))
}

// readLastModified returns the stored Last-Modified header value (HTTP time format).
// Returns "" if the file does not exist or cannot be read.
func readLastModified() string {
	data, err := os.ReadFile(filepath.Join(dbDir, lastModifiedFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeLastModified persists a Last-Modified header value for future If-Modified-Since use.
func writeLastModified(value string) {
	if value == "" {
		return
	}
	p := filepath.Join(dbDir, lastModifiedFile)
	if err := os.WriteFile(p, []byte(value), 0644); err != nil {
		log.Printf("Warning: could not write %s: %v", lastModifiedFile, err)
	}
}

// swapDB atomically replaces the in-memory reader and closes the old one.
func swapDB(newDB *maxminddb.Reader) {
	old := dbValue.Swap(newDB)
	if old != nil {
		if r, ok := old.(*maxminddb.Reader); ok {
			r.Close()
		}
	}
}

// installFile moves src to dst, falling back to a copy on cross-device rename.
func installFile(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Rename(src, dst); err != nil {
		return copyFile(src, dst)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

/* ---------- HTTP client with proxy support ---------- */

// newHTTPClient returns an *http.Client with the given timeout and an explicit
// transport that honours the standard proxy environment variables:
//
//   - HTTPS_PROXY / https_proxy — proxy for HTTPS requests (CDN, peer)
//   - HTTP_PROXY  / http_proxy  — proxy for plain-HTTP requests
//   - NO_PROXY    / no_proxy    — comma-separated list of hosts to bypass
//
// These are identical to the variables used by curl and most Unix tools.
// If none are set the client connects directly.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// logProxyConfig logs the effective proxy URL for the given target URL, if any.
// Called once at startup to make the proxy configuration visible in the logs.
func logProxyConfig(targetURL string) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return
	}
	req := &http.Request{URL: u}
	proxyURL, err := http.ProxyFromEnvironment(req)
	if err != nil || proxyURL == nil {
		log.Printf("Proxy: none (direct connection to %s)", u.Host)
		return
	}
	// Redact password from proxy URL before logging.
	safe := *proxyURL
	if safe.User != nil {
		if _, hasPwd := safe.User.Password(); hasPwd {
			safe.User = url.UserPassword(safe.User.Username(), "***")
		}
	}
	log.Printf("Proxy: %s (for %s)", safe.String(), u.Host)
}



func lookupTorIP(db *maxminddb.Reader, ipStr string) (*TorAnswer, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP: %s", ipStr)
	}
	var rec TorRecord
	if err := db.Lookup(ip, &rec); err != nil {
		return nil, err
	}
	if !rec.IsTor {
		return &TorAnswer{IP: ipStr, IsTor: false}, nil
	}
	ans := &TorAnswer{
		IP:          ipStr,
		IsTor:       true,
		NodeType:    rec.NodeType,
		Flags:       rec.Flags,
		Nickname:    rec.Nickname,
		Fingerprint: rec.Fingerprint,
		Country:     strings.ToUpper(rec.Country),
		AS:          rec.AS,
		ASName:      rec.ASName,
		FirstSeen:   rec.FirstSeen,
		LastSeen:    rec.LastSeen,
		IsGuard:     rec.IsGuard,
		IsExit:      rec.IsExit,
		IsMiddle:    rec.IsMiddle,
		IsAuthority: rec.IsAuthority,
		IsHSDir:     rec.IsHSDir,
	}
	if rec.Latitude != 0 || rec.Longitude != 0 {
		lat, lon := rec.Latitude, rec.Longitude
		ans.Latitude, ans.Longitude = &lat, &lon
	}
	if rec.ConsensusWeight > 0 {
		w := rec.ConsensusWeight
		ans.ConsensusWeight = &w
	}
	return ans, nil
}

/* ---------- HTTP Handlers ---------- */

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func faviconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Write(faviconPNG)
}

func openapiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}

func torHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
			return
		}
		var req TorRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondTor(w, "ERROR", nil)
			return
		}
		defer r.Body.Close()

		if len(req.IPs) > maxIPs {
			respondTor(w, "ERROR", nil)
			return
		}

		dbVal := dbValue.Load()
		if dbVal == nil {
			respondTor(w, "ERROR", nil)
			return
		}
		db := dbVal.(*maxminddb.Reader)

		var (
			answers  []TorAnswer
			torCount int
		)

		switch {
		case req.IP != nil && len(req.IPs) == 0:
			ans, err := lookupTorIP(db, *req.IP)
			if err != nil {
				log.Printf("DB lookup error: %v", err)
				respondTor(w, "ERROR", nil)
				return
			}
			if ans != nil {
				answers = append(answers, *ans)
				if ans.IsTor {
					torCount++
				}
			}
		case len(req.IPs) > 0 && req.IP == nil:
			for _, ipStr := range req.IPs {
				ans, err := lookupTorIP(db, ipStr)
				if err != nil {
					log.Printf("DB lookup error for %s: %v", ipStr, err)
					answers = append(answers, TorAnswer{IP: ipStr, IsTor: false})
					continue
				}
				if ans != nil {
					answers = append(answers, *ans)
					if ans.IsTor {
						torCount++
					}
				}
			}
		default:
			respondTor(w, "ERROR", nil)
			return
		}

		if len(answers) == 0 {
			respondTor(w, "NOTFOUND", nil)
			return
		}
		if torCount == 0 {
			respondTor(w, "NOTFOUND", answers)
			return
		}
		respondTor(w, "SUCCESS", answers)
	}
}

func respondTor(w http.ResponseWriter, status string, answers []TorAnswer) {
	w.Header().Set("Content-Type", "application/json")
	resp := TorResponse{Status: status, Answers: answers}
	json.NewEncoder(w).Encode(resp)
}

// getDBHandler serves the current tor.mmdb file so peer instances can sync.
func getDBHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mmdbPath := filepath.Join(dbDir, dbFileName)
		if _, err := os.Stat(mmdbPath); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, mmdbPath)
	}
}

/* ---------- DB — peer download mode ---------- */

// downloadFromPeer fetches tor.mmdb from the /db/tor endpoint of the configured
// peer instance (TOR_DB_URL), atomically swaps it into memory, and persists it.
func downloadFromPeer(ctx context.Context) error {
	u, err := url.Parse(dbURL)
	if err != nil {
		return fmt.Errorf("invalid TOR_DB_URL %q: %w", dbURL, err)
	}
	u.Path = "/db/tor"
	peerURL := u.String()
	log.Printf("Downloading tor.mmdb from peer: %s", peerURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	client := newHTTPClient(120 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("peer GET %s: %w", peerURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %s", resp.Status)
	}

	tmpFile, err := os.CreateTemp(dbDir, "tor-peer-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write peer mmdb: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	newDB, err := maxminddb.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open peer mmdb: %w", err)
	}

	swapDB(newDB)

	finalPath := filepath.Join(dbDir, dbFileName)
	if err := installFile(tmpName, finalPath); err != nil {
		log.Printf("Warning: could not persist peer mmdb: %v", err)
	}

	writeTimestamp()
	log.Println("Peer mmdb download complete")
	return nil
}

/* ---------- DB — CDN CSV build mode ---------- */

// errNotModified is returned by fetchCSVFromCDN when the CDN responds 304.
// The caller should skip the build and consider the current DB still valid.
var errNotModified = errors.New("CSV not modified (304)")

// errRateLimited is returned by fetchCSVFromCDN when the CDN responds 429.
// RetryAfter holds the unix timestamp after which the client is allowed to retry.
type errRateLimited struct {
	RetryAfter int64
}

func (e *errRateLimited) Error() string {
	return fmt.Sprintf("CDN rate-limited (429) — retry after unix timestamp %d (%s)",
		e.RetryAfter,
		time.Unix(e.RetryAfter, 0).UTC().Format(time.RFC3339))
}

// fetchCSVFromCDN fetches the gzipped CSV from the CDN and returns an
// io.ReadCloser positioned at the start of the decompressed CSV stream.
// The caller is responsible for closing the returned reader.
//
// Returns errNotModified if the CDN responds 304 (If-Modified-Since matched).
// Returns a descriptive error on 429 (rate-limited), including the unix
// timestamp after which the client may retry.
func fetchCSVFromCDN(ctx context.Context) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnCSVURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create CDN request: %w", err)
	}

	req.Header.Set("User-Agent", "http2tor/1.0 (+https://github.com/letstool/http2tor)")

	// Authorization — sent only when a LICENSE_KEY is configured.
	if licenseKey != "" {
		req.Header.Set("Authorization", "Basic "+licenseKey)
	}

	// If-Modified-Since — lets the CDN return 304 and skip quota consumption.
	if lm := readLastModified(); lm != "" {
		req.Header.Set("If-Modified-Since", lm)
		log.Printf("CDN request with If-Modified-Since: %s", lm)
	}

	client := newHTTPClient(180 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("CDN GET: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusNotModified: // 304
		resp.Body.Close()
		log.Println("CDN: CSV not modified (304) — current DB is up to date")
		return nil, "", errNotModified

	case http.StatusTooManyRequests: // 429
		ra := resp.Header.Get("Retry-After")
		resp.Body.Close()
		ts, _ := strconv.ParseInt(strings.TrimSpace(ra), 10, 64)
		return nil, "", &errRateLimited{RetryAfter: ts}

	case http.StatusOK: // 200 — proceed

	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		return nil, "", fmt.Errorf("CDN returned %s: %s", resp.Status, body)
	}

	// Capture Last-Modified for subsequent If-Modified-Since requests.
	lastModified := resp.Header.Get("Last-Modified")

	// The CDN always serves gzip (Content-Type: application/gzip).
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, "", fmt.Errorf("CDN gzip reader: %w", err)
	}

	// Return a ReadCloser that closes both the gzip reader and the HTTP body.
	return &gzipReadCloser{gz: gz, body: resp.Body}, lastModified, nil
}

// gzipReadCloser wraps a gzip.Reader and its underlying HTTP response body,
// ensuring both are closed when Close is called.
type gzipReadCloser struct {
	gz   *gzip.Reader
	body io.ReadCloser
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g *gzipReadCloser) Close() error {
	err1 := g.gz.Close()
	err2 := g.body.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// parseBool converts the CSV boolean values "true"/"false" to Go bool.
func parseBool(s string) bool { return strings.EqualFold(s, "true") }

// buildTorDBFromCSV fetches the gzipped CSV from the CDN, parses it, compiles
// a fresh tor.mmdb, and atomically swaps it into memory.
//
// CSV columns (0-indexed):
//
//	0:ip  1:node_type  2:flags(|sep)  3:nickname  4:fingerprint  5:country
//	6:latitude  7:longitude  8:as  9:as_name  10:first_seen  11:last_seen
//	12:consensus_weight  13:is_guard  14:is_exit  15:is_middle
//	16:is_authority  17:is_hsdir
func buildTorDBFromCSV(ctx context.Context) error {
	log.Printf("Fetching Tor CSV from CDN: %s", cdnCSVURL)

	csvReader, lastModified, err := fetchCSVFromCDN(ctx)
	if err != nil {
		if err == errNotModified {
			// DB is current — update the local timestamp so the scheduler
			// does not immediately retry on the next tick.
			writeTimestamp()
			return nil
		}
		// errRateLimited and other errors bubble up unwrapped so the
		// scheduler can inspect them with errors.As.
		return fmt.Errorf("CDN fetch: %w", err)
	}
	defer csvReader.Close()

	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "http2tor-TorDB",
		Description:  map[string]string{"en": "Tor Network Node Database built by http2tor"},
		RecordSize:   28,
		IPVersion:    6,
	})
	if err != nil {
		return fmt.Errorf("create mmdb writer: %w", err)
	}

	r := csv.NewReader(csvReader)
	r.ReuseRecord = true     // performance: reuse the backing slice
	r.FieldsPerRecord = 18   // enforce strict column count

	// Skip header row.
	if _, err := r.Read(); err != nil {
		return fmt.Errorf("read CSV header: %w", err)
	}

	inserted := 0
	lineNum := 1

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Warning: CSV parse error at line %d: %v — skipping", lineNum+1, err)
			lineNum++
			continue
		}
		lineNum++

		ipStr    := record[0]
		nodeTyp  := record[1]
		flagsRaw := record[2]
		nickname := record[3]
		fp       := record[4]
		country  := record[5]
		latStr   := record[6]
		lonStr   := record[7]
		asNum    := record[8]
		asName   := record[9]
		firstSeen := record[10]
		lastSeen  := record[11]
		cwStr     := record[12]
		isGuard   := parseBool(record[13])
		isExit    := parseBool(record[14])
		isMiddle  := parseBool(record[15])
		isAuth    := parseBool(record[16])
		isHSDir   := parseBool(record[17])

		ip := net.ParseIP(ipStr)
		if ip == nil {
			log.Printf("Warning: invalid IP %q at line %d — skipping", ipStr, lineNum)
			continue
		}

		lat, _ := strconv.ParseFloat(latStr, 64)
		lon, _ := strconv.ParseFloat(lonStr, 64)
		cw, _  := strconv.ParseUint(cwStr, 10, 64)

		// flags field uses "|" as separator within the CSV field.
		var flags []string
		if flagsRaw != "" {
			flags = strings.Split(flagsRaw, "|")
		}
		flagSlice := make(mmdbtype.Slice, len(flags))
		for i, f := range flags {
			flagSlice[i] = mmdbtype.String(f)
		}

		record2 := mmdbtype.Map{
			"is_tor":           mmdbtype.Bool(true),
			"node_type":        mmdbtype.String(nodeTyp),
			"flags":            flagSlice,
			"nickname":         mmdbtype.String(nickname),
			"fingerprint":      mmdbtype.String(fp),
			"country":          mmdbtype.String(country),
			"latitude":         mmdbtype.Float64(lat),
			"longitude":        mmdbtype.Float64(lon),
			"as":               mmdbtype.String(asNum),
			"as_name":          mmdbtype.String(asName),
			"first_seen":       mmdbtype.String(firstSeen),
			"last_seen":        mmdbtype.String(lastSeen),
			"consensus_weight": mmdbtype.Uint64(cw),
			"is_guard":         mmdbtype.Bool(isGuard),
			"is_exit":          mmdbtype.Bool(isExit),
			"is_middle":        mmdbtype.Bool(isMiddle),
			"is_authority":     mmdbtype.Bool(isAuth),
			"is_hsdir":         mmdbtype.Bool(isHSDir),
		}

		// Build a host-route network (/32 for IPv4, /128 for IPv6).
		// writer.Insert is the correct method name in mmdbwriter v1.0.0.
		bits := 128
		if ip.To4() != nil {
			bits = 32
			ip = ip.To4()
		}
		network := &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}

		if err := writer.Insert(network, record2); err != nil {
			log.Printf("Warning: failed to insert %s: %v", ipStr, err)
			continue
		}
		inserted++
	}
	log.Printf("Parsed %d IP records from CDN CSV", inserted)

	// Write mmdb to a temp file, then atomically swap.
	tmpFile, err := os.CreateTemp(dbDir, "tor-build-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := writer.WriteTo(tmpFile); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write mmdb: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	newDB, err := maxminddb.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open new mmdb: %w", err)
	}

	swapDB(newDB)

	finalPath := filepath.Join(dbDir, dbFileName)
	if err := installFile(tmpName, finalPath); err != nil {
		return fmt.Errorf("install mmdb: %w", err)
	}

	writeTimestamp()
	writeLastModified(lastModified)
	log.Printf("Tor DB built from CDN CSV: %d IPs inserted", inserted)
	return nil
}

/* ---------- DB — dispatch ---------- */

// updateDB calls the right update strategy depending on whether TOR_DB_URL is set.
//
//   - TOR_DB_URL set   → downloadFromPeer: fetch tor.mmdb from the peer's /db/tor endpoint.
//   - TOR_DB_URL unset → buildTorDBFromCSV: fetch gzipped CSV from the CDN and compile mmdb.
func updateDB(ctx context.Context) error {
	if dbURL != "" {
		return downloadFromPeer(ctx)
	}
	return buildTorDBFromCSV(ctx)
}

// ensureDB loads the cached database if it is still within the refresh interval;
// otherwise it calls updateDB to fetch or build a fresh copy.
func ensureDB(ctx context.Context) error {
	mmdbPath := filepath.Join(dbDir, dbFileName)
	if _, err := os.Stat(mmdbPath); err == nil {
		age := readAge()
		if age < updateInterval {
			db, err := maxminddb.Open(mmdbPath)
			if err != nil {
				return fmt.Errorf("open existing database: %w", err)
			}
			dbValue.Store(db)
			log.Printf("Loaded existing Tor DB (built %s ago, max age %s)",
				age.Round(time.Minute), updateInterval)
			return nil
		}
		log.Printf("Tor DB is %s old (max %s), updating...",
			age.Round(time.Minute), updateInterval)
	}
	return updateDB(ctx)
}

/* ---------- Scheduler ---------- */

// schedulePeriodicUpdate fires updateDB every updateInterval (4 h).
// If updateDB returns an *errRateLimited (CDN 429), the next attempt is
// deferred until the Retry-After timestamp instead of the normal interval.
func schedulePeriodicUpdate(ctx context.Context) {
	mode := "CDN CSV build"
	if dbURL != "" {
		mode = "peer download (" + dbURL + ")"
	}
	log.Printf("Tor DB auto-refresh every %s [mode: %s]", updateInterval, mode)

	go func() {
		timer := time.NewTimer(updateInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				err := updateDB(ctx)
				if err != nil {
					var rl *errRateLimited
					if errors.As(err, &rl) && rl.RetryAfter > 0 {
						// Wait until the CDN-specified Retry-After timestamp.
						wait := time.Until(time.Unix(rl.RetryAfter, 0))
						if wait <= 0 {
							wait = updateInterval
						}
						log.Printf("Rate-limited by CDN: next attempt in %s (at %s)",
							wait.Round(time.Second),
							time.Unix(rl.RetryAfter, 0).UTC().Format(time.RFC3339))
						timer.Reset(wait)
					} else {
						log.Printf("Scheduled update failed: %v — retrying in %s", err, updateInterval)
						timer.Reset(updateInterval)
					}
				} else {
					timer.Reset(updateInterval)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

/* ---------- Main ---------- */

func main() {
	const sentinel = "\x00"
	flagDBURL      := flag.String("db-url",      sentinel, "Base URL of a peer http2tor instance (e.g. http://host:8080). Overrides TOR_DB_URL. If unset, fetches from CDN.")
	flagDBDir      := flag.String("db-dir",      sentinel, "Directory for the mmdb file. Overrides TOR_DB_DIR. Default: /data")
	flagListenAddr := flag.String("listen-addr", sentinel, "Listen address. Overrides LISTEN_ADDR. Default: 127.0.0.1:8080")
	flagLicenseKey := flag.String("license-key", sentinel, "CDN license key (Basic auth token). Overrides LICENSE_KEY. Optional.")
	flagMaxIPs     := flag.Int("max-ips",         -1,       "Max IPs per request. Overrides TOR_MAX_IPS. Default: 100")
	flag.Parse()

	resolve := func(flagVal, envKey, defaultVal string) string {
		if flagVal != sentinel {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		return defaultVal
	}

	dbURL      = resolve(*flagDBURL,      "TOR_DB_URL",  "")
	dbDir      = resolve(*flagDBDir,      "TOR_DB_DIR",  "/data")
	listenAddr = resolve(*flagListenAddr, "LISTEN_ADDR", "127.0.0.1:8080")
	licenseKey = resolve(*flagLicenseKey, "LICENSE_KEY", "")

	maxIPs = 100
	if *flagMaxIPs >= 0 {
		maxIPs = *flagMaxIPs
	} else if v := os.Getenv("TOR_MAX_IPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxIPs = n
		}
	}

	switch {
	case dbURL != "":
		log.Printf("Mode: peer sync from %s (interval: %s)", dbURL, updateInterval)
		logProxyConfig(dbURL)
	case licenseKey != "":
		log.Printf("Mode: CDN CSV build — licensed (interval: %s)", updateInterval)
		logProxyConfig(cdnCSVURL)
	default:
		log.Printf("Mode: CDN CSV build — anonymous (interval: %s)", updateInterval)
		logProxyConfig(cdnCSVURL)
	}

	if err := os.MkdirAll(dbDir, 0755); err != nil {
		log.Fatalf("failed to create directory %s: %v", dbDir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ensureDB(ctx); err != nil {
		log.Fatalf("failed to initialize Tor database: %v", err)
	}

	schedulePeriodicUpdate(ctx)

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/favicon.png", faviconHandler)
	http.HandleFunc("/openapi.json", openapiHandler)
	http.HandleFunc("/api/v1/istor", torHandler())
	http.HandleFunc("/db/tor", getDBHandler())

	srv := &http.Server{
		Addr:         listenAddr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Printf("http2tor server listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
