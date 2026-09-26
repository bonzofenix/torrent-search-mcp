package scrape

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// nyaa.si - RSS feed for search, HTML table for the popular listing.

func nyaaRSSRows(xml string) [][]string {
	var rows [][]string
	for _, item := range strings.Split(xml, "<item>")[1:] {
		infoHash := strings.ToLower(rssField(item, "nyaa:infoHash"))
		name := html.UnescapeString(rssField(item, "title"))
		if infoHash == "" || name == "" {
			continue
		}
		category := rssField(item, "nyaa:category")
		if category == "" {
			category = "Anime"
		}
		size := rssField(item, "nyaa:size")
		if size == "" {
			size = "N/A"
		}
		rows = append(rows, row(name, category, size, rssField(item, "nyaa:seeders"), rssField(item, "nyaa:leechers"),
			rssField(item, "nyaa:downloads"), rssField(item, "pubDate"), BuildMagnet(infoHash, name), rssField(item, "guid")))
	}
	return rows
}

func nyaaParse(ctx context.Context, query string) (string, error) {
	text, err := getText(ctx, "https://nyaa.si/", url.Values{"page": {"rss"}, "q": {query}, "c": {"0_0"}, "f": {"0"}})
	if err != nil {
		return "", err
	}
	return formatRows(nyaaRSSRows(text)), nil
}

const nyaaPopularURL = "https://nyaa.si/"

var nyaaPopularParams = url.Values{"q": {""}, "s": {"seeders"}, "o": {"desc"}}

var (
	nyaaTR     = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	nyaaName   = regexp.MustCompile(`<a href="(/view/\d+)" title="([^"]+)"`)
	hrefMagnet = regexp.MustCompile(`href="(magnet:\?xt=urn:btih:[^"]+)"`)
	nyaaCell   = regexp.MustCompile(`(?s)<td class="text-center"[^>]*>(.*?)</td>`)
	nyaaTS     = regexp.MustCompile(`data-timestamp="(\d+)"`)
	number     = regexp.MustCompile(`([\d,]+)`)
)

// nyaaHTMLRows parses the HTML browse table (the RSS feed ignores sort params).
func nyaaHTMLRows(htmlText string) [][]string {
	var rows [][]string
	for _, trMatch := range nyaaTR.FindAllStringSubmatch(htmlText, -1) {
		tr := trMatch[1]
		name := nyaaName.FindStringSubmatch(tr)
		magnet := hrefMagnet.FindStringSubmatch(tr)
		if name == nil || magnet == nil {
			continue
		}
		var cells []string
		for _, c := range nyaaCell.FindAllStringSubmatch(tr, -1) {
			cells = append(cells, c[1])
		}
		// Cells: [links, size, date, seeders, leechers, downloads]
		if len(cells) < 6 {
			continue
		}
		num := func(cell string, fallback any) any {
			if m := number.FindStringSubmatch(cell); m != nil {
				return strings.ReplaceAll(m[1], ",", "")
			}
			return fallback
		}
		var date any
		if m := nyaaTS.FindStringSubmatch(tr); m != nil {
			ts, _ := strconv.ParseInt(m[1], 10, 64)
			date = ts
		}
		size := strings.TrimSpace(cells[1])
		if size == "" {
			size = "N/A"
		}
		rows = append(rows, row(html.UnescapeString(name[2]), "Anime", size, num(cells[3], 0), num(cells[4], 0),
			num(cells[5], nil), date, html.UnescapeString(magnet[1]), "https://nyaa.si"+name[1]))
	}
	return rows
}

// nyaaPopular lists the most seeded torrents; the HTML page honors sorting,
// the RSS feed does not.
func nyaaPopular(ctx context.Context) (string, error) {
	text, err := getText(ctx, nyaaPopularURL, nyaaPopularParams)
	if err != nil {
		return "", err
	}
	return formatRows(nyaaHTMLRows(text)), nil
}
