package scrape

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

// subsplease.org - JSON API.

var resPreference = []string{"1080", "720", "480"}

var xlParam = regexp.MustCompile(`[?&]xl=(\d+)`)

func pickDownload(downloads []any) map[string]any {
	for _, res := range resPreference {
		for _, raw := range downloads {
			if d := asMap(raw); d != nil && d["res"] == res && truthy(d["magnet"]) {
				return d
			}
		}
	}
	for _, raw := range downloads {
		if d := asMap(raw); d != nil && truthy(d["magnet"]) {
			return d
		}
	}
	return nil
}

func subspleaseRows(entries []any) [][]string {
	var rows [][]string
	for _, raw := range entries {
		entry := asMap(raw)
		if entry == nil {
			continue
		}
		download := pickDownload(asList(entry["downloads"]))
		if download == nil {
			continue
		}
		magnet := pyStr(download["magnet"])
		show := pyStr(or(entry["show"], "Unknown"))
		episode := ""
		if truthy(entry["episode"]) {
			episode = " - " + pyStr(entry["episode"])
		}
		name := show + episode + " [" + pyStr(or(download["res"], "?")) + "p]"
		size := "N/A"
		if m := xlParam.FindStringSubmatch(magnet); m != nil {
			size = HumanSize(m[1])
		}
		pageURL := ""
		if page := entry["page"]; truthy(page) {
			pageURL = "https://subsplease.org/shows/" + pyStr(page) + "/"
		}
		rows = append(rows, row(name, "Anime", size, 0, 0, nil, entry["release_date"], magnet, pageURL))
	}
	return rows
}

func subspleaseParse(ctx context.Context, query string) (string, error) {
	params := url.Values{"tz": {"UTC"}, "f": {"latest"}}
	if q := strings.TrimSpace(query); q != "" {
		params.Set("f", "search")
		params.Set("s", q)
	}
	text, err := getText(ctx, "https://subsplease.org/api/", params)
	if err != nil {
		return "", err
	}
	entries, err := orderedObject(text)
	if err != nil {
		return "", err
	}
	return formatRows(subspleaseRows(entries)), nil
}
