package scrape

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/bonzofenix/torrent-search-mcp/internal/torrent"
)

// Parser fetches a source and returns its results as CSV text.
type Parser func(ctx context.Context, query string) (string, error)

// Website is one searchable source, keyed by its scraping name.
type Website struct {
	Name  string
	Parse Parser
}

// Websites is the source registry, in the order results are merged.
var Websites = []Website{
	{"nyaa.si", nyaaParse},
	{"yts.mx", func(ctx context.Context, q string) (string, error) { return ytsParse(ctx, q, "") }},
	{"apibay.org", apibayParse}, // official ThePirateBay API (shown as thepiratebay.org)
	{"eztvx.to", eztvParse},
	{"fitgirl-repacks.site", fitgirlParse},
	{"subsplease.org", subspleaseParse},
	{"uindex.org", uindexParse},
	{"1337x.to", func(ctx context.Context, q string) (string, error) { return x1337Parse(ctx, q, 0) }},
}

// PopularFetcher returns a source's top listing as CSV text; perSource > 0
// is the requested per-source limit (0 = everything).
type PopularFetcher func(ctx context.Context, perSource int) (string, error)

// PopularSource is a source exposing a top/popular listing.
type PopularSource struct {
	Name  string
	Fetch PopularFetcher
}

// PopularSources lists the sources with a top listing, same CSV contract as
// the search parsers.
var PopularSources = []PopularSource{
	{"apibay.org", func(ctx context.Context, _ int) (string, error) { return apibayParse(ctx, "") }}, // official TPB top100 (movies + HD)
	{"uindex.org", func(ctx context.Context, _ int) (string, error) { return uindexParse(ctx, "") }}, // site-wide top list, magnets inline
	{"1337x.to", func(ctx context.Context, limit int) (string, error) { return x1337Parse(ctx, "", limit) }},
	{"yts.mx", func(ctx context.Context, _ int) (string, error) { return ytsParse(ctx, "", "seeds") }}, // most seeded uploads
	{"nyaa.si", func(ctx context.Context, _ int) (string, error) { return nyaaPopular(ctx) }},
	{"eztvx.to", func(ctx context.Context, _ int) (string, error) { return eztvParse(ctx, "") }}, // latest releases as candidate pool
}

// SourceTimeout is the hard cap per source per request (the yts mirror
// alone can take ~20s).
const SourceTimeout = 30 * time.Second

// PopularTTL is the per-source fresh window for popular listings: within it
// a source is served from its own cache, so a popular call only refetches
// genuinely stale sources.
var PopularTTL = 300 * time.Second

type popularEntry struct {
	fetchedAt time.Time
	used      int // per_source used for the fetch (0 = full listing)
	text      string
}

var (
	popularMu         sync.Mutex
	popularCache      = map[string]popularEntry{}
	popularRefreshing = map[string]bool{}
)

// safely runs fn, turning a panic into an error so one broken parser can
// never take the long-lived MCP process down.
func safely(fn func() (string, error)) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

func scrapeSource(ctx context.Context, name string, parse Parser, query string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, SourceTimeout)
	defer cancel()
	text, err := safely(func() (string, error) { return parse(ctx, query) })
	if err != nil {
		log.Printf("WARNING: Error scraping %s for query '%s': %v", name, query, err)
		return "", false
	}
	return "SOURCE -> " + name + "\n" + text, true
}

func enabled(name string, sources []string) bool {
	if sources == nil {
		return true
	}
	for _, s := range sources {
		if s == name {
			return true
		}
	}
	return false
}

// ScrapeTorrents fetches all enabled sources in parallel (nil sources = all)
// and returns their texts in registry order.
func ScrapeTorrents(ctx context.Context, query string, sources []string) []string {
	EnsureTrackers(ctx)
	results := make([]string, len(Websites))
	ok := make([]bool, len(Websites))
	var wg sync.WaitGroup
	for i, w := range Websites {
		if !enabled(w.Name, sources) {
			continue
		}
		goSafe(&wg, "scrape "+w.Name, func() {
			results[i], ok[i] = scrapeSource(ctx, w.Name, w.Parse, query)
		})
	}
	wg.Wait()
	var texts []string
	for i, text := range results {
		if ok[i] {
			texts = append(texts, text)
		}
	}
	return texts
}

