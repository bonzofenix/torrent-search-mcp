package scrape

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var magnet40 = "magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&dn=x"

// fakeText swaps getText for the duration of a test.
func fakeText(t *testing.T, fn func(u string, params url.Values) (string, error)) {
	t.Helper()
	orig := getText
	getText = func(_ context.Context, u string, params url.Values) (string, error) { return fn(u, params) }
	t.Cleanup(func() { getText = orig })
}

func fixed(body string) func(string, url.Values) (string, error) {
	return func(string, url.Values) (string, error) { return body, nil }
}

func extract(t *testing.T, source, out string) []string {
	t.Helper()
	var names []string
	for _, torrent := range ExtractTorrents([]string{"SOURCE -> " + source + "\n" + out}) {
		names = append(names, torrent.Filename)
	}
	return names
}

func TestMergeTrackers(t *testing.T) {
	merged := mergeTrackers("# comment\nudp://tracker.remote.example:1337/announce\n\n" +
		"udp://tracker.opentrackr.org:1337/announce\nudp://tracker.remote.example:1337/announce\n")
	if merged[0] != Trackers[0] || merged[len(merged)-1] != "udp://tracker.remote.example:1337/announce" {
		t.Fatalf("merged = %v", merged)
	}
	if len(merged) != len(Trackers)+1 {
		t.Fatalf("merged should be deduplicated: %v", merged)
	}
}

func TestBuildMagnet(t *testing.T) {
	magnet := BuildMagnet(strings.Repeat("a", 40), "My Torrent & File")
	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:"+strings.Repeat("a", 40)+"&dn=My%20Torrent%20%26%20File&tr=") {
		t.Fatalf("magnet = %s", magnet)
	}
	if !strings.Contains(magnet, "&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce") {
		t.Fatalf("magnet trackers = %s", magnet)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[any]string{
		0: "N/A", 512: "512 B", 487900000: "465.3 MiB", int64(150389060754): "140.1 GiB",
		"150389060754": "140.1 GiB", "bogus": "N/A", nil: "N/A", 1023: "1023 B", 1024: "1.0 KiB",
		1048575: "1024.0 KiB", 3628388371660.8: "3.3 TiB", "12.0": "12 B", 0.5: "0 B",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%v) = %q, want %q", in, got, want)
		}
	}
}

