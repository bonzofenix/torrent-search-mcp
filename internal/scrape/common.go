// Package scrape ports torrent_search/wrapper/parser.py and scraper.py: one
// parser per source, all normalized to the same ';'-separated CSV text, and
// the parallel search/popular pipelines on top of them.
package scrape

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bonzofenix/torrent-search-mcp/internal/torrent"
)

// UA is the browser User-Agent sent to every source.
const UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// Trackers are the public trackers appended to magnets built from a bare info hash.
var Trackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.dler.org:6969/announce",
	"http://tracker.opentrackr.org:1337/announce",
	"http://tracker.openbittorrent.com:80/announce",
	"http://tracker.dler.org:6969/announce",
	"https://tracker.tamersunion.org:443/announce",
}

const trackersBestURL = "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best.txt"

// CSVHeader is the column contract every parser emits.
const CSVHeader = "filename;category;size;seeders;leechers;downloads;date;magnet_link;page_url"

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// client bounds each phase at 20s like httpx.Timeout(20) rather than the
// whole exchange: slow mirrors that keep streaming still finish, and every
// fetch is capped overall by the per-source context (SourceTimeout).
var client = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   20 * time.Second,
	ResponseHeaderTimeout: 20 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   8,
	ForceAttemptHTTP2:     true,
}}

// goSafe runs fn on a WaitGroup goroutine, recovering panics: a panic in a
// bare goroutine would take the whole long-lived MCP process down.
func goSafe(wg *sync.WaitGroup, what string, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("WARNING: %s panicked: %v", what, r)
			}
		}()
		fn()
	}()
}

// HTTPStatusError is returned for non-2xx answers (httpx raise_for_status).
type HTTPStatusError struct {
	StatusCode int
	URL        string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d for url '%s'", e.StatusCode, e.URL)
}

func fetchText(ctx context.Context, rawURL string, params url.Values) (string, error) {
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UA)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &HTTPStatusError{StatusCode: resp.StatusCode, URL: rawURL}
	}
	return string(body), nil
}

// getText fetches a URL; tests swap it to serve fixtures.
var getText = fetchText

// decodeJSON decodes with json.Number so numeric fields keep their text form.
func decodeJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func getJSON(ctx context.Context, rawURL string, params url.Values) (any, error) {
	text, err := getText(ctx, rawURL, params)
	if err != nil {
		return nil, err
	}
	return decodeJSON(text)
}

var (
	lastGoodMu   sync.Mutex
	lastGoodHost = map[string]string{}
)

// firstHost fetches a path through a mirror rotation; returns the working
// (host, text).
//
// The last host that answered is tried first next time: mirrors that 403 or
// redirect cost several wasted round trips per fetch otherwise.
func firstHost(ctx context.Context, hosts []string, path string, params url.Values) (string, string, error) {
	key := strings.Join(hosts, "\x00")
	lastGoodMu.Lock()
	preferred := lastGoodHost[key]
	lastGoodMu.Unlock()
	ordered := hosts
	if preferred != "" {
		ordered = []string{preferred}
		for _, h := range hosts {
			if h != preferred {
				ordered = append(ordered, h)
			}
		}
	}
	var lastErr error
	for _, host := range ordered {
		text, err := getText(ctx, "https://"+host+path, params)
		if err != nil {
			lastErr = err
			continue
		}
		lastGoodMu.Lock()
		lastGoodHost[key] = host
		lastGoodMu.Unlock()
		return host, text, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no hosts to try for %s", path)
	}
	return "", "", lastErr
}

// getFirst fetches a path through a mirror rotation, returning the first answer.
var getFirst = func(ctx context.Context, hosts []string, path string, params url.Values) (string, error) {
	_, text, err := firstHost(ctx, hosts, path, params)
	return text, err
}

// ---------------------------------------------------------------------------
// Trackers
// ---------------------------------------------------------------------------

var (
	trackersMu     sync.RWMutex
	trackers       = append([]string(nil), Trackers...)
	trackersLoaded sync.Once
)

// mergeTrackers merges the remote tracker list into the base one, deduplicated.
func mergeTrackers(remoteText string) []string {
	seen := map[string]bool{}
	var merged []string
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			merged = append(merged, t)
		}
	}
	for _, t := range Trackers {
		add(t)
	}
	for _, line := range splitLines(remoteText) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			add(line)
		}
	}
	return merged
}

