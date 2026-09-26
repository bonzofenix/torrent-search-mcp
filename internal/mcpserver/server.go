// Package mcpserver ports torrent_search/mcp_server.py: the seven MCP tools,
// with the same names, arguments, descriptions and text output.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bonzofenix/torrent-search-mcp/internal/search"
	"github.com/bonzofenix/torrent-search-mcp/internal/torrent"
)

// SourceOrder ranks the popular sections by traffic (TorrentFreak 2026
// ranking), using the public display domains.
var SourceOrder = []string{
	"thepiratebay.org",
	"1337x.to",
	"uindex.org",
	"eztvx.to",
	"yts.vg",
	"nyaa.si",
	"fitgirl-repacks.site",
	"subsplease.org",
}

func sourceRank(source string) int {
	for i, s := range SourceOrder {
		if s == source {
			return i
		}
	}
	return len(SourceOrder)
}

// Tools holds the tool implementations and their configuration.
type Tools struct {
	api *search.API
	// IncludeLinks exposes magnet links in listings (INCLUDE_LINKS=true).
	// Magnets are always kept in the backend cache either way.
	IncludeLinks bool
	// APIBaseURL switches to remote mode: when TORRENT_SEARCH_API_URL is set
	// (e.g. http://api:8000), the tools proxy the REST API instead of
	// scraping locally. Empty = standalone.
	APIBaseURL string
	http       *http.Client
}

