package scrape

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// apibay.org - The Pirate Bay JSON API.

var apibayCategories = map[int]string{
	200: "Video",
	201: "Video - Movies",
	202: "Video - Movies DVDR",
	205: "Video - TV shows",
	206: "Video - Handheld",
	207: "Video - Movies HD",
	208: "Video - Movies HD x265",
	209: "Video - Movies 3D",
}

func apibayRows(items []any) [][]string {
	var rows [][]string
	for _, raw := range items {
		item := asMap(raw)
		if item == nil {
			continue
		}
		infoHash := strings.ToLower(asString(item["info_hash"]))
		if len(infoHash) != 40 || infoHash == strings.Repeat("0", 40) || asString(item["id"]) == "0" {
			continue
		}
		name := pyStr(or(item["name"], "Unknown"))
		category := "Video"
		if id, ok := item["category"].(string); ok && isDigits(id) {
			n, _ := strconv.Atoi(id)
			if c, ok := apibayCategories[n]; ok {
				category = c
			}
		}
		pageURL := ""
		if truthy(item["id"]) {
			pageURL = "https://thepiratebay.org/description.php?id=" + pyStr(item["id"])
		}
		rows = append(rows, row(name, category, HumanSize(item["size"]), item["seeders"], item["leechers"],
			nil, item["added"], BuildMagnet(infoHash, name), pageURL))
	}
	return rows
}

func apibayParse(ctx context.Context, query string) (string, error) {
	q := strings.TrimSpace(query)
	var items []any
	if q != "" {
		data, err := getJSON(ctx, "https://apibay.org/q.php", url.Values{"q": {q}})
		if err != nil {
			return "", err
		}
		items = asList(data)
	} else {
		urls := []string{
			"https://apibay.org/precompiled/data_top100_207.json",
			"https://apibay.org/precompiled/data_top100_208.json",
		}
		pages := make([][]any, len(urls))
		var wg sync.WaitGroup
		for i, u := range urls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if data, err := getJSON(ctx, u, nil); err == nil {
					pages[i] = asList(data)
				}
			}()
		}
		wg.Wait()
		for _, p := range pages {
			items = append(items, p...)
		}
	}
	return formatRows(apibayRows(items)), nil
}