// Expected values produced by the Python fmt_date.
func TestFmtDate(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "N/A"}, {0, "N/A"}, {true, "N/A"}, {"", "N/A"}, {"0", "N/A"},
		{1614202551, "2021-02-24T21:35:51+00:00"},
		{"1614202551", "2021-02-24T21:35:51+00:00"},
		{"2026-03-02T10:06:49.527547+00:00", "2026-03-02T10:06:49.527547+00:00"},
		{"Sat, 08 Aug 2026 18:16:12 +0000", "2026-08-08T18:16:12+00:00"},
		{"Mon, 10 Aug 2026 08:09:35 -0000", "2026-08-10T08:09:35+00:00"},
		{"Sun, 09 Aug 2026 16:03:43 GMT", "2026-08-09T16:03:43+00:00"},
		{"Tue, 1 Sep 2026 1:02:03 +0200", "2026-09-01T01:02:03+02:00"},
		{"2026-08-23 15:01", "2026-08-23T15:01:00"},
		{"2026-01-01T12:00:00Z", "2026-01-01T12:00:00+00:00"},
		{"2026-01-01T12:00:00.5+0530", "2026-01-01T12:00:00.500000+05:30"},
		{"2026-02-30T00:00:00", "N/A"},
		{"2026-01-01", "2026-01-01"},
		{"garbage", "N/A"},
	}
	for _, c := range cases {
		if got := FmtDate(c.in); got != c.want {
			t.Errorf("FmtDate(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRSSFieldHandlesCDATAAndCase(t *testing.T) {
	item := "<item><title><![CDATA[My &amp; Title]]></title><nyaa:infoHash>abc</nyaa:infoHash></item>"
	if rssField(item, "title") != "My &amp; Title" || rssField(item, "nyaa:infohash") != "abc" || rssField(item, "missing") != "" {
		t.Fatal("rssField mismatch")
	}
}

func TestRowSanitizesSemicolons(t *testing.T) {
	got := row("Name;With;Semi", "Video", "1 GB", 1, 2, nil, nil, magnet40, "")
	want := []string{"Name,With,Semi", "Video", "1 GB", "1", "2", "N/A", "N/A", magnet40, ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row = %v", got)
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("a\r\nb\rc\nd e\n")
	if !reflect.DeepEqual(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("splitLines = %q", got)
	}
}

func TestFirstHostRotationAndPreference(t *testing.T) {
	var calls []string
	fakeText(t, func(u string, _ url.Values) (string, error) {
		calls = append(calls, u)
		if strings.Contains(u, "bad1") {
			return "", errors.New("down")
		}
		return "ok", nil
	})
	hosts := []string{"bad1.example", "good.example"}
	if text, err := getFirst(context.Background(), hosts, "/path", nil); err != nil || text != "ok" {
		t.Fatalf("getFirst = %q %v", text, err)
	}
	calls = nil
	if _, err := getFirst(context.Background(), hosts, "/path", nil); err != nil || len(calls) != 1 {
		t.Fatalf("last good host should be tried first: %v", calls)
	}
	fakeText(t, func(string, url.Values) (string, error) { return "", errors.New("down") })
	if _, err := getFirst(context.Background(), []string{"a.example", "b.example"}, "/path", nil); err == nil {
		t.Fatal("all hosts failing must error")
	}
	if _, err := getFirst(context.Background(), nil, "/path", nil); err == nil || !strings.Contains(err.Error(), "no hosts") {
		t.Fatalf("no hosts: %v", err)
	}
}

func TestExtractTorrentsFromCSVText(t *testing.T) {
	text := "SOURCE -> apibay.org\n" + CSVHeader + "\n" +
		"Sample Show S01 1080p;Video - Movies;1.2 GB;10;5;100;2026-01-01;magnet:?xt=urn:btih:abcdef&dn=x;\n" +
		"Sample Series S01 720p;Video - TV shows;800 MB;3;1;50;2025-06-01;magnet:?xt=urn:btih:fedcba&dn=y;"
	got := ExtractTorrents([]string{text})
	if len(got) != 2 || got[0].Filename != "Sample Show S01 1080p" || *got[0].Source != "apibay.org" || got[0].Seeders != 10 {
		t.Fatalf("got %v", got)
	}
}

func TestExtractTorrentsEdgeRows(t *testing.T) {
	if got := ExtractTorrents([]string{"SOURCE -> eztvx.to\nNo results", "SOURCE -> yts.mx\n" + CSVHeader + "\nx;y;z;1;1;1;d;m"}); len(got) != 1 {
		t.Fatalf("no results source must be skipped: %v", got)
	}
	trailing := CSVHeader + "\nName;Anime;1 GB;2;1;50;2026-01-01;magnet:?xt=urn:btih:aaa&dn=x;;"
	if got := extract(t, "nyaa.si", trailing); !reflect.DeepEqual(got, []string{"Name"}) {
		t.Fatalf("trailing empties: %v", got)
	}
	malformed := CSVHeader + "\nGood;Anime;1 GB;2;1;50;2026-01-01;m\nBad;Anime;1 GB;not-a-number;1;50;2026-01-01;m"
	if got := extract(t, "nyaa.si", malformed); !reflect.DeepEqual(got, []string{"Good"}) {
		t.Fatalf("malformed: %v", got)
	}
	overflow := CSVHeader + "\nName;Anime;1 GB;2;1;50;2026-01-01;magnet:?xt=urn:btih:aaa&dn=x;;extra"
	got := ExtractTorrents([]string{"SOURCE -> nyaa.si\n" + overflow})
	if len(got) != 1 || got[0].Filename != "Name" || *got[0].MagnetLink != "magnet:?xt=urn:btih:aaa&dn=x" {
		t.Fatalf("overflow: %v", got)
	}
}

func TestYTS(t *testing.T) {
	data, _ := decodeJSON(`{"data":{"movies":[{"title_long":"Sample Show (2008)","date_uploaded_unix":1614202551,"torrents":[
		{"hash":"` + strings.Repeat("B", 40) + `","quality":"1080p","type":"webrip","size_bytes":1500000000,"seeds":5,"peers":2},
		{"hash":"` + strings.Repeat("c", 40) + `","quality":"720p","type":"bluray","size_bytes":800000000,"seeds":1,"peers":0}]},
		{"title_long":"No Torrents","torrents":[{"quality":"1080p"},{"hash":"","quality":"720p"}]}]}}`)
	rows := ytsRows(asMap(data))
	if len(rows) != 2 || rows[0][0] != "Sample Show (2008) [1080p webrip]" || rows[0][6] != "2021-02-24T21:35:51+00:00" ||
		!strings.HasPrefix(rows[0][7], "magnet:?xt=urn:btih:"+strings.Repeat("b", 40)) || rows[0][3] != "5" {
		t.Fatalf("rows = %v", rows)
	}

	var seen url.Values
	orig := getFirst
	t.Cleanup(func() { getFirst = orig })
	getFirst = func(_ context.Context, _ []string, _ string, params url.Values) (string, error) {
		seen = params
		if params.Get("query_term") != "" {
			return `{"data":{"movies":[{"title":"Test","torrents":[{"hash":"` + strings.Repeat("d", 40) + `","quality":"720p","type":"web"}]}]}}`, nil
		}
		return `{"data":{"movies":[]}}`, nil
	}
	out, _ := ytsParse(context.Background(), "test", "")
	if got := extract(t, "yts.mx", out); !reflect.DeepEqual(got, []string{"Test [720p web]"}) {
		t.Fatalf("parse = %v", got)
	}
	if out, _ := ytsParse(context.Background(), "", ""); out != "No results" || seen.Get("sort_by") != "date_added" {
		t.Fatalf("browse = %q %v", out, seen)
	}
}

func TestApibay(t *testing.T) {
	data, _ := decodeJSON(`[
		{"id":"1","info_hash":"` + strings.Repeat("d", 40) + `","name":"Show S01 1080p","category":"205","size":"1500000000","seeders":"100","leechers":"10","added":"1614202551"},
		{"id":"0","info_hash":"` + strings.Repeat("e", 40) + `","name":"dead","category":"0"},
		{"id":"2","info_hash":"` + strings.Repeat("0", 40) + `","name":"zero hash"},
		{"id":"3","info_hash":"` + strings.Repeat("f", 40) + `","name":"weird cat","category":"zzz"},
		{"info_hash":"` + strings.Repeat("a", 40) + `","name":"missing id","category":"200"}]`)
	rows := apibayRows(asList(data))
	if len(rows) != 3 || rows[0][1] != "Video - TV shows" || rows[0][3] != "100" || rows[0][2] != "1.4 GiB" ||
		rows[0][8] != "https://thepiratebay.org/description.php?id=1" || rows[1][1] != "Video" || rows[2][8] != "" {
		t.Fatalf("rows = %v", rows)
	}
	fakeText(t, func(u string, params url.Values) (string, error) {
		switch {
		case strings.Contains(u, "q.php") && params.Get("q") == "sample show":
			return `[{"id":"1","info_hash":"` + strings.Repeat("d", 40) + `","name":"Show","category":"201"}]`, nil
		case strings.Contains(u, "207"):
			return `[{"id":"1","info_hash":"` + strings.Repeat("d", 40) + `","name":"Movie","category":"207"}]`, nil
		case strings.Contains(u, "208"):
			return `[{"id":"2","info_hash":"` + strings.Repeat("e", 40) + `","name":"Show","category":"205"}]`, nil
		}
		return "", errors.New("unexpected " + u)
	})
	out, _ := apibayParse(context.Background(), "sample show")
	if got := ExtractTorrents([]string{"SOURCE -> apibay.org\n" + out}); *got[0].Category != "Video - Movies" {
		t.Fatalf("search = %v", got)
	}
	out, _ = apibayParse(context.Background(), "")
	if got := extract(t, "apibay.org", out); !reflect.DeepEqual(got, []string{"Movie", "Show"}) {
		t.Fatalf("top100 = %v", got)
	}
}

func TestEZTV(t *testing.T) {
	fakeText(t, fixed(`{"torrents":[
		{"hash":"`+strings.Repeat("f", 40)+`","filename":"Show S01E01 1080p","magnet_url":"`+magnet40+`","size_bytes":"1500000000","seeds":50,"peers":5,"date_released_unix":1614202551},
		{"hash":"`+strings.Repeat("e", 40)+`","title":"Other Show 720p"},
		{"hash":"","filename":"no hash"}]}`))
	out, _ := eztvParse(context.Background(), "show")
	rows := ExtractTorrents([]string{"SOURCE -> eztvx.to\n" + out})
	if len(rows) != 2 || *rows[0].MagnetLink != magnet40 || *rows[0].Category != "Video - TV shows" ||
		!strings.HasPrefix(*rows[1].MagnetLink, "magnet:?xt=urn:btih:"+strings.Repeat("e", 40)+"&dn=Other%20Show%20720p") {
		t.Fatalf("rows = %v", rows)
	}
	out, _ = eztvParse(context.Background(), "other 720")
	if got := extract(t, "eztvx.to", out); !reflect.DeepEqual(got, []string{"Other Show 720p"}) {
		t.Fatalf("filtered = %v", got)
	}
	if out, _ := eztvParse(context.Background(), "sample show"); out != "No results" {
		t.Fatalf("no match = %q", out)
	}
}

const uindexRow = `<tr><td class="top-col-rank"><span class="top-rank">1</span></td>` +
	`<td class="sr-col-cat"><a href="top.php?c=2&amp;t=7d" class="sr-cat-badge"> TV</a></td>` +
	`<td class="sr-col-name">` +
	`<a href="magnet:?xt=urn:btih:%s&amp;dn=Name" class="sr-magnet" title="Download Magnet"></a> ` +
	`<a href="/details.php?id=123" class="sr-torrent-link">%s <span class="top-new-badge">NEW</span></a></td>` +
	`<td class="sr-col-size">517.00 MB</td>` +
	`<td class="sr-col-uploaded" title="2.9 days ago">2 hours ago</td>` +
	`<td class="sr-col-seeders"><span class="sr-seed">12,422</span></td>` +
	`<td class="sr-col-leechers"><span class="sr-leech">21,234</span></td></tr>`

func TestUIndex(t *testing.T) {
	origNow := now
	t.Cleanup(func() { now = origNow })
	now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	body := fmt.Sprintf(uindexRow, strings.Repeat("a", 40), "Sample OS 24.04 LTS Desktop") +
		fmt.Sprintf(uindexRow, strings.Repeat("b", 40), "Other Distro") +
		`<tr><td class="top-col-rank"><span class="top-rank">9</span></td><td class="sr-col-name"><a href="/details.php?id=2">No Magnet</a></td></tr>` +
		"<tr><td>not a listing row</td></tr>"
	rows := uindexRows(body)
	want := []string{"Sample OS 24.04 LTS Desktop", "TV", "517.00 MB", "12422", "21234", "N/A", "2027-01-15",
		"magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&dn=Name", "https://uindex.org/details.php?id=123"}
	if len(rows) != 2 || !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("rows = %v", rows)
	}
	if uindexDateOf("2026-01-01") != "2026-01-01" || len(uindexDateOf("3 weeks ago")) != 10 {
		t.Fatal("uindexDateOf fallback")
	}
	fakeText(t, fixed(body))
	out, _ := uindexParse(context.Background(), "sample os lts")
	if got := extract(t, "uindex.org", out); !reflect.DeepEqual(got, []string{"Sample OS 24.04 LTS Desktop"}) {
		t.Fatalf("filtered = %v", got)
	}
}

func TestFitGirl(t *testing.T) {
	xml := `<rss><channel><item>
        <title>Game Repack</title>
        <link>https://fitgirl-repacks.site/game/</link>
        <pubDate>Sat, 08 Aug 2026 18:16:12 +0000</pubDate>
        <description><a href="magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `&amp;dn=game&amp;tr=udp%3A%2F%2Ftracker%2Fannounce">magnet</a></description>
    </item><item><title>No Magnet</title></item></channel></rss>`
	rows := fitgirlRows(xml)
	if len(rows) != 1 || rows[0][0] != "Game Repack" || rows[0][1] != "Games" || strings.Contains(rows[0][7], "&amp;") ||
		rows[0][6] != "2026-08-08T18:16:12+00:00" || rows[0][8] != "https://fitgirl-repacks.site/game/" {
		t.Fatalf("rows = %v", rows)
	}
	var seen []string
	fakeText(t, func(u string, _ url.Values) (string, error) { seen = append(seen, u); return "<rss></rss>", nil })
	fitgirlParse(context.Background(), "assassin creed")
	fitgirlParse(context.Background(), "")
	if seen[0] != "https://fitgirl-repacks.site/?s=assassin%20creed&feed=rss2" || seen[1] != "https://fitgirl-repacks.site/feed/" {
		t.Fatalf("urls = %v", seen)
	}
}

func TestSubsPlease(t *testing.T) {
	body := `{"Show - 1173":{"show":"Show","episode":"1173","page":"show","release_date":"Sun, 09 Aug 2026 16:03:43 +0000",
		"downloads":[{"res":"480","magnet":"magnet:?xt=urn:btih:` + strings.Repeat("b", 40) + `&xl=376124912"},
		{"res":"1080","magnet":"magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `&xl=1500000000"}]},
		"Broken":{"show":"Broken","downloads":[{"res":"480"}]},
		"Another - 2":{"show":"Another","episode":"2","downloads":[{"res":"2160","magnet":"magnet:?xt=urn:btih:aa"}]}}`
	var seen url.Values
	fakeText(t, func(_ string, params url.Values) (string, error) { seen = params; return body, nil })
	out, _ := subspleaseParse(context.Background(), "show")
	if seen.Get("f") != "search" || seen.Get("s") != "show" {
		t.Fatalf("params = %v", seen)
	}
	rows := ExtractTorrents([]string{"SOURCE -> subsplease.org\n" + out})
	if len(rows) != 2 || rows[0].Filename != "Show - 1173 [1080p]" || rows[0].Size != "1.4 GiB" ||
		rows[0].Date != "2026-08-09T16:03:43+00:00" || *rows[0].PageURL != "https://subsplease.org/shows/show/" ||
		rows[1].Filename != "Another - 2 [2160p]" {
		t.Fatalf("rows = %v", rows)
	}
	fakeText(t, func(_ string, params url.Values) (string, error) { seen = params; return "[]", nil })
	if out, _ := subspleaseParse(context.Background(), ""); out != "No results" || seen.Get("f") != "latest" {
		t.Fatalf("latest = %q %v", out, seen)
	}
}

func TestNyaa(t *testing.T) {
	xml := `<rss><channel><item>
        <title>Sample.Show.E1173.1080p.WEBRip.x265</title>
        <guid isPermaLink="true">https://nyaa.si/view/123456</guid>
        <pubDate>Mon, 10 Aug 2026 08:09:35 -0000</pubDate>
        <nyaa:infoHash>` + strings.Repeat("C", 40) + `</nyaa:infoHash>
        <nyaa:category>Anime - English-translated</nyaa:category>
        <nyaa:size>487.9 MiB</nyaa:size>
        <nyaa:seeders>75</nyaa:seeders>
        <nyaa:leechers>3</nyaa:leechers>
        <nyaa:downloads>153</nyaa:downloads>
    </item><item><title>No Hash</title></item><item><nyaa:infoHash>abc</nyaa:infoHash></item></channel></rss>`
	rows := nyaaRSSRows(xml)
	if len(rows) != 1 || rows[0][1] != "Anime - English-translated" || rows[0][2] != "487.9 MiB" || rows[0][3] != "75" ||
		rows[0][5] != "153" || rows[0][6] != "2026-08-10T08:09:35+00:00" || rows[0][8] != "https://nyaa.si/view/123456" ||
		!strings.HasPrefix(rows[0][7], "magnet:?xt=urn:btih:"+strings.Repeat("c", 40)) {
		t.Fatalf("rows = %v", rows)
	}

	tr := `<tr><td><a href="/?c=1_2">cat</a></td>` +
		`<td colspan="2"><a href="/view/123" title="Show S01E01">x</a></td>` +
		`<td class="text-center"><a href="/download/123.torrent">dl</a>` +
		`<a href="magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `&amp;dn=x">m</a></td>` +
		`<td class="text-center">1.5 GiB</td>` +
		`<td class="text-center" data-timestamp="1787497286">2026-08-23 15:01</td>` +
		`<td class="text-center">3,442</td><td class="text-center">91</td><td class="text-center">12954</td></tr>`
	short := `<tr><td colspan="2"><a href="/view/9" title="Short">x</a><a href="magnet:?xt=urn:btih:dd&amp;dn=x">m</a></td><td class="text-center">1 GB</td></tr>`
	var seen url.Values
	var seenURL string
	fakeText(t, func(u string, params url.Values) (string, error) {
		seenURL, seen = u, params
		return "<table>" + tr + short + tr + "</table>", nil
	})
	out, _ := nyaaPopular(context.Background())
	got := ExtractTorrents([]string{"SOURCE -> nyaa.si\n" + out})
	if len(got) != 2 || got[0].Filename != "Show S01E01" || got[0].Size != "1.5 GiB" || got[0].Seeders != 3442 ||
		got[0].Leechers != 91 || got[0].Downloads != "12954" || got[0].Date != "2026-08-23T15:01:26+00:00" ||
		*got[0].PageURL != "https://nyaa.si/view/123" || strings.Contains(*got[0].MagnetLink, "&amp;") {
		t.Fatalf("popular = %v", got)
	}
	if seenURL != nyaaPopularURL || !reflect.DeepEqual(seen, nyaaPopularParams) {
		t.Fatalf("request = %s %v", seenURL, seen)
	}
}

const x1337HTML = `<table class="table-list">
    <tr><td><a href="/torrent/111/1/">Show S01 1080p</a></td>
        <td class="coll-2 seeds">1,234</td>
        <td class="coll-3 leeches">45</td>
        <td class="coll-4 size">1.2 GiB</td></tr>
    <tr><td><a href="/torrent/222/1/">Show S01 720p</a></td>
        <td class="coll-2 seeds">10</td>
        <td class="coll-3 leeches">5</td>
        <td class="coll-4 size">800 MB</td></tr>
    <tr><td><a href="/details/333">Not a torrent link</a></td>
        <td class="coll-2 seeds">1</td><td class="coll-3 leeches">1</td></tr>
</table>`

func TestX1337RowsAndDate(t *testing.T) {
	rows := x1337Rows(x1337HTML)
	if len(rows) != 2 || !reflect.DeepEqual(rows[0], []string{"Show S01 1080p", "/torrent/111/1/", "1.2 GiB", "1234", "45"}) {
		t.Fatalf("rows = %v", rows)
	}
	if x1337Rows("<p>no table here</p>") != nil {
		t.Fatal("no table")
	}
	if got := x1337UploadDate("<strong>Date uploaded</strong><span>Jun. 26th  '26</span>"); got != "2026-06-26" {
		t.Fatalf("date = %s", got)
	}
	if x1337UploadDate("<p>nothing</p>") != "N/A" || x1337UploadDate("<strong>Date uploaded</strong><span>Xxx. 26th  '26</span>") != "N/A" {
		t.Fatal("date fallbacks")
	}
}

func TestX1337FetchAndDetail(t *testing.T) {
	fakeText(t, func(u string, _ url.Values) (string, error) {
		switch {
		case strings.Contains(u, "good"):
			return `<a href="magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `&amp;dn=x" /><strong>Date uploaded</strong><span>Jun. 26th '26</span>`, nil
		case strings.Contains(u, "nomagnet"):
			return "<p>no magnet here</p>", nil
		}
		return "", errors.New("down")
	})
	base, text, err := x1337Fetch(context.Background(), "/good")
	if err != nil || base != "https://1337x.to" || !strings.Contains(text, "magnet:") {
		t.Fatalf("fetch = %s %v", base, err)
	}
	if _, _, err := x1337Fetch(context.Background(), "/bad"); err == nil {
		t.Fatal("all mirrors failing must error")
	}
	if magnet, date, ok := x1337Detail(context.Background(), base, "/good"); !ok || strings.Contains(magnet, "&amp;") || date != "2026-06-26" {
		t.Fatalf("detail = %s %s %v", magnet, date, ok)
	}
	for _, path := range []string{"/bad", "/nomagnet"} {
		if _, _, ok := x1337Detail(context.Background(), base, path); ok {
			t.Fatalf("detail %s should fail", path)
		}
	}
}

