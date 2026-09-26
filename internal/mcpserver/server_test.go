package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bonzofenix/torrent-search-mcp/internal/search"
	"github.com/bonzofenix/torrent-search-mcp/internal/torrent"
)

func mk(t *testing.T, name, source string, seeders int) torrent.Torrent {
	t.Helper()
	out, err := torrent.Format(map[string]string{
		"filename": name, "category": "Video", "size": "1 GB", "date": "d", "seeders": string(rune('0' + seeders)),
		"leechers": "0", "magnet_link": "magnet:?" + name,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFormatTorrentsHidesMagnetsUnlessIncluded(t *testing.T) {
	tools := &Tools{}
	found := []torrent.Torrent{mk(t, "a", "nyaa.si", 1)}
	if got := tools.formatTorrents(found, false); strings.Contains(got, "magnet_link") {
		t.Fatalf("magnets should be hidden: %s", got)
	}
	tools.IncludeLinks = true
	if got := tools.formatTorrents(found, false); !strings.Contains(got, "'magnet_link': 'magnet:?a'") {
		t.Fatalf("magnets should be shown: %s", got)
	}
	if found[0].MagnetLink == nil {
		t.Fatal("formatting must not strip the caller's magnet")
	}
}

func TestFormatTorrentsGroupsBySource(t *testing.T) {
	found := []torrent.Torrent{
		mk(t, "n1", "nyaa.si", 1), mk(t, "t1", "thepiratebay.org", 1), mk(t, "z", "zzz.example", 1),
		mk(t, "t9", "thepiratebay.org", 9), mk(t, "a", "aaa.example", 1),
	}
	noSource := mk(t, "u", "x", 1)
	noSource.Source = nil
	found = append(found, noSource)
	got := (&Tools{}).formatTorrents(found, true)
	var headers []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "== ") {
			headers = append(headers, line)
		}
	}
	want := []string{"== thepiratebay.org ==", "== nyaa.si ==", "== aaa.example ==", "== unknown ==", "== zzz.example =="}
	if strings.Join(headers, "|") != strings.Join(want, "|") {
		t.Fatalf("headers = %v", headers)
	}
	if !strings.Contains(got, "== thepiratebay.org ==\n{'id': 'thepiratebay.org-") || strings.Index(got, "'t9'") > strings.Index(got, "'t1'") {
		t.Fatalf("sections should rank by seeders:\n%s", got)
	}
	if !strings.Contains(got, "\n\n== nyaa.si ==") {
		t.Fatalf("blocks should be blank-line separated:\n%s", got)
	}
}

func remote(t *testing.T, handler http.HandlerFunc) *Tools {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Tools{api: search.New(), APIBaseURL: srv.URL, http: srv.Client()}
}

const row = `{"id":"q-20-nyaa.si-abc","filename":"Remote","category":"Anime","size":"1 GB","seeders":3,"leechers":1,"downloads":"N/A","date":"d","magnet_link":"magnet:?r","page_url":null,"uploader":null,"source":"nyaa.si"}`

func TestRemoteSearchPopularSourcesAndGet(t *testing.T) {
	var seen []string
	tools := remote(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/torrent/search":
			if r.URL.Query().Get("query") == "none" {
				io.WriteString(w, "[]")
				return
			}
			io.WriteString(w, "["+row+"]")
		case "/torrent/popular":
			io.WriteString(w, "["+row+"]")
		case "/sources":
			io.WriteString(w, `["nyaa.si","yts.vg"]`)
		case "/torrent/known":
			io.WriteString(w, `"magnet:?xt=urn:btih:abc"`)
		case "/torrent/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	want := `{'id': 'q-20-nyaa.si-abc', 'filename': 'Remote', 'category': 'Anime', 'size': '1 GB', 'seeders': 3, 'leechers': 1, 'downloads': 'N/A', 'date': 'd', 'source': 'nyaa.si'}`
	if got, err := tools.SearchTorrents(ctx, "sample show"); err != nil || got != want {
		t.Fatalf("search = %q %v", got, err)
	}
	if got, _ := tools.SearchTorrents(ctx, "none"); got != "No torrents found" {
		t.Fatalf("empty search = %q", got)
	}
	if got, _ := tools.PopularTorrents(ctx, 5); got != "== nyaa.si ==\n"+want {
		t.Fatalf("popular = %q", got)
	}
	if got, _ := tools.AvailableSources(ctx); strings.Join(got, ",") != "nyaa.si,yts.vg" {
		t.Fatalf("sources = %v", got)
	}
	if got, _ := tools.GetTorrent(ctx, "known"); got != "magnet:?xt=urn:btih:abc" {
		t.Fatalf("get = %q", got)
	}
	if got, _ := tools.GetTorrent(ctx, "missing"); got != "Torrent not found" {
		t.Fatalf("get missing = %q", got)
	}
	if _, err := tools.GetTorrent(ctx, "broken"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("get broken = %v", err)
	}
	if seen[0] != "POST /torrent/search?max_items=20&query=sample+show" || seen[2] != "GET /torrent/popular?per_source=5" {
		t.Fatalf("requests = %v", seen)
	}
}

func TestAuthorizeWebapp(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TORRENT_SEARCH_API_KEY", "")
	if got, _ := (&Tools{}).AuthorizeWebapp(ctx, "c", "1"); !strings.HasPrefix(got, "Webapp authorization is disabled") {
		t.Fatalf("no key = %q", got)
	}
	t.Setenv("TORRENT_SEARCH_API_KEY", "secret")
	if got, _ := (&Tools{}).AuthorizeWebapp(ctx, "c", "1"); !strings.HasPrefix(got, "authorize_webapp requires TORRENT_SEARCH_API_URL") {
		t.Fatalf("no url = %q", got)
	}
	status := http.StatusOK
	tools := remote(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/telegram/auth/register" || r.URL.Query().Get("code") != "ABC" || r.URL.Query().Get("chat_id") != "42" ||
			r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s %v", r.URL, r.Header)
		}
		w.WriteHeader(status)
	})
	for code, want := range map[int]string{
		http.StatusOK:           "Access granted.",
		http.StatusNotFound:     "Unknown or expired pairing code.",
		http.StatusUnauthorized: "Authorization rejected:",
	} {
		status = code
		if got, err := tools.AuthorizeWebapp(ctx, "ABC", "42"); err != nil || !strings.HasPrefix(got, want) {
			t.Errorf("status %d = %q %v", code, got, err)
		}
	}
	status = http.StatusBadGateway
	if _, err := tools.AuthorizeWebapp(ctx, "ABC", "42"); err == nil {
		t.Error("unexpected statuses must error")
	}
}