// NewTools reads the same environment the Python server reads at import.
func NewTools() *Tools {
	return &Tools{
		api:          search.New(),
		IncludeLinks: strings.ToLower(os.Getenv("INCLUDE_LINKS")) == "true",
		APIBaseURL:   strings.TrimRight(os.Getenv("TORRENT_SEARCH_API_URL"), "/"),
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// Remote REST API
// ---------------------------------------------------------------------------

// apiError is a non-2xx REST answer (httpx raise_for_status).
type apiError struct {
	status int
	method string
	path   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s failed: HTTP %d %s", e.method, e.path, e.status, http.StatusText(e.status))
}

// apiDo sends a request to the REST API and returns the status and body;
// only transport failures are errors.
func (t *Tools) apiDo(ctx context.Context, method, path string, params url.Values, body any, bearer string) (int, []byte, error) {
	u := t.APIBaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// apiJSON performs a request, requires a 2xx answer and decodes its JSON.
func (t *Tools) apiJSON(ctx context.Context, method, path string, params url.Values, out any) error {
	status, data, err := t.apiDo(ctx, method, path, params, nil, "")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &apiError{status, method, path}
	}
	return json.Unmarshal(data, out)
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

func (t *Tools) torrentLine(tr torrent.Torrent) string {
	if !t.IncludeLinks {
		tr.MagnetLink = nil
	}
	return tr.String()
}

func (t *Tools) formatTorrents(found []torrent.Torrent, bySource bool) string {
	if !bySource {
		lines := make([]string, len(found))
		for i, tr := range found {
			lines[i] = t.torrentLine(tr)
		}
		return strings.Join(lines, "\n")
	}
	groups := map[string][]torrent.Torrent{}
	for _, tr := range found {
		source := torrent.Deref(tr.Source)
		if source == "" {
			source = "unknown"
		}
		groups[source] = append(groups[source], tr)
	}
	sources := make([]string, 0, len(groups))
	for s := range groups {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool {
		ri, rj := sourceRank(sources[i]), sourceRank(sources[j])
		if ri != rj {
			return ri < rj
		}
		return sources[i] < sources[j]
	})
	blocks := make([]string, 0, len(sources))
	for _, source := range sources {
		lines := []string{"== " + source + " =="}
		// Sections read best with the healthiest swarms first.
		ranked := groups[source]
		sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Seeders > ranked[j].Seeders })
		for _, tr := range ranked {
			lines = append(lines, t.torrentLine(tr))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// AvailableSources gets the list of available torrent sources.
func (t *Tools) AvailableSources(ctx context.Context) ([]string, error) {
	if t.APIBaseURL != "" {
		var sources []string
		if err := t.apiJSON(ctx, http.MethodGet, "/sources", nil, &sources); err != nil {
			return nil, err
		}
		return sources, nil
	}
	return t.api.AvailableSources(), nil
}

// SearchTorrents runs search_torrents.
func (t *Tools) SearchTorrents(ctx context.Context, query string) (string, error) {
	log.Printf("INFO: Searching for torrents: %s", query)
	var found []torrent.Torrent
	if t.APIBaseURL != "" {
		params := url.Values{"query": {query}, "max_items": {"20"}}
		if err := t.apiJSON(ctx, http.MethodPost, "/torrent/search", params, &found); err != nil {
			return "", err
		}
	} else {
		found = t.api.SearchTorrents(ctx, query, 20, 0)
	}
	if len(found) == 0 {
		return "No torrents found", nil
	}
	return t.formatTorrents(found, false), nil
}

// PopularTorrents runs popular_torrents.
func (t *Tools) PopularTorrents(ctx context.Context, perSource int) (string, error) {
	log.Printf("INFO: Fetching popular torrents (per_source=%d)", perSource)
	var found []torrent.Torrent
	if t.APIBaseURL != "" {
		params := url.Values{"per_source": {fmt.Sprint(perSource)}}
		if err := t.apiJSON(ctx, http.MethodGet, "/torrent/popular", params, &found); err != nil {
			return "", err
		}
	} else {
		found = t.api.PopularTorrents(ctx, perSource)
	}
	if len(found) == 0 {
		return "No torrents found", nil
	}
	return t.formatTorrents(found, true), nil
}

// GetTorrent runs get_torrent.
func (t *Tools) GetTorrent(ctx context.Context, torrentID string) (string, error) {
	log.Printf("INFO: Getting magnet link for torrent: %s", torrentID)
	if t.APIBaseURL != "" {
		// The REST endpoint answers application/json with a JSON string,
		// so decode it instead of returning the quoted body.
		var magnet any
		err := t.apiJSON(ctx, http.MethodGet, "/torrent/"+url.PathEscape(torrentID), nil, &magnet)
		if apiErr, ok := err.(*apiError); ok && apiErr.status == http.StatusNotFound {
			return "Torrent not found", nil
		}
		if err != nil {
			return "", err
		}
		if s, ok := magnet.(string); ok {
			return s, nil
		}
		out, _ := json.Marshal(magnet)
		return string(out), nil
	}
	if magnet, ok := t.api.GetTorrent(ctx, torrentID); ok {
		return magnet, nil
	}
	return "Torrent not found", nil
}

// AuthorizeWebapp runs authorize_webapp.
func (t *Tools) AuthorizeWebapp(ctx context.Context, code, chatID string) (string, error) {
	log.Printf("INFO: Authorizing webapp access for chat %s", chatID)
	secret := os.Getenv("TORRENT_SEARCH_API_KEY")
	if secret == "" {
		return "Webapp authorization is disabled: set TORRENT_SEARCH_API_KEY " +
			"(same value on the REST API server) to enable pairing approvals.", nil
	}
	if t.APIBaseURL == "" {
		return "authorize_webapp requires TORRENT_SEARCH_API_URL: pairing " +
			"state lives on the REST API server that serves the webapp.", nil
	}
	status, _, err := t.apiDo(ctx, http.MethodPost, "/telegram/auth/register",
		url.Values{"code": {code}, "chat_id": {chatID}}, nil, secret)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusNotFound:
		return "Unknown or expired pairing code. Ask the user for the current code shown in the gate.", nil
	case status == http.StatusUnauthorized:
		return "Authorization rejected: TORRENT_SEARCH_API_KEY does not match the API server.", nil
	case status < 200 || status >= 300:
		return "", &apiError{status, http.MethodPost, "/telegram/auth/register"}
	}
	return "Access granted. Tell the user to go back to the webapp tab: the " +
		"browser will confirm the pairing there within a few seconds and " +
		"remember it. Until they return to the site, authentication is not " +
		"complete.", nil
}

// ForwardInput is the forward_torrent payload.
type ForwardInput struct {
	Filename   string  `json:"filename" jsonschema:"Exact torrent filename shown in the search results."`
	MagnetLink string  `json:"magnet_link" jsonschema:"Magnet link of the torrent (from get_torrent, or from a search run with INCLUDE_LINKS=true)."`
	ChatID     string  `json:"chat_id" jsonschema:"The owner's Telegram chat id the torrent is sent to."`
	Size       *string `json:"size,omitempty" jsonschema:"Optional human-readable size (e.g. '1.2 GiB')."`
	Seeders    *int    `json:"seeders,omitempty" jsonschema:"Optional seeder count."`
}

// ForwardTorrent runs forward_torrent.
func (t *Tools) ForwardTorrent(ctx context.Context, in ForwardInput) (string, error) {
	log.Printf("INFO: Forwarding torrent to Telegram chat %s", in.ChatID)
	secret := os.Getenv("TORRENT_SEARCH_API_KEY")
	if secret == "" {
		return "Telegram forwarding is disabled: set TORRENT_SEARCH_API_KEY " +
			"(same value on the REST API server) to enable forwards.", nil
	}
	if t.APIBaseURL == "" {
		return "forward_torrent requires TORRENT_SEARCH_API_URL: forwarding " +
			"uses the REST API server that owns the Telegram bot.", nil
	}
	body := struct {
		Filename   string  `json:"filename"`
		MagnetLink string  `json:"magnet_link"`
		Size       *string `json:"size"`
		Seeders    *int    `json:"seeders"`
	}{in.Filename, in.MagnetLink, in.Size, in.Seeders}
	status, _, err := t.apiDo(ctx, http.MethodPost, "/forward_telegram", url.Values{"chat_id": {in.ChatID}}, body, secret)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusBadRequest:
		return "Forward rejected: a valid owner chat_id is required.", nil
	case status == http.StatusUnauthorized:
		return "Forward rejected: TORRENT_SEARCH_API_KEY does not match the API server.", nil
	case status == http.StatusTooManyRequests:
		return "Too many forwards for this chat (20/min). Try again in a minute.", nil
	case status == http.StatusServiceUnavailable:
		return "Telegram forwarding is disabled on the API server " +
			"(TELEGRAM_BOT_TOKEN not configured).", nil
	case status < 200 || status >= 300:
		return "", &apiError{status, http.MethodPost, "/forward_telegram"}
	}
	return "Torrent forwarded to Telegram. Tell the user it is in their chat.", nil
}

// TorrentWebapp runs torrent_webapp.
func (t *Tools) TorrentWebapp() string {
	u := strings.TrimRight(os.Getenv("WEBUI_URL"), "/")
	if u == "" {
		return "webapp URL not configured. Set WEBUI_URL (e.g. http://localhost:8000 " +
			"or a public URL) to advertise the webapp."
	}
	return "Torrent Search webapp: " + u + "\n" +
		"A multi-source torrent search interface with per-site popular tiles, " +
		"magnet links, and a Telegram relay for sending torrents to the owner's chat.\n" +
		"Access is pairing-gated: on first visit the site shows a one-time pairing code. " +
		"Ask the user for that code, then call the authorize_webapp tool with it " +
		"(and your Telegram chat id) to grant the browser permanent access. " +
		"Codes expire after 5 minutes."
}

// ---------------------------------------------------------------------------
// MCP registration
// ---------------------------------------------------------------------------

type textResult struct {
	Result string `json:"result"`
}

type listResult struct {
	Result []string `json:"result"`
}

type noArgs struct{}

type searchArgs struct {
	UserIntent string `json:"user_intent" jsonschema:"User's overall intention (e.g. 'latest episode of Sample Show')."`
	Query      string `json:"query" jsonschema:"Optimized search keywords, lowercase and space-separated. Strip generic terms (movie, torrent, download), filler words (the, a, of) and technical tags (1080p, h265, bluray) unless explicitly requested. TV shows: 'name sXXeYY' for episodes, 'name sXX' for seasons. Add 'multi' only if a multi-language version is requested."`
}

type popularArgs struct {
	PerSource int `json:"per_source,omitempty" jsonschema:"How many top results to keep per source (default 20)."`
}

type getTorrentArgs struct {
	TorrentID string `json:"torrent_id" jsonschema:"Torrent ID returned by a previous search_torrents or popular_torrents call."`
}

type authorizeArgs struct {
	Code   string `json:"code" jsonschema:"Pairing code shown in the webapp 'Telegram Access' gate."`
	ChatID string `json:"chat_id" jsonschema:"The owner's Telegram chat id the webapp access is bound to."`
}

// guard turns a panicking tool into an error result: the process must
// outlive any single bad call.
func guard(name string, err *error) {
	if r := recover(); r != nil {
		log.Printf("ERROR: tool %s panicked: %v\n%s", name, r, debug.Stack())
		*err = fmt.Errorf("internal error in %s: %v", name, r)
	}
}

// textTool registers a tool whose result is a single string, returned both
// as text content and as {"result": ...} structured content like FastMCP.
func textTool[In any](s *mcp.Server, tool *mcp.Tool, fn func(context.Context, In) (string, error)) {
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out textResult, err error) {
		defer guard(tool.Name, &err)
		text, err := fn(ctx, in)
		if err != nil {
			return nil, textResult{}, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, textResult{text}, nil
	})
}

func mustSchema[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	return schema
}

// NewServer builds the MCP server exposing the tools.
func NewServer(t *Tools, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "Torrent Search Tools", Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "available_sources",
		Title:       "Available Sources",
		Description: "Get the list of available torrent sources.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (res *mcp.CallToolResult, out listResult, err error) {
		defer guard("available_sources", &err)
		sources, err := t.AvailableSources(ctx)
		if err != nil {
			return nil, listResult{}, err
		}
		text, _ := json.Marshal(sources)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, listResult{sources}, nil
	})

	textTool(s, &mcp.Tool{
		Name:  "search_torrents",
		Title: "Search Torrents",
		Description: `Perform an advanced torrent search across multiple providers.

# Result Analysis & Ranking:
1. **Quality**: Prefer 1080p or 4k, over 720p.
2. **Efficiency**: Prefer h265/HEVC for better quality/size ratio.
3. **Health**: Maximize seeders + leechers.
4. **Size**: Prefer smaller files within the same quality bracket.
5. **Language**: Ultimately, if multiple equivalent options are available, choose the one with more languages.

# Response Requirements:
- Recommend the **top 3-5** results maximum.
- For each recommendation, include: Filename, Size, Seeds/Leechs, Date, Source, and a 1-sentence "Why this?" reason.
- If results are poor, irrelevant or too diverse, suggest specific keywords to improve the search.`,
	}, func(ctx context.Context, in searchArgs) (string, error) {
		return t.SearchTorrents(ctx, in.Query)
	})

	popularSchema := mustSchema[popularArgs]()
	perSource := popularSchema.Properties["per_source"]
	perSource.Default = json.RawMessage("20")
	perSource.Minimum = new(float64)
	*perSource.Minimum = 1
	textTool(s, &mcp.Tool{
		Name:  "popular_torrents",
		Title: "Popular Torrents",
		Description: "Get the most popular torrents right now across providers with an official top listing.\n\n" +
			"Output is grouped per source (thepiratebay.org, uindex.org, 1337x.to, eztvx.to,\n" +
			"yts.vg, nyaa.si), keeping up to `per_source` results per site, each site's\n" +
			"entries pre-ranked by swarm health.\n\n" +
			"# Response Requirements:\n" +
			"- Recommend the **top 5-10** results maximum, focused on latest releases.\n" +
			"- For each recommendation, include: Filename, Size, Seeds/Leechs, Date, Source, and a 1-sentence \"Why this?\" reason.",
		InputSchema: popularSchema,
	}, func(ctx context.Context, in popularArgs) (string, error) {
		return t.PopularTorrents(ctx, in.PerSource)
	})

	textTool(s, &mcp.Tool{
		Name:        "get_torrent",
		Title:       "Get Torrent",
		Description: "Get the magnet link for a specific torrent by id.",
	}, func(ctx context.Context, in getTorrentArgs) (string, error) {
		return t.GetTorrent(ctx, in.TorrentID)
	})

	textTool(s, &mcp.Tool{
		Name:  "authorize_webapp",
		Title: "Authorize Webapp",
		Description: `Authorize a browser on the Torrent Search webapp via its pairing code.

The code comes from an user interaction on the webapp:
they click "Open in Telegram", scan the QR code, or copy the prompt message,
which sends "Authorize <CODE> for Torrent Search" to you. Take the code from that
message and call this tool with it and the user's Telegram chat id.
Codes are single-use and expire after 5 minutes. After approval the user
MUST go back to the webapp: the browser polls and completes the
authentication there (it shows "Access granted" and unlocks the app permanently).`,
	}, func(ctx context.Context, in authorizeArgs) (string, error) {
		return t.AuthorizeWebapp(ctx, in.Code, in.ChatID)
	})

	textTool(s, &mcp.Tool{
		Name:  "forward_torrent",
		Title: "Forward Torrent",
		Description: `Send a torrent (filename + magnet) to the user's Telegram chat.

The forward goes through the Torrent Search REST API, which owns the
Telegram bot token; when PRUNE_MAGNET_LINKS is enabled the magnet is
pruned to 'magnet:?xt=urn:btih:HASH&dn=<filename>' before sending.
Requires TORRENT_SEARCH_API_KEY (same value as the API server) and
TORRENT_SEARCH_API_URL. Forwards are rate-limited per chat (20/min).`,
	}, t.ForwardTorrent)

	textTool(s, &mcp.Tool{
		Name:  "torrent_webapp",
		Title: "Torrent Webapp",
		Description: `Present the Torrent Search webapp and its Telegram pairing access system.

Returns the webapp URL and how to get authorized by you.
Tell the user to open the URL: on first visit the site shows a pairing
dialog with a QR code and buttons ("Open in Telegram", "t.me" fallback, "Copy Prompt").
Ask the user to scan the QR code, click "Open in Telegram", or copy the
prompt message and send it to you in the Telegram chat.
That message ("Authorize <CODE> for Torrent Search") carries the code;
when it arrives, extract it and call authorize_webapp with it and the user's
Telegram chat id. Codes expire after 5 minutes.`,
	}, func(context.Context, noArgs) (string, error) {
		return t.TorrentWebapp(), nil
	})

	return s
}