// EnsureTrackers enriches Trackers with the ngosang trackers list, once per
// process. Falls back to the static Trackers on failure. Refresh via restart.
func EnsureTrackers(ctx context.Context) {
	trackersLoaded.Do(func() {
		// Detached from the caller: this runs once and must not be cut short
		// by whichever request happened to trigger it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		text, err := getText(ctx, trackersBestURL, nil)
		if err != nil {
			return
		}
		merged := mergeTrackers(text)
		trackersMu.Lock()
		trackers = merged
		trackersMu.Unlock()
	})
}

// pyQuote is urllib.parse.quote(s, safe=""): everything but unreserved
// characters is percent-encoded.
func pyQuote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// BuildMagnet builds a magnet URI from an info hash and display name.
func BuildMagnet(infoHash, name string) string {
	trackersMu.RLock()
	defer trackersMu.RUnlock()
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:" + infoHash + "&dn=" + pyQuote(name))
	for _, t := range trackers {
		b.WriteString("&tr=" + pyQuote(t))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Python value semantics for loosely-typed JSON fields
// ---------------------------------------------------------------------------

// truthy is Python's bool(value) for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr is Python's str(value) for decoded JSON scalars.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// or returns v when truthy, else fallback (Python's `v or fallback`).
func or(v any, fallback any) any {
	if truthy(v) {
		return v
	}
	return fallback
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// HumanSize formats a byte count as a human-readable string.
func HumanSize(numBytes any) string {
	var value float64
	switch x := or(numBytes, 0).(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return "N/A"
		}
		value = f
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return "N/A"
		}
		value = f
	case int:
		value = float64(x)
	case int64:
		value = float64(x)
	case float64:
		value = x
	default:
		return "N/A"
	}
	if !(value > 0) {
		return "N/A"
	}
	if value < 1024 {
		return fmt.Sprintf("%d B", int64(value))
	}
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		if value < 1024 || unit == "TiB" {
			return strconv.FormatFloat(value, 'f', 1, 64) + " " + unit
		}
	}
	return "N/A"
}