func TestForwardTorrent(t *testing.T) {
	ctx := context.Background()
	in := ForwardInput{Filename: "F", MagnetLink: "magnet:?x", ChatID: "42"}
	t.Setenv("TORRENT_SEARCH_API_KEY", "")
	if got, _ := (&Tools{}).ForwardTorrent(ctx, in); !strings.HasPrefix(got, "Telegram forwarding is disabled: set") {
		t.Fatalf("no key = %q", got)
	}
	t.Setenv("TORRENT_SEARCH_API_KEY", "secret")
	if got, _ := (&Tools{}).ForwardTorrent(ctx, in); !strings.HasPrefix(got, "forward_torrent requires TORRENT_SEARCH_API_URL") {
		t.Fatalf("no url = %q", got)
	}
	status := http.StatusOK
	var body string
	tools := remote(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		if r.URL.Path != "/forward_telegram" || r.URL.Query().Get("chat_id") != "42" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.WriteHeader(status)
	})
	for code, want := range map[int]string{
		http.StatusOK:                 "Torrent forwarded to Telegram.",
		http.StatusBadRequest:         "Forward rejected: a valid owner chat_id",
		http.StatusUnauthorized:       "Forward rejected: TORRENT_SEARCH_API_KEY",
		http.StatusTooManyRequests:    "Too many forwards",
		http.StatusServiceUnavailable: "Telegram forwarding is disabled on the API server",
	} {
		status = code
		if got, err := tools.ForwardTorrent(ctx, in); err != nil || !strings.HasPrefix(got, want) {
			t.Errorf("status %d = %q %v", code, got, err)
		}
	}
	if body != `{"filename":"F","magnet_link":"magnet:?x","size":null,"seeders":null}` {
		t.Fatalf("body = %s", body)
	}
	status = http.StatusInternalServerError
	if _, err := tools.ForwardTorrent(ctx, in); err == nil {
		t.Error("unexpected statuses must error")
	}
}

