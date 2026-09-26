package scrape

import (
	"context"
	"net/url"
	"strings"
)

// yts.mx - JSON API.

var ytsHosts = []string{"yts.mx", "yts.am", "yts.rs"}

func ytsRows(data map[string]any) [][]string {
	var rows [][]string
	for _, rawMovie := range asList(asMap(data["data"])["movies"]) {
		movie := asMap(rawMovie)
		if movie == nil {
			continue
		}
		title := pyStr(or(or(movie["title_long"], movie["title"]), "Unknown"))
		for _, rawTorrent := range asList(movie["torrents"]) {
			t := asMap(rawTorrent)
			infoHash := asString(t["hash"])
			if infoHash == "" {
				continue
			}
			var tags []string
			for _, key := range []string{"quality", "type"} {
				if truthy(t[key]) {
					tags = append(tags, pyStr(t[key]))
				}
			}
			filename := title
			if len(tags) > 0 {
				filename = title + " [" + strings.Join(tags, " ") + "]"
			}
			rows = append(rows, row(filename, "Video - Movies", HumanSize(t["size_bytes"]), t["seeds"], t["peers"],
				nil, movie["date_uploaded_unix"], BuildMagnet(strings.ToLower(infoHash), filename), ""))
		}
	}
	return rows
}

func ytsParse(ctx context.Context, query, sortBy string) (string, error) {
	params := url.Values{"limit": {"50"}}
	if q := strings.TrimSpace(query); q != "" {
		params.Set("query_term", q)
	} else {
		if sortBy == "" {
			sortBy = "date_added"
		}
		params.Set("sort_by", sortBy)
	}
	text, err := getFirst(ctx, ytsHosts, "/api/v2/list_movies.json", params)
	if err != nil {
		return "", err
	}
	data, err := decodeJSON(text)
	if err != nil {
		return "", err
	}
	return formatRows(ytsRows(asMap(data))), nil
}
