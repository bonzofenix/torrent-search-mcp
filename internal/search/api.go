// Package search ports torrent_search/wrapper/api_client.py: the search
// facade the MCP tools call in standalone mode.
package search

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/bonzofenix/torrent-search-mcp/internal/scrape"
	"github.com/bonzofenix/torrent-search-mcp/internal/torrent"
)

// sourceDisplayNames maps scraping keys to the domains shown to users. Data
// keeps flowing through the original mirrors/APIs; only the displayed names
// are normalized.
var sourceDisplayNames = map[string]string{
	"apibay.org": "thepiratebay.org",
	"yts.mx":     "yts.vg",
}

// DisplaySource returns the public domain for a scraping key.
func DisplaySource(name string) string {
	if shown, ok := sourceDisplayNames[name]; ok {
		return shown
	}
	return name
}

// popularMemoTTL mirrors the 2-minute @cached decorator on popular_torrents.
const popularMemoTTL = 120 * time.Second

type popularMemo struct {
	at       time.Time
	torrents []torrent.Torrent
}

// API searches torrents across the enabled sources.
type API struct {
	Sources  []string // enabled scraping keys
	Excluded []string

	cache  *torrent.Cache
	flight singleflight.Group

	// Swappable for tests.
	search  func(ctx context.Context, query string, sources []string) []torrent.Torrent
	popular func(ctx context.Context, sources []string, perSource int) []torrent.Torrent

	memoMu sync.Mutex
	memo   map[int]popularMemo
}

// New builds the API, honoring EXCLUDE_SOURCES (comma-separated scraping keys).
func New() *API {
	a := &API{
		cache:   torrent.NewCache(time.Hour, 5000),
		search:  scrape.SearchTorrents,
		popular: scrape.PopularTorrents,
		memo:    map[int]popularMemo{},
	}
	excluded := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("EXCLUDE_SOURCES"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			a.Excluded = append(a.Excluded, s)
			excluded[s] = true
		}
	}
	for _, w := range scrape.Websites {
		if !excluded[w.Name] {
			a.Sources = append(a.Sources, w.Name)
		}
	}
	return a
}

// AvailableSources lists the enabled sources by display domain, deduplicated.
func (a *API) AvailableSources() []string {
	seen := []string{}
	for _, name := range a.Sources {
		shown := DisplaySource(name)
		found := false
		for _, s := range seen {
			found = found || s == shown
		}
		if !found {
			seen = append(seen, shown)
		}
	}
	return seen
}

func sortByHealth(torrents []torrent.Torrent) {
	sort.SliceStable(torrents, func(i, j int) bool {
		return torrents[i].Seeders+torrents[i].Leechers > torrents[j].Seeders+torrents[j].Leechers
	})
}

// share runs fn once for concurrent identical calls (single-flight). The
// shared run is detached from the first caller's cancellation, like the
// shielded asyncio task in Python; sources carry their own timeouts.
func (a *API) share(ctx context.Context, key string, fn func(context.Context) []torrent.Torrent) []torrent.Torrent {
	v, _, _ := a.flight.Do(key, func() (any, error) {
		return fn(context.WithoutCancel(ctx)), nil
	})
	shared := v.([]torrent.Torrent)
	return append([]torrent.Torrent(nil), shared...) // callers mutate ids and sources
}

// SearchTorrents searches every enabled source.
//
// Searches are not cached: new torrents can appear at any time. The results
// are still stored in the 1-hour torrent cache so that GetTorrent can
// resolve magnet links without re-scraping. maxItems <= 0 means no global
// cap; perSource > 0 keeps at most that many results per source, ranked by
// swarm health, before the global cap.
func (a *API) SearchTorrents(ctx context.Context, query string, maxItems, perSource int) []torrent.Torrent {
	query = strings.ToLower(query)
	found := a.share(ctx, fmt.Sprintf("search:%s:%d", query, maxItems), func(ctx context.Context) []torrent.Torrent {
		return a.search(ctx, query, a.Sources)
	})

	if perSource > 0 {
		var order []string
		grouped := map[string][]torrent.Torrent{}
		for _, t := range found {
			key := torrent.Deref(t.Source)
			if _, ok := grouped[key]; !ok {
				order = append(order, key)
			}
			grouped[key] = append(grouped[key], t)
		}
		var spread []torrent.Torrent
		for _, key := range order {
			group := grouped[key]
			sortByHealth(group)
			if len(group) > perSource {
				group = group[:perSource]
			}
			spread = append(spread, group...)
		}
		found = spread
	}

	sortByHealth(found)
	if maxItems > 0 && len(found) > maxItems {
		found = found[:maxItems]
	}
	hint := maxItems
	if hint <= 0 {
		hint = 20 // re-search hint: uncapped searches still embed the default
	}
	for i := range found {
		found[i].Source = torrent.Ptr(DisplaySource(torrent.Deref(found[i].Source)))
		found[i].PrependInfo(query, hint)
	}
	a.cache.Clean()
	a.cache.Update(found)
	return found
}

// PopularTorrents returns the most popular torrents per source with a top
// listing, up to perSource each (0 = all), memoized for two minutes.
func (a *API) PopularTorrents(ctx context.Context, perSource int) []torrent.Torrent {
	a.memoMu.Lock()
	if m, ok := a.memo[perSource]; ok && time.Since(m.at) < popularMemoTTL {
		a.memoMu.Unlock()
		return append([]torrent.Torrent(nil), m.torrents...)
	}
	a.memoMu.Unlock()

	found := a.share(ctx, fmt.Sprintf("popular:%d", perSource), func(ctx context.Context) []torrent.Torrent {
		return a.popular(ctx, nil, perSource)
	})
	for i := range found {
		found[i].Source = torrent.Ptr(DisplaySource(torrent.Deref(found[i].Source)))
		// Empty query marker: ids stay decodable but are not re-searchable.
		found[i].PrependInfo("", perSource)
	}
	a.cache.Clean()
	a.cache.Update(found)

	a.memoMu.Lock()
	a.memo[perSource] = popularMemo{time.Now(), found}
	a.memoMu.Unlock()
	return append([]torrent.Torrent(nil), found...)
}

// GetTorrent returns the magnet link of a previously found torrent, falling
// back to re-running the search embedded in its id.
func (a *API) GetTorrent(ctx context.Context, torrentID string) (string, bool) {
	found, cached := a.cache.Get(torrentID)

	query, maxItems := "", 10
	if q, n, _, _, err := torrent.ExtractInfo(torrentID); err == nil {
		query, maxItems = q, n
	}
	if query == "" && !cached {
		// Garbage ids, or popular-listing ids whose cache entry expired
		// (they carry no query marker and cannot be re-searched).
		log.Printf("WARNING: Invalid torrent ID: %s", torrentID)
		return "", false
	}

	if !cached {
		for _, t := range a.SearchTorrents(ctx, query, maxItems, 0) {
			if t.ID == torrentID {
				found, cached = t, true
				break
			}
		}
	}
	a.cache.Clean()

	if cached && torrent.Deref(found.MagnetLink) != "" {
		return *found.MagnetLink, true
	}
	return "", false
}