func TestTorrentWebapp(t *testing.T) {
	t.Setenv("WEBUI_URL", "")
	if got := (&Tools{}).TorrentWebapp(); !strings.HasPrefix(got, "webapp URL not configured.") {
		t.Fatalf("unset = %q", got)
	}
	t.Setenv("WEBUI_URL", "https://torrents.example/")
	if got := (&Tools{}).TorrentWebapp(); !strings.HasPrefix(got, "Torrent Search webapp: https://torrents.example\n") {
		t.Fatalf("set = %q", got)
	}
}

// connect runs the server over in-memory transports, as an MCP client would.
func connect(t *testing.T, tools *Tools) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := NewServer(tools, "test").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestServerExposesPythonToolSurface(t *testing.T) {
	cs := connect(t, &Tools{api: search.New()})
	ctx := context.Background()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string][]string{}
	for _, tool := range list.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		json.Unmarshal(schema, &s)
		for name := range s.Properties {
			args[tool.Name] = append(args[tool.Name], name)
		}
		if tool.Description == "" || tool.Title == "" {
			t.Errorf("%s lacks a description or title", tool.Name)
		}
	}
	want := map[string]int{"available_sources": 0, "search_torrents": 2, "popular_torrents": 1, "get_torrent": 1,
		"authorize_webapp": 2, "forward_torrent": 5, "torrent_webapp": 0}
	if len(args) > len(want) || len(list.Tools) != len(want) {
		t.Fatalf("tools = %v", args)
	}
	for name, n := range want {
		if len(args[name]) != n {
			t.Errorf("%s args = %v", name, args[name])
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "available_sources"})
	if err != nil || res.IsError {
		t.Fatalf("available_sources = %v %v", res, err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, `["nyaa.si","yts.vg","thepiratebay.org"`) {
		t.Fatalf("text = %s", text)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_torrent", Arguments: map[string]any{"torrent_id": "garbage"}})
	if err != nil || res.Content[0].(*mcp.TextContent).Text != "Torrent not found" {
		t.Fatalf("get_torrent = %v %v", res, err)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if string(structured) != `{"result":"Torrent not found"}` {
		t.Fatalf("structured = %s", structured)
	}
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "popular_torrents", Arguments: map[string]any{"per_source": 0}})
	if !res.IsError {
		t.Fatal("per_source below 1 must be rejected")
	}
}

func TestToolPanicsBecomeErrors(t *testing.T) {
	// A nil API makes the standalone search panic; the session must survive.
	cs := connect(t, &Tools{})
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_torrents", Arguments: map[string]any{"user_intent": "x", "query": "y"}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "internal error in search_torrents") {
		t.Fatalf("panic result = %v %v", res, err)
	}
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "torrent_webapp"}); err != nil || res.IsError {
		t.Fatalf("session should survive a panicking tool: %v %v", res, err)
	}
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "available_sources"}); err != nil || !res.IsError {
		t.Fatalf("available_sources panic = %v %v", res, err)
	}
}