func fakeX1337(t *testing.T, pages func(path string) string, detail func(path string) bool) *[]string {
	t.Helper()
	origFetch, origDetail := x1337Fetch, x1337Detail
	t.Cleanup(func() { x1337Fetch, x1337Detail = origFetch, origDetail })
	var mu sync.Mutex
	calls := &[]string{}
	x1337Fetch = func(_ context.Context, path string) (string, string, error) {
		if html := pages(path); html != "" {
			return "https://1337xx.to", html, nil
		}
		return "", "", errors.New("blocked")
	}
	x1337Detail = func(_ context.Context, _ string, path string) (string, string, bool) {
		mu.Lock()
		*calls = append(*calls, path)
		mu.Unlock()
		return magnet40, "2026-06-26", detail(path)
	}
	return calls
}

func TestX1337Parse(t *testing.T) {
	always := func(string) bool { return true }
	fakeX1337(t, func(path string) string {
		if strings.Contains(path, "popular-movies") || strings.Contains(path, "/Movies/") {
			return x1337HTML
		}
		return "<p>no results</p>"
	}, always)
	out, _ := x1337Parse(context.Background(), "", 0)
	got := ExtractTorrents([]string{"SOURCE -> 1337x.to\n" + out})
	if len(got) != 2 || *got[0].Category != "Video - Movies" {
		t.Fatalf("browse = %v", got)
	}
	out, _ = x1337Parse(context.Background(), "the show s01", 0)
	got = ExtractTorrents([]string{"SOURCE -> 1337x.to\n" + out})
	if len(got) != 2 || got[0].Seeders != 1234 || got[0].Date != "2026-06-26" || *got[0].PageURL != "https://1337xx.to/torrent/111/1/" {
		t.Fatalf("query = %v", got)
	}

	calls := fakeX1337(t, func(string) string { return x1337HTML }, always)
	out, _ = x1337Parse(context.Background(), "", 1)
	if len(extract(t, "1337x.to", out)) != 1 || len(*calls) != 1 {
		t.Fatalf("max items should cap detail fetches: %v", *calls)
	}

	fakeX1337(t, func(path string) string { return x1337HTML }, func(path string) bool { return path != "/torrent/222/1/" })
	out, _ = x1337Parse(context.Background(), "show 720p", 0)
	if out != "No results" {
		t.Fatalf("failed details should be skipped: %q", out)
	}

	fakeX1337(t, func(string) string { return "" }, always)
	if out, _ := x1337Parse(context.Background(), "show", 0); out != "No results" {
		t.Fatalf("all fetches failing = %q", out)
	}
}

