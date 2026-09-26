package scrape

import (
	"context"
	"net/url"
	"strings"
)

// eztvx.to - JSON API.

func eztvRows(data map[string]any) [][]string {
	var rows [][]string
	for _, raw := range asList(data["torrents"]) {
		t := asMap(raw)
		if t == nil {
			continue
		}
		infoHash := strings.ToLower(asString(t["hash"]))
		if infoHash == "" {
			continue
		}
		name := pyStr(or(or(t["filename"], t["title"]), infoHash))
		magnet := asString(t["magnet_url"])
		if magnet == "" {
			magnet = BuildMagnet(infoHash, name)
		}
		rows = append(rows, row(name, "Video - TV shows", HumanSize(t["size_bytes"]), t["seeds"], t["peers"],
			nil, t["date_released_unix"], magnet, ""))
	}
	return rows
}

func eztvParse(ctx context.Context, query string) (string, error) {
	// The EZTV API has no query search (it ignores `q`), so queries are
	// matched client-side against the latest releases.
	data, err := getJSON(ctx, "https://eztvx.to/api/get-torrents", url.Values{"limit": {"100"}, "page": {"1"}})
	if err != nil {
		return "", err
	}
	var rows [][]string
	toks := tokens(query)
	for _, r := range eztvRows(asMap(data)) {
		if matchesAll(r[0], toks) {
			rows = append(rows, r)
		}
	}
	return formatRows(rows), nil
}
