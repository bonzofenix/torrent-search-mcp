package torrent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var magnet40 = "magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&dn=x"

func format(t *testing.T, overrides map[string]string) Torrent {
	t.Helper()
	data := map[string]string{
		"filename":    "  Show S01E01 1080p  ",
		"size":        "1.2 GiB",
		"seeders":     "10",
		"leechers":    "5",
		"date":        "2026-01-01T12:00:00",
		"magnet_link": magnet40,
	}
	source := "nyaa.si"
	for k, v := range overrides {
		if k == "source" {
			source = v
			continue
		}
		data[k] = v
	}
	out, err := Format(data, source)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return out
}

func TestFormatDefaults(t *testing.T) {
	got := format(t, nil)
	sum := sha256.Sum256([]byte(magnet40))
	if got.Filename != "Show S01E01 1080p" || got.Seeders != 10 || got.Leechers != 5 {
		t.Fatalf("unexpected torrent: %+v", got)
	}
	if got.Downloads != "N/A" || got.Category != nil || got.Uploader != nil {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	if want := "nyaa.si-" + hex.EncodeToString(sum[:])[:10]; got.ID != want {
		t.Fatalf("id = %q, want %q", got.ID, want)
	}
}

func TestFormatMissingMagnetUsesNoneID(t *testing.T) {
	if got := format(t, map[string]string{"magnet_link": ""}); got.ID != "nyaa.si-none" {
		t.Fatalf("id = %q", got.ID)
	}
}

func TestFormatNormalizesPageURL(t *testing.T) {
	for value, want := range map[string]string{
		"":                            "",
		"javascript:alert(1)":         "",
		"//example.com/torrent":       "",
		"https://example.com/torrent": "https://example.com/torrent",
	} {
		if got := Deref(format(t, map[string]string{"page_url": value}).PageURL); got != want {
			t.Errorf("page_url(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestFormatRejectsBadRows(t *testing.T) {
	if _, err := Format(map[string]string{"filename": "x", "size": "1", "date": "d", "seeders": "nope"}, "s"); err == nil {
		t.Fatal("non-numeric seeders must fail")
	}
	if _, err := Format(map[string]string{"filename": "x", "size": "1"}, "s"); err == nil {
		t.Fatal("missing date must fail")
	}
}

// Golden strings produced by the Python server's str(Torrent).
func TestStringMatchesPython(t *testing.T) {
	cases := []struct {
		data   map[string]string
		source string
		want   string
	}{
		{
			map[string]string{"filename": " Show S01E01 1080p ", "category": "Anime", "size": "1.2 GiB", "seeders": "10", "leechers": "5", "downloads": "153", "date": "2026-01-01T12:00:00", "magnet_link": magnet40, "page_url": "https://nyaa.si/view/1"},
			"nyaa.si",
			`{'id': 'nyaa.si-254b50d68d', 'filename': 'Show S01E01 1080p', 'category': 'Anime', 'size': '1.2 GiB', 'seeders': 10, 'leechers': 5, 'downloads': '153', 'date': '2026-01-01T12:00:00', 'magnet_link': 'magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&dn=x', 'page_url': 'https://nyaa.si/view/1', 'source': 'nyaa.si'}`,
		},
		{
			map[string]string{"filename": "It's \"quoted\" \\ tab\there", "category": "Video", "size": "N/A", "seeders": "", "leechers": "0", "downloads": "", "date": "N/A", "magnet_link": "", "page_url": "javascript:x"},
			"weird source/ünïcode",
			`{'id': 'd2VpcmQgc291cmNlL8O8bsOvY29kZQ-none', 'filename': 'It\'s "quoted" \\ tab\there', 'category': 'Video', 'size': 'N/A', 'seeders': 0, 'leechers': 0, 'downloads': 'N/A', 'date': 'N/A', 'magnet_link': '', 'source': 'weird source/ünïcode'}`,
		},
		{
			map[string]string{"filename": "L'été – 日本\x00​\U0001F600 ", "category": "Video", "size": "1 B", "seeders": "1", "leechers": "2", "downloads": "N/A", "date": "x", "magnet_link": "m", "page_url": ""},
			"1337x.to",
			"{'id': '1337x.to-62c66a7a5d', 'filename': \"L'été – 日本\\x00\\u200b\U0001F600\", 'category': 'Video', 'size': '1 B', 'seeders': 1, 'leechers': 2, 'downloads': 'N/A', 'date': 'x', 'magnet_link': 'm', 'source': '1337x.to'}",
		},
	}
	for _, c := range cases {
		got, err := Format(c.data, c.source)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != c.want {
			t.Errorf("String()\n got %s\nwant %s", got.String(), c.want)
		}
	}
}

func TestStringOmitsNilMagnet(t *testing.T) {
	got := format(t, nil)
	got.MagnetLink = nil
	if strings.Contains(got.String(), "magnet_link") {
		t.Fatalf("magnet_link should be omitted: %s", got)
	}
}

// Ids minted by the Python server must decode here.
func TestExtractInfoDecodesPythonIDs(t *testing.T) {
	for encoded, want := range map[string]string{
		"4AP9vsu40hnULw8H5Ky3xGkUCK":    "sample show",
		"AM04I8buPhZ":                   "",
		"193cnmEFefUccN500R7PfCADppHiA": "héllo wörld",
	} {
		query, maxItems, source, ref, err := ExtractInfo(encoded + "-20-nyaa.si-a1035a5e20")
		if err != nil || query != want || maxItems != 20 || source != "nyaa.si" || ref != "a1035a5e20" {
			t.Errorf("ExtractInfo(%s) = %q %d %q %q %v", encoded, query, maxItems, source, ref, err)
		}
	}
}

func TestPrependAndExtractInfoRoundTrip(t *testing.T) {
	got := format(t, map[string]string{"source": "weird source/ünïcode"})
	got.PrependInfo("héllo wörld", 10)
	if strings.ContainsAny(got.ID, "/ ") {
		t.Fatalf("id is not URL-safe: %q", got.ID)
	}
	query, maxItems, _, _, err := ExtractInfo(got.ID)
	if err != nil || query != "héllo wörld" || maxItems != 10 {
		t.Fatalf("round trip = %q %d %v", query, maxItems, err)
	}
}

func TestExtractInfoInvalid(t *testing.T) {
	for _, id := range []string{"bogus", "abc-x-src-ref", "!!!-1-src-ref", "0-1-src-ref"} {
		if _, _, _, _, err := ExtractInfo(id); err == nil {
			t.Errorf("ExtractInfo(%q) should fail", id)
		}
	}
}

func TestBase62LeadingZeros(t *testing.T) {
	for _, in := range [][]byte{{}, {0}, {0, 0, 1}, make([]byte, 70), append(make([]byte, 62), 7, 9)} {
		out, err := decodeBytes(encodeBytes(in))
		if err != nil || string(out) != string(in) {
			t.Errorf("round trip %v -> %q -> %v %v", in, encodeBytes(in), out, err)
		}
	}
}

func TestUnmarshalJSON(t *testing.T) {
	var rows []Torrent
	body := `[{"id":"a-1","filename":"F","category":null,"size":"1 GB","seeders":"3","leechers":2,"downloads":7,"date":"d","magnet_link":"m","page_url":"ftp://x","uploader":null,"source":"yts.vg"},
	{"id":"a-2","filename":"G","size":"1 GB","seeders":1,"leechers":0,"downloads":"N/A","date":"d","page_url":"https://x.org/1"}]`
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	if want := `{'id': 'a-1', 'filename': 'F', 'size': '1 GB', 'seeders': 3, 'leechers': 2, 'downloads': 7, 'date': 'd', 'magnet_link': 'm', 'source': 'yts.vg'}`; rows[0].String() != want {
		t.Fatalf("got %s", rows[0])
	}
	if Deref(rows[1].PageURL) != "https://x.org/1" || rows[1].Downloads != "N/A" {
		t.Fatalf("got %s", rows[1])
	}
	for _, bad := range []string{`{"id":"x"}`, `{"id":"x","filename":"f","size":"s","date":"d","seeders":"a","leechers":1}`, `{"id":"x","filename":"f","size":"s","date":"d","seeders":1}`, `{"id":"x","filename":"f","size":"s","date":"d","seeders":1,"leechers":1,"downloads":1.5}`, `[]`} {
		var one Torrent
		if err := json.Unmarshal([]byte(bad), &one); err == nil {
			t.Errorf("Unmarshal(%s) should fail", bad)
		}
	}
}

func TestCacheUpdateGetRefreshAndClean(t *testing.T) {
	cache := NewCache(60*time.Second, 5000)
	clock := int64(1000)
	cache.now = func() int64 { return clock }
	first := format(t, map[string]string{"magnet_link": "b"})
	second := format(t, map[string]string{"magnet_link": "c"})
	cache.Update([]Torrent{first, second})
	if got, ok := cache.Get(first.ID); !ok || got.ID != first.ID {
		t.Fatal("first should be cached")
	}
	if _, ok := cache.Get("missing"); ok {
		t.Fatal("missing should miss")
	}
	clock += 59
	cache.Get(first.ID) // refresh
	clock += 30
	cache.Clean()
	if _, ok := cache.Get(first.ID); !ok {
		t.Fatal("refreshed entry should survive")
	}
	if cache.Len() != 1 {
		t.Fatalf("stale entry should be cleaned, len=%d", cache.Len())
	}
}

func TestCacheEnforcesMaxSize(t *testing.T) {
	cache := NewCache(time.Minute, 2)
	ids := []string{}
	for _, m := range []string{"b", "c", "d"} {
		torrent := format(t, map[string]string{"magnet_link": m})
		ids = append(ids, torrent.ID)
		cache.Update([]Torrent{torrent})
	}
	if _, ok := cache.Get(ids[0]); ok {
		t.Fatal("oldest entry should be evicted")
	}
	if cache.Len() != 2 {
		t.Fatalf("len = %d", cache.Len())
	}
	unbounded := NewCache(time.Minute, 0)
	for _, m := range []string{"1", "2", "3", "4", "5"} {
		unbounded.Update([]Torrent{format(t, map[string]string{"magnet_link": m})})
	}
	if unbounded.Len() != 5 {
		t.Fatalf("len = %d", unbounded.Len())
	}
}
