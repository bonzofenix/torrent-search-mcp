package scrape

import (
	"context"
	"html"
	"regexp"
	"strings"
)

// fitgirl-repacks.site - WordPress RSS feed.

var hrefMagnetI = regexp.MustCompile(`(?i)href="(magnet:\?xt=urn:btih:[^"]+)"`)

func fitgirlRows(xml string) [][]string {
	var rows [][]string
	for _, item := range strings.Split(xml, "<item>")[1:] {
		m := hrefMagnetI.FindStringSubmatch(item)
		if m == nil {
			continue
		}
		name := html.UnescapeString(rssField(item, "title"))
		if name == "" {
			name = "Unknown"
		}
		rows = append(rows, row(name, "Games", "N/A", 0, 0, nil, rssField(item, "pubDate"),
			html.UnescapeString(m[1]), rssField(item, "link")))
	}
	return rows
}

func fitgirlParse(ctx context.Context, query string) (string, error) {
	base := "https://fitgirl-repacks.site"
	u := base + "/feed/"
	if q := strings.TrimSpace(query); q != "" {
		u = base + "/?s=" + pyQuote(q) + "&feed=rss2"
	}
	text, err := getText(ctx, u, nil)
	if err != nil {
		return "", err
	}
	return formatRows(fitgirlRows(text)), nil
}
