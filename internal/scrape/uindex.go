package scrape

import (
	"context"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// uindex.org - browse top list (search endpoint is Cloudflare-challenged).

const uindexTopURL = "https://uindex.org/top.php"

var (
	uindexAge        = regexp.MustCompile(`(\d+(?:\.\d+)?)\s+(second|minute|hour|day|week|month|year)s?\s+ago`)
	uindexAgeSeconds = map[string]float64{
		"second": 1, "minute": 60, "hour": 3600, "day": 86400,
		"week": 604800, "month": 2592000, "year": 31536000,
	}
	uindexTR    = regexp.MustCompile(`(?s)<tr><td class="top-col-rank">.*?</tr>`)
	uindexName  = regexp.MustCompile(`(?s)href="(/details\.php\?id=\d+)"[^>]*class="sr-torrent-link"[^>]*>(.*?)</a>`)
	uindexCat   = regexp.MustCompile(`sr-cat-badge[^>]*>(?:<svg.*?</svg>)?\s*([^<]+)</a>`)
	uindexSize  = regexp.MustCompile(`class="sr-col-size">([^<]+)<`)
	uindexDate  = regexp.MustCompile(`class="sr-col-uploaded"[^>]*>([^<]+)<`)
	uindexSeed  = regexp.MustCompile(`class="sr-seed">([\d,]+)<`)
	uindexLeech = regexp.MustCompile(`class="sr-leech">([\d,]+)<`)
	spanTag     = regexp.MustCompile(`<span[^>]*>.*?</span>`)
	anyTag      = regexp.MustCompile(`<[^>]+>`)
)

// now is swapped by tests to freeze the clock.
var now = time.Now

// uindexDateOf converts uIndex's relative ages ("2.9 days ago") to YYYY-MM-DD.
func uindexDateOf(value string) string {
	m := uindexAge.FindStringSubmatch(value)
	if m == nil {
		return FmtDate(value)
	}
	amount, _ := strconv.ParseFloat(m[1], 64)
	seconds := amount * uindexAgeSeconds[m[2]]
	return now().Add(-time.Duration(seconds * float64(time.Second))).UTC().Format("2006-01-02")
}

// uindexRows parses the top-list table; the name link href is the magnet itself.
func uindexRows(htmlText string) [][]string {
	var rows [][]string
	for _, tr := range uindexTR.FindAllString(htmlText, -1) {
		magnet := hrefMagnet.FindStringSubmatch(tr)
		name := uindexName.FindStringSubmatch(tr)
		if magnet == nil || name == nil {
			continue
		}
		first := func(re *regexp.Regexp, fallback string) string {
			if m := re.FindStringSubmatch(tr); m != nil {
				return m[1]
			}
			return fallback
		}
		category := "N/A"
		if m := uindexCat.FindStringSubmatch(tr); m != nil {
			category = strings.TrimSpace(m[1])
		}
		date := "N/A"
		if m := uindexDate.FindStringSubmatch(tr); m != nil {
			date = uindexDateOf(m[1])
		}
		filename := strings.TrimSpace(html.UnescapeString(anyTag.ReplaceAllString(spanTag.ReplaceAllString(name[2], ""), " ")))
		rows = append(rows, row(filename, category, first(uindexSize, "N/A"),
			strings.ReplaceAll(first(uindexSeed, "0"), ",", ""), strings.ReplaceAll(first(uindexLeech, "0"), ",", ""),
			nil, date, html.UnescapeString(magnet[1]), "https://uindex.org"+name[1]))
	}
	return rows
}

func uindexParse(ctx context.Context, query string) (string, error) {
	// /search.php is Cloudflare-challenged for non-browsers, but /top.php is
	// open and carries magnets inline, so queries are matched client-side
	// against the current top list (same approach as EZTV).
	text, err := getText(ctx, uindexTopURL, nil)
	if err != nil {
		return "", err
	}
	var rows [][]string
	toks := tokens(query)
	for _, r := range uindexRows(text) {
		if matchesAll(r[0], toks) {
			rows = append(rows, r)
		}
	}
	return formatRows(rows), nil
}
