package scrape

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 1337x.to - HTML scrape with mirror rotation.

var x1337Hosts = []string{"1337x.to", "1337x.st", "x1337x.ws", "www.1337xx.to", "1337xx.to"}

var x1337StopWords = map[string]bool{"the": true, "a": true, "an": true, "of": true, "and": true, "or": true, "to": true}

var x1337Months = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var (
	x1337Link   = regexp.MustCompile(`(?i)href="(/torrent/[^"]+)"[^>]*>([^<]+)</a>`)
	x1337Size   = regexp.MustCompile(`(?i)class="coll-4 size[^"]*">\s*([\d.]+\s*[KMGT]i?B)`)
	x1337Seeds  = regexp.MustCompile(`(?i)class="coll-2 seeds[^"]*">\s*([\d,]+)`)
	x1337Leech  = regexp.MustCompile(`(?i)class="coll-3 leeches[^"]*">\s*([\d,]+)`)
	x1337Date   = regexp.MustCompile(`(?i)Date uploaded</strong>\s*<span>\s*([A-Za-z]{3})\.?\s+(\d{1,2})[a-z]{2}\s*'(\d{2})`)
	x1337Magnet = regexp.MustCompile(`(?i)magnet:\?xt=urn:btih:[^"'<>\s]+`)
)

// x1337Rows parses the search-result table from a 1337x category page.
// Returns rows of [name, torrent_path, size, seeders, leechers].
func x1337Rows(htmlText string) [][]string {
	start := strings.Index(htmlText, "table-list")
	if start < 0 {
		return nil
	}
	var rows [][]string
	for _, tr := range strings.Split(htmlText[start:], "<tr")[1:] {
		link := x1337Link.FindStringSubmatch(tr)
		if link == nil {
			continue
		}
		size, seeds, leech := "N/A", "0", "0"
		if m := x1337Size.FindStringSubmatch(tr); m != nil {
			size = m[1]
		}
		if m := x1337Seeds.FindStringSubmatch(tr); m != nil {
			seeds = strings.ReplaceAll(m[1], ",", "")
		}
		if m := x1337Leech.FindStringSubmatch(tr); m != nil {
			leech = strings.ReplaceAll(m[1], ",", "")
		}
		rows = append(rows, []string{html.UnescapeString(strings.TrimSpace(link[2])), link[1], size, seeds, leech})
	}
	return rows
}

// x1337UploadDate parses the 'Date uploaded' field of a 1337x detail page.
func x1337UploadDate(htmlText string) string {
	m := x1337Date.FindStringSubmatch(htmlText)
	if m == nil {
		return "N/A"
	}
	month := x1337Months[strings.ToLower(m[1])]
	if month == 0 {
		return "N/A"
	}
	day, _ := strconv.Atoi(m[2])
	year, _ := strconv.Atoi(m[3])
	return fmt.Sprintf("%d-%02d-%02d", 2000+year, month, day)
}

// Detail pages resolve through the mirror rotation, so concurrency is
// bounded to keep the mirror hosts from throttling the whole listing.
var x1337DetailSlots = make(chan struct{}, 8)

// x1337Fetch fetches a 1337x path through the mirror rotation; returns (base, html).
var x1337Fetch = func(ctx context.Context, path string) (string, string, error) {
	host, text, err := firstHost(ctx, x1337Hosts, path, nil)
	if err != nil {
		return "", "", err
	}
	return "https://" + host, text, nil
}

// x1337Detail fetches a torrent page and returns (magnet, date); ok=false on failure.
var x1337Detail = func(ctx context.Context, base, path string) (string, string, bool) {
	select {
	case x1337DetailSlots <- struct{}{}:
	case <-ctx.Done():
		return "", "", false
	}
	defer func() { <-x1337DetailSlots }()
	detailHTML, err := getText(ctx, base+path, nil)
	if err != nil {
		return "", "", false
	}
	magnet := x1337Magnet.FindString(detailHTML)
	if magnet == "" {
		return "", "", false
	}
	return html.UnescapeString(magnet), x1337UploadDate(detailHTML), true
}

// x1337Parse searches (or, with an empty query, lists popular) movies and
// TV; maxItems > 0 caps how many detail pages are fetched.
func x1337Parse(ctx context.Context, query string, maxItems int) (string, error) {
	q := strings.TrimSpace(query)
	type page struct{ path, category string }
	var pages []page
	if q != "" {
		encoded := strings.ReplaceAll(pyQuote(q), "%20", "+")
		pages = []page{
			{"/category-search/" + encoded + "/Movies/1/", "Video - Movies"},
			{"/category-search/" + encoded + "/TV/1/", "Video - TV shows"},
		}
	} else {
		pages = []page{{"/popular-movies", "Video - Movies"}, {"/popular-tv", "Video - TV shows"}}
	}
	var toks []string
	for _, t := range tokens(q) {
		if !x1337StopWords[t] {
			toks = append(toks, t)
		}
	}

	type candidate struct {
		base, category string
		row            []string
	}
	listings := make([][]candidate, len(pages))
	var wg sync.WaitGroup
	for i, p := range pages {
		goSafe(&wg, "1337x listing", func() {
			base, listHTML, err := x1337Fetch(ctx, p.path)
			if err != nil {
				return
			}
			for _, r := range x1337Rows(listHTML) {
				if matchesAll(r[0], toks) {
					listings[i] = append(listings[i], candidate{base, p.category, r})
				}
			}
		})
	}
	wg.Wait()
	var candidates []candidate
	for _, l := range listings {
		candidates = append(candidates, l...)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, _ := strconv.Atoi(candidates[i].row[3])
		b, _ := strconv.Atoi(candidates[j].row[3])
		return a > b
	})
	if maxItems > 0 && len(candidates) > maxItems {
		candidates = candidates[:maxItems]
	}

	type detail struct {
		magnet, date string
		ok           bool
	}
	details := make([]detail, len(candidates))
	for i, c := range candidates {
		goSafe(&wg, "1337x detail", func() {
			magnet, date, ok := x1337Detail(ctx, c.base, c.row[1])
			details[i] = detail{magnet, date, ok}
		})
	}
	wg.Wait()
	var rows [][]string
	for i, c := range candidates {
		if !details[i].ok {
			continue
		}
		name, path, size, seeds, leeches := c.row[0], c.row[1], c.row[2], c.row[3], c.row[4]
		rows = append(rows, row(name, c.category, size, seeds, leeches, nil, details[i].date, details[i].magnet, c.base+path))
	}
	return formatRows(rows), nil
}