func TestSearchTorrentsMergesSourcesAndSurvivesFailures(t *testing.T) {
	origSites := Websites
	t.Cleanup(func() { Websites = origSites })
	Websites = []Website{
		{"ok.example", func(context.Context, string) (string, error) {
			return CSVHeader + "\nA;Video;1 GB;1;0;;2026-01-01;m1;", nil
		}},
		{"boom.example", func(context.Context, string) (string, error) { panic("parser bug") }},
		{"err.example", func(context.Context, string) (string, error) { return "", errors.New("down") }},
		{"excluded.example", func(context.Context, string) (string, error) {
			return CSVHeader + "\nB;Video;1 GB;1;0;;2026-01-01;m2;", nil
		}},
	}
	got := SearchTorrents(context.Background(), "q", []string{"ok.example", "boom.example", "err.example"})
	if len(got) != 1 || got[0].Filename != "A" {
		t.Fatalf("got %v", got)
	}
}

func TestPopularTorrentsCachesPerSource(t *testing.T) {
	origSources, origTTL := PopularSources, PopularTTL
	t.Cleanup(func() {
		PopularSources, PopularTTL = origSources, origTTL
		popularMu.Lock()
		popularCache = map[string]popularEntry{}
		popularMu.Unlock()
	})
	var mu sync.Mutex
	fetches := map[string]int{}
	listing := func(name string) PopularFetcher {
		return func(context.Context, int) (string, error) {
			mu.Lock()
			fetches[name]++
			mu.Unlock()
			return CSVHeader + "\n" + name + " low;V;1 GB;1;0;;d;m-" + name + "-1;\n" + name + " high;V;1 GB;9;0;;d;m-" + name + "-2;\n" +
				name + " mid;V;1 GB;5;0;;d;m-" + name + "-3;", nil
		}
	}
	PopularSources = []PopularSource{{"a.example", listing("a")}, {"b.example", listing("b")},
		{"bad.example", func(context.Context, int) (string, error) { return "", errors.New("down") }}}

	got := PopularTorrents(context.Background(), nil, 2)
	var names []string
	for _, t := range got {
		names = append(names, t.Filename)
	}
	if !reflect.DeepEqual(names, []string{"a high", "b high", "a mid", "b mid"}) {
		t.Fatalf("names = %v", names)
	}
	PopularTorrents(context.Background(), nil, 1) // covered by the per_source=2 fetch
	if fetches["a"] != 1 {
		t.Fatalf("fresh cache should be reused: %v", fetches)
	}
	PopularTorrents(context.Background(), nil, 3) // needs a fuller listing
	if fetches["a"] != 2 {
		t.Fatalf("larger request should refetch: %v", fetches)
	}

	PopularTTL = 0 // everything is stale: served from cache, refreshed in background
	PopularTorrents(context.Background(), []string{"a.example"}, 3)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := fetches["a"]
		mu.Unlock()
		popularMu.Lock()
		refreshing := popularRefreshing["a.example"]
		popularMu.Unlock()
		if n == 3 && !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not run: %v", fetches)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPopularCovers(t *testing.T) {
	for _, c := range []struct {
		used, requested int
		want            bool
	}{{0, 5, true}, {0, 0, true}, {5, 0, false}, {5, 3, true}, {3, 5, false}} {
		if got := popularCovers(c.used, c.requested); got != c.want {
			t.Errorf("popularCovers(%d, %d) = %v", c.used, c.requested, got)
		}
	}
}

func TestMain(m *testing.M) {
	trackersLoaded.Do(func() {}) // keep tests off the network
	os.Exit(m.Run())
}

func TestEnsureTrackersMergesRemoteList(t *testing.T) {
	origTrackers := trackers
	t.Cleanup(func() { trackers = origTrackers })
	trackersLoaded = sync.Once{}
	t.Cleanup(func() { trackersLoaded = sync.Once{}; trackersLoaded.Do(func() {}) })
	fakeText(t, fixed("udp://tracker.remote.example:1337/announce\n"))
	EnsureTrackers(context.Background())
	if !strings.Contains(BuildMagnet("a", "b"), "tracker.remote.example") {
		t.Fatal("remote trackers should be merged")
	}
}
