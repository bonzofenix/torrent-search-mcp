package search

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bonzofenix/torrent-search-mcp/internal/torrent"
)

func mk(t *testing.T, name, source string, seeders, leechers int) torrent.Torrent {
	t.Helper()
	out, err := torrent.Format(map[string]string{
		"filename": name, "size": "1 GB", "date": "d",
		"seeders": itoa(seeders), "leechers": itoa(leechers), "magnet_link": "magnet:?" + name,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func names(ts []torrent.Torrent) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Filename+"@"+torrent.Deref(t.Source))
	}
	return out
}

func TestAvailableSourcesAndExclusions(t *testing.T) {
	t.Setenv("EXCLUDE_SOURCES", " eztvx.to , ,1337x.to")
	a := New()
	want := []string{"nyaa.si", "yts.vg", "thepiratebay.org", "fitgirl-repacks.site", "subsplease.org", "uindex.org"}
	if got := a.AvailableSources(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %v", got)
	}
	if !reflect.DeepEqual(a.Excluded, []string{"eztvx.to", "1337x.to"}) {
		t.Fatalf("excluded = %v", a.Excluded)
	}
}

func TestSearchTorrentsRanksCapsAndCaches(t *testing.T) {
	a := New()
	var calls atomic.Int32
	a.search = func(_ context.Context, query string, _ []string) []torrent.Torrent {
		calls.Add(1)
		if query != "sample show" {
			t.Errorf("query should be lowercased: %q", query)
		}
		return []torrent.Torrent{mk(t, "low", "apibay.org", 1, 0), mk(t, "high", "nyaa.si", 9, 0), mk(t, "mid", "yts.mx", 5, 0)}
	}
	got := a.SearchTorrents(context.Background(), "Sample Show", 2, 0)
	if !reflect.DeepEqual(names(got), []string{"high@nyaa.si", "mid@yts.vg"}) {
		t.Fatalf("got %v", names(got))
	}
	query, maxItems, source, _, err := torrent.ExtractInfo(got[1].ID)
	if err != nil || query != "sample show" || maxItems != 2 || source != "yts.mx" {
		t.Fatalf("id info = %q %d %q %v", query, maxItems, source, err)
	}
	magnet, ok := a.GetTorrent(context.Background(), got[0].ID)
	if !ok || magnet != "magnet:?high" || calls.Load() != 1 {
		t.Fatalf("cached lookup = %q %v calls=%d", magnet, ok, calls.Load())
	}
}

func TestSearchTorrentsPerSourceSpread(t *testing.T) {
	a := New()
	a.search = func(context.Context, string, []string) []torrent.Torrent {
		return []torrent.Torrent{mk(t, "a1", "a.example", 1, 0), mk(t, "a9", "a.example", 9, 0), mk(t, "a5", "a.example", 5, 0), mk(t, "b2", "b.example", 2, 0)}
	}
	got := a.SearchTorrents(context.Background(), "q", 0, 2)
	if !reflect.DeepEqual(names(got), []string{"a9@a.example", "a5@a.example", "b2@b.example"}) {
		t.Fatalf("got %v", names(got))
	}
	if _, maxItems, _, _, _ := torrent.ExtractInfo(got[0].ID); maxItems != 20 {
		t.Fatalf("uncapped search should embed the default hint, got %d", maxItems)
	}
}

func TestSearchTorrentsSingleFlight(t *testing.T) {
	a := New()
	release := make(chan struct{})
	var calls atomic.Int32
	a.search = func(context.Context, string, []string) []torrent.Torrent {
		calls.Add(1)
		<-release
		return []torrent.Torrent{mk(t, "x", "nyaa.si", 1, 0)}
	}
	var wg sync.WaitGroup
	results := make([][]torrent.Torrent, 5)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = a.SearchTorrents(context.Background(), "q", 20, 0) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("identical concurrent searches should coalesce, calls=%d", calls.Load())
	}
	for _, r := range results {
		if strings.Count(r[0].ID, "-20-") != 1 {
			t.Fatalf("shared results must not be double-prefixed: %s", r[0].ID)
		}
	}
}

func TestGetTorrentResearchesExpiredIDs(t *testing.T) {
	a := New()
	a.search = func(context.Context, string, []string) []torrent.Torrent {
		return []torrent.Torrent{mk(t, "x", "nyaa.si", 1, 0)}
	}
	id := a.SearchTorrents(context.Background(), "q", 20, 0)[0].ID
	fresh := New()
	fresh.search = a.search
	if magnet, ok := fresh.GetTorrent(context.Background(), id); !ok || magnet != "magnet:?x" {
		t.Fatalf("re-search = %q %v", magnet, ok)
	}
	if _, ok := fresh.GetTorrent(context.Background(), "garbage"); ok {
		t.Fatal("garbage ids must miss")
	}
	if _, ok := fresh.GetTorrent(context.Background(), torrent.Compress("q")+"-20-nyaa.si-nomatch"); ok {
		t.Fatal("unknown ids must miss after re-search")
	}
}

func TestPopularTorrentsMemoAndIDs(t *testing.T) {
	a := New()
	var calls atomic.Int32
	a.popular = func(_ context.Context, _ []string, perSource int) []torrent.Torrent {
		calls.Add(1)
		if perSource != 3 {
			t.Errorf("perSource = %d", perSource)
		}
		return []torrent.Torrent{mk(t, "p", "apibay.org", 3, 0)}
	}
	got := a.PopularTorrents(context.Background(), 3)
	again := a.PopularTorrents(context.Background(), 3)
	if calls.Load() != 1 || got[0].ID != again[0].ID {
		t.Fatalf("popular should be memoized: calls=%d", calls.Load())
	}
	if torrent.Deref(got[0].Source) != "thepiratebay.org" {
		t.Fatalf("source = %v", got[0].Source)
	}
	query, perSource, _, _, err := torrent.ExtractInfo(got[0].ID)
	if err != nil || query != "" || perSource != 3 {
		t.Fatalf("id info = %q %d %v", query, perSource, err)
	}
	if magnet, ok := a.GetTorrent(context.Background(), got[0].ID); !ok || magnet != "magnet:?p" {
		t.Fatalf("popular ids resolve from cache: %q %v", magnet, ok)
	}
	if _, ok := New().GetTorrent(context.Background(), got[0].ID); ok {
		t.Fatal("expired popular ids cannot be re-searched")
	}
}
