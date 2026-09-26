// Package torrent ports torrent_search/wrapper/models.py: the Torrent record,
// its id scheme and the magnet lookup cache.
package torrent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var urlsafeIDPart = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// idPart encodes one torrent id component as a single URL path segment.
//
// Hex hashes and scraping keys pass through unchanged; values with spaces,
// slashes or non-ASCII characters are base64url-encoded, so an id can never
// break out of /torrent/{id} routing with a raw "/".
func idPart(text string) string {
	if urlsafeIDPart.MatchString(text) {
		return text
	}
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

// Torrent mirrors the Python pydantic model. Optional fields are nil when
// unset, which is what keeps them out of String().
type Torrent struct {
	ID         string
	Filename   string
	Category   *string
	Size       string
	Seeders    int
	Leechers   int
	Downloads  any // nil, string or int64 (Python: int | str | None)
	Date       string
	MagnetLink *string
	PageURL    *string
	Uploader   *string
	Source     *string
}

// Ptr returns a pointer to s.
func Ptr(s string) *string { return &s }

// Deref returns *s, or "" when s is nil.
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// validPageURL keeps only absolute HTTP(S) URLs.
func validPageURL(value string) *string {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil
	}
	return &value
}

func parseInt(value string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(value))
}

// Format builds a Torrent from one parsed CSV row (Torrent.format).
func Format(data map[string]string, source string) (Torrent, error) {
	filename, ok := data["filename"]
	if !ok {
		return Torrent{}, errors.New("missing filename")
	}
	size, ok := data["size"]
	if !ok {
		return Torrent{}, errors.New("missing size")
	}
	date, ok := data["date"]
	if !ok {
		return Torrent{}, errors.New("missing date")
	}
	ref := data["id"]
	if ref == "" {
		ref = "none"
		if magnet := data["magnet_link"]; magnet != "" {
			sum := sha256.Sum256([]byte(magnet))
			ref = hex.EncodeToString(sum[:])[:10]
		}
	}
	t := Torrent{
		ID:        idPart(source) + "-" + idPart(ref),
		Filename:  strings.TrimSpace(filename),
		Size:      size,
		Date:      date,
		Downloads: "N/A",
		Source:    Ptr(source),
	}
	var err error
	if v := data["seeders"]; v != "" {
		if t.Seeders, err = parseInt(v); err != nil {
			return Torrent{}, err
		}
	}
	if v := data["leechers"]; v != "" {
		if t.Leechers, err = parseInt(v); err != nil {
			return Torrent{}, err
		}
	}
	if v := data["downloads"]; v != "" {
		t.Downloads = v
	}
	if v, ok := data["category"]; ok {
		t.Category = Ptr(v)
	}
	if v, ok := data["magnet_link"]; ok {
		t.MagnetLink = Ptr(v)
	}
	if v, ok := data["uploader"]; ok {
		t.Uploader = Ptr(v)
	}
	t.PageURL = validPageURL(data["page_url"])
	return t, nil
}

// PrependInfo embeds the query and result cap in the id so get_torrent can
// re-run the search once the cache entry has expired.
func (t *Torrent) PrependInfo(query string, maxItems int) {
	t.ID = fmt.Sprintf("%s-%d-%s", Compress(query), maxItems, t.ID)
}

// ExtractInfo decodes an id built by PrependInfo into (query, maxItems,
// source, ref).
func ExtractInfo(torrentID string) (string, int, string, string, error) {
	parts := strings.SplitN(torrentID, "-", 4)
	if len(parts) != 4 {
		return "", 0, "", "", fmt.Errorf("invalid torrent id %q", torrentID)
	}
	maxItems, err := parseInt(parts[1])
	if err != nil {
		return "", 0, "", "", err
	}
	query, err := Decompress(parts[0])
	if err != nil {
		return "", 0, "", "", err
	}
	return query, maxItems, parts[2], parts[3], nil
}

// String reproduces Python's str(model_dump(exclude_unset, exclude_none)):
// a dict repr in field order with unset fields omitted.
func (t Torrent) String() string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	add := func(key, value string) {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(pyRepr(key))
		b.WriteString(": ")
		b.WriteString(value)
	}
	addStr := func(key string, value *string) {
		if value != nil {
			add(key, pyRepr(*value))
		}
	}
	add("id", pyRepr(t.ID))
	add("filename", pyRepr(t.Filename))
	addStr("category", t.Category)
	add("size", pyRepr(t.Size))
	add("seeders", strconv.Itoa(t.Seeders))
	add("leechers", strconv.Itoa(t.Leechers))
	switch v := t.Downloads.(type) {
	case string:
		add("downloads", pyRepr(v))
	case int64:
		add("downloads", strconv.FormatInt(v, 10))
	}
	add("date", pyRepr(t.Date))
	addStr("magnet_link", t.MagnetLink)
	addStr("page_url", t.PageURL)
	addStr("uploader", t.Uploader)
	addStr("source", t.Source)
	b.WriteByte('}')
	return b.String()
}

// flexInt accepts a JSON integer or a numeric string, like pydantic's lax int.
func flexInt(raw json.RawMessage, field string) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("%s is required", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseInt(s)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return n, nil
}

// UnmarshalJSON validates a torrent row from the REST API (Torrent.model_validate).
func (t *Torrent) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID         *string         `json:"id"`
		Filename   *string         `json:"filename"`
		Category   *string         `json:"category"`
		Size       *string         `json:"size"`
		Seeders    json.RawMessage `json:"seeders"`
		Leechers   json.RawMessage `json:"leechers"`
		Downloads  json.RawMessage `json:"downloads"`
		Date       *string         `json:"date"`
		MagnetLink *string         `json:"magnet_link"`
		PageURL    json.RawMessage `json:"page_url"`
		Uploader   *string         `json:"uploader"`
		Source     *string         `json:"source"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for name, value := range map[string]*string{"id": raw.ID, "filename": raw.Filename, "size": raw.Size, "date": raw.Date} {
		if value == nil {
			return fmt.Errorf("%s is required", name)
		}
	}
	out := Torrent{
		ID: *raw.ID, Filename: *raw.Filename, Category: raw.Category, Size: *raw.Size,
		Date: *raw.Date, MagnetLink: raw.MagnetLink, Uploader: raw.Uploader, Source: raw.Source,
	}
	var err error
	if out.Seeders, err = flexInt(raw.Seeders, "seeders"); err != nil {
		return err
	}
	if out.Leechers, err = flexInt(raw.Leechers, "leechers"); err != nil {
		return err
	}
	if len(raw.Downloads) > 0 && string(raw.Downloads) != "null" {
		var s string
		if json.Unmarshal(raw.Downloads, &s) == nil {
			out.Downloads = s
		} else {
			var n int64
			if err := json.Unmarshal(raw.Downloads, &n); err != nil {
				return fmt.Errorf("downloads: %w", err)
			}
			out.Downloads = n
		}
	}
	var pageURL string
	if json.Unmarshal(raw.PageURL, &pageURL) == nil {
		out.PageURL = validPageURL(pageURL)
	}
	*t = out
	return nil
}