var (
	dateOnly = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	isoDate  = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2})(?:[.,](\d{1,9}))?)?(Z|[+-]\d{2}:?\d{2})?$`)
)

// isoformat renders t like Python's datetime.isoformat().
func isoformat(t time.Time, aware bool) string {
	out := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		out += fmt.Sprintf(".%06d", us)
	}
	if aware {
		out += t.Format("-07:00")
	}
	return out
}

// parseISO handles the datetime.fromisoformat shapes sources actually send.
func parseISO(value string) (string, bool) {
	m := isoDate.FindStringSubmatch(value)
	if m == nil {
		return "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	year, month, day, hour, minute, sec := n(m[1]), n(m[2]), n(m[3]), n(m[4]), n(m[5]), n(m[6])
	frac := m[7]
	for len(frac) < 9 {
		frac += "0"
	}
	nanos := n(frac[:6]) * 1000
	loc, aware := time.UTC, false
	if tz := m[8]; tz != "" {
		aware = true
		if tz != "Z" {
			digits := strings.ReplaceAll(tz[1:], ":", "")
			offset := n(digits[:2])*3600 + n(digits[2:])*60
			if n(digits[:2]) > 23 || n(digits[2:]) > 59 {
				return "", false
			}
			if tz[0] == '-' {
				offset = -offset
			}
			loc = time.FixedZone("", offset)
		}
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, nanos, loc)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day || t.Hour() != hour || t.Minute() != minute || t.Second() != sec {
		return "", false
	}
	return isoformat(t, aware), true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FmtDate formats a unix timestamp, ISO-8601 or RFC-822 date.
//
// Returns the full UTC datetime when the source includes time information,
// otherwise preserves the YYYY-MM-DD date string for date-only sources.
func FmtDate(value any) string {
	var ts int64
	switch x := value.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			ts = i
		} else if f, err := x.Float64(); err == nil && !math.IsInf(f, 0) {
			ts = int64(f)
		} else {
			return "N/A"
		}
	case int:
		ts = int64(x)
	case int64:
		ts = x
	case float64:
		ts = int64(x)
	case string:
		if !isDigits(x) {
			return fmtDateString(x)
		}
		i, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return "N/A"
		}
		ts = i
	default:
		return "N/A"
	}
	if ts == 0 {
		return "N/A"
	}
	return isoformat(time.Unix(ts, 0).UTC(), true)
}

func fmtDateString(value string) string {
	if value == "" {
		return "N/A"
	}
	// Date-only strings come from sources that do not provide a time of day
	// (e.g. 1337x detail pages). Keep them as-is so the UI does not invent one.
	if dateOnly.MatchString(value) {
		return value
	}
	if out, ok := parseISO(value); ok {
		return out
	}
	// RSS feeds send RFC 822 dates; "-0000" offsets are treated as UTC.
	if t, err := mail.ParseDate(value); err == nil {
		return isoformat(t, true)
	}
	return "N/A"
}

// row builds one CSV row (_row).
func row(filename, category, size string, seeders, leechers, downloads, date any, magnet, pageURL string) []string {
	dl := "N/A"
	if truthy(downloads) {
		dl = pyStr(downloads)
	}
	return []string{
		strings.TrimSpace(strings.ReplaceAll(filename, ";", ",")),
		category,
		size,
		pyStr(or(seeders, 0)),
		pyStr(or(leechers, 0)),
		dl,
		FmtDate(date),
		magnet,
		pageURL,
	}
}

// formatRows serializes rows to the CSV text ExtractTorrents expects.
func formatRows(rows [][]string) string {
	if len(rows) == 0 {
		return "No results"
	}
	lines := []string{CSVHeader}
	for _, r := range rows {
		lines = append(lines, strings.Join(r, ";"))
	}
	return strings.Join(lines, "\n")
}

var rssFieldCache sync.Map

// rssField extracts a tag's text from a raw RSS item (handles CDATA and attributes).
func rssField(item, name string) string {
	re, ok := rssFieldCache.Load(name)
	if !ok {
		quoted := regexp.QuoteMeta(name)
		re, _ = rssFieldCache.LoadOrStore(name, regexp.MustCompile(
			`(?is)<`+quoted+`(?:\s+[^>]*)?>(?:<!\[CDATA\[)?(.*?)(?:\]\]>)?</`+quoted+`>`))
	}
	m := re.(*regexp.Regexp).FindStringSubmatch(item)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// splitLines is Python's str.splitlines().
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			if r == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func tokens(query string) []string {
	return strings.Fields(strings.ToLower(strings.TrimSpace(query)))
}

// matchesAll reports whether every token appears in name (case-insensitive).
func matchesAll(name string, toks []string) bool {
	lower := strings.ToLower(name)
	for _, t := range toks {
		if !strings.Contains(lower, t) {
			return false
		}
	}
	return true
}

// ExtractTorrents parses "SOURCE -> name\n<csv>" texts into torrents; rows
// that fail validation are skipped.
func ExtractTorrents(texts []string) []torrent.Torrent {
	var out []torrent.Torrent
	for _, text := range texts {
		source, content, ok := strings.Cut(text, "\n")
		if !ok || strings.Contains(content, "No results") {
			continue
		}
		source = strings.TrimPrefix(source, "SOURCE -> ")
		data := splitLines(content)
		if len(data) == 0 {
			continue
		}
		headers := strings.Split(data[0], ";")
		for _, line := range data[1:] {
			values := strings.Split(line, ";")
			if len(values) > len(headers) {
				extra := len(values) - len(headers)
				if allBlank(values[len(headers):]) {
					values = values[:len(headers)]
				} else if len(values) > 1 {
					// Extra values mid-row (e.g. a ';' inside a filename):
					// the extra fields right after the filename are joined
					// back into it; the tail beyond the header length is
					// dropped below.
					values[1] = strings.Join(values[1:1+extra], " - ")
					values = append(values[:2], values[1+extra:]...)
				}
			}
			fields := map[string]string{}
			for i, h := range headers {
				if i < len(values) {
					fields[h] = values[i]
				}
			}
			if t, err := torrent.Format(fields, source); err == nil {
				out = append(out, t)
			}
		}
	}
	return out
}

func allBlank(values []string) bool {
	for _, v := range values {
		if strings.TrimFunc(v, unicode.IsSpace) != "" {
			return false
		}
	}
	return true
}

// orderedObject decodes a top-level JSON object keeping key order (Python
// dicts preserve it, and result order follows it). Non-objects yield nil.
func orderedObject(text string) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, nil
	}
	var values []any
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}