// SearchTorrents searches every enabled source.
func SearchTorrents(ctx context.Context, query string, sources []string) []torrent.Torrent {
	start := time.Now()
	torrents := ExtractTorrents(ScrapeTorrents(ctx, query, sources))
	log.Printf("INFO: Extracted %d torrents in %.2f sec.", len(torrents), time.Since(start).Seconds())
	return torrents
}

func fetchPopularText(ctx context.Context, src PopularSource, perSource int) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, SourceTimeout)
	defer cancel()
	text, err := safely(func() (string, error) { return src.Fetch(ctx, perSource) })
	if err != nil {
		log.Printf("WARNING: Error fetching popular listing from %s: %v", src.Name, err)
		return "", false
	}
	return "SOURCE -> " + src.Name + "\n" + text, true
}

// popularCovers reports whether a listing fetched with used satisfies
// requested. 0 means the full listing; any other value is a best-N
// truncation, so a fuller fetch can serve a smaller request but never the
// other way around.
func popularCovers(used, requested int) bool {
	if used == 0 {
		return true
	}
	return requested != 0 && used >= requested
}

// refreshPopular refreshes a stale popular listing in the background,
// keeping the best coverage.
func refreshPopular(src PopularSource, perSource int) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("WARNING: popular refresh of %s panicked: %v", src.Name, r)
		}
		popularMu.Lock()
		delete(popularRefreshing, src.Name)
		popularMu.Unlock()
	}()
	text, ok := fetchPopularText(context.Background(), src, perSource)
	if !ok {
		return
	}
	popularMu.Lock()
	defer popularMu.Unlock()
	if existing, found := popularCache[src.Name]; !found || popularCovers(perSource, existing.used) {
		popularCache[src.Name] = popularEntry{time.Now(), perSource, text}
	}
}

func popularSource(ctx context.Context, src PopularSource, perSource int) (string, bool) {
	popularMu.Lock()
	entry, found := popularCache[src.Name]
	if found && popularCovers(entry.used, perSource) {
		if time.Since(entry.fetchedAt) >= PopularTTL && !popularRefreshing[src.Name] {
			// Stale but usable: answer now and refresh in the background, so
			// a slow source never holds back the aggregate listing.
			popularRefreshing[src.Name] = true
			go refreshPopular(src, perSource)
		}
		popularMu.Unlock()
		return entry.text, true
	}
	popularMu.Unlock()
	text, ok := fetchPopularText(ctx, src, perSource)
	if ok {
		popularMu.Lock()
		popularCache[src.Name] = popularEntry{time.Now(), perSource, text}
		popularMu.Unlock()
	}
	return text, ok
}

func sortByHealth(torrents []torrent.Torrent) {
	sort.SliceStable(torrents, func(i, j int) bool {
		return torrents[i].Seeders+torrents[i].Leechers > torrents[j].Seeders+torrents[j].Leechers
	})
}

// PopularTorrents returns the current top listings of every supporting
// source (nil sources = all), keeping up to perSource results per source
// (0 = all), ranked by seeders + leechers.
func PopularTorrents(ctx context.Context, sources []string, perSource int) []torrent.Torrent {
	start := time.Now()
	EnsureTrackers(ctx)
	results := make([]string, len(PopularSources))
	ok := make([]bool, len(PopularSources))
	var wg sync.WaitGroup
	for i, src := range PopularSources {
		if !enabled(src.Name, sources) {
			continue
		}
		goSafe(&wg, "popular "+src.Name, func() {
			results[i], ok[i] = popularSource(ctx, src, perSource)
		})
	}
	wg.Wait()
	var torrents []torrent.Torrent
	for i, text := range results {
		if !ok[i] {
			continue
		}
		found := ExtractTorrents([]string{text})
		sortByHealth(found)
		if perSource > 0 && len(found) > perSource {
			found = found[:perSource]
		}
		torrents = append(torrents, found...)
	}
	sortByHealth(torrents)
	log.Printf("INFO: Extracted %d popular torrents in %.2f sec.", len(torrents), time.Since(start).Seconds())
	return torrents
}
