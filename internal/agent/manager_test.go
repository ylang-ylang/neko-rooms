package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/m1k1o/neko-rooms/internal/types"
)

type fakeRooms struct {
	mu      sync.Mutex
	entries map[string]types.RoomEntry
	calls   []string
}

func (f *fakeRooms) GetEntryByName(_ context.Context, name string) (*types.RoomEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	entry, ok := f.entries[name]
	if !ok {
		return nil, types.ErrRoomNotFound
	}
	return &entry, nil
}

func (f *fakeRooms) put(entry types.RoomEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[entry.Name] = entry
}

func (f *fakeRooms) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, name)
}

type upstreamRequest struct {
	Host, Path, Authorization, Cookie string
}

type fakeChromium struct {
	server   *httptest.Server
	seen     chan upstreamRequest
	closed   chan string
	commands atomic.Int64
}

func newFakeChromium(t *testing.T, name string) *fakeChromium {
	t.Helper()
	f := &fakeChromium{seen: make(chan upstreamRequest, 256), closed: make(chan string, 256)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen <- upstreamRequest{r.Host, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie")}
		switch r.URL.Path {
		case "/json/version":
			writeJSON(w, 200, map[string]any{
				"Browser":              "existing-chromium-" + name,
				"webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/browser/" + name,
			})
		case "/json", "/json/", "/json/list":
			writeJSON(w, 200, []any{map[string]any{
				"id": "displayed-page", "type": "page", "url": "https://existing-tab.example/",
				"webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/page/" + name,
			}})
		default:
			if !isDevToolsPath(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			defer func() { f.closed <- r.URL.Path }()
			for {
				var command struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := conn.ReadJSON(&command); err != nil {
					return
				}
				f.commands.Add(1)
				if err := conn.WriteJSON(map[string]any{
					"id": command.ID, "browser": name,
					"result": map[string]any{"method": command.Method, "page": "displayed-page"},
				}); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

type harness struct {
	manager *Manager
	rooms   *fakeRooms
	server  *httptest.Server
	client  *http.Client
}

func newHarness(t *testing.T, upstreams map[string]*fakeChromium) *harness {
	t.Helper()
	rooms := &fakeRooms{entries: map[string]types.RoomEntry{
		"stable": {Name: "stable", ID: "aaaaaaaaaaaa", Running: true},
		"other":  {Name: "other", ID: "cccccccccccc", Running: true},
	}}
	manager, err := New(rooms, "")
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		f := upstreams[addr]
		if f == nil {
			return nil, fmt.Errorf("unexpected upstream %s", addr)
		}
		u, _ := url.Parse(f.server.URL)
		return (&net.Dialer{}).DialContext(ctx, network, u.Host)
	}
	manager.transport.DialContext = dial
	manager.dialer.NetDialContext = dial
	router := chi.NewRouter()
	router.Route("/manage/api", manager.Mount)
	router.Handle("/rooms/{roomName}/*", manager)
	router.Handle("/rooms/{roomName}", manager)
	server := httptest.NewServer(router)
	t.Cleanup(func() {
		manager.Shutdown()
		server.Close()
	})
	return &harness{manager: manager, rooms: rooms, server: server, client: &http.Client{Timeout: 5 * time.Second}}
}

type issuedKey struct {
	KeyID, Key, RoomName, CDPURL, Prompt string
	CreatedAt, ExpiresAt                 time.Time
}

func (h *harness) request(t *testing.T, method, path, body, token string, status int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.StatusCode, status, data)
	}
	return data
}

func (h *harness) issue(t *testing.T, room, body string) issuedKey {
	t.Helper()
	data := h.request(t, "POST", "/manage/api/agent/rooms/"+room+"/keys", body, "", 201)
	var response struct {
		KeyID     string    `json:"key_id"`
		Key       string    `json:"key"`
		RoomName  string    `json:"room_name"`
		CDPURL    string    `json:"cdp_url"`
		Prompt    string    `json:"prompt"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	return issuedKey{response.KeyID, response.Key, response.RoomName, response.CDPURL, response.Prompt, response.CreatedAt, response.ExpiresAt}
}

func (h *harness) socket(t *testing.T, path, key string) *websocket.Conn {
	t.Helper()
	conn, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+path, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		if res != nil {
			t.Fatalf("dial: HTTP %d: %v", res.StatusCode, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func command(t *testing.T, conn *websocket.Conn, browser string) {
	t.Helper()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Target.getTargets"}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var response struct {
		ID      int    `json:"id"`
		Browser string `json:"browser"`
		Result  struct {
			Page string `json:"page"`
		} `json:"result"`
	}
	if err := conn.ReadJSON(&response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 1 || response.Browser != browser || response.Result.Page != "displayed-page" {
		t.Fatalf("unexpected CDP reply %+v", response)
	}
}

func assertSocketClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("revoked socket still received a message")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("socket was not closed: %v", err)
	}
}

func awaitUpstreamClosed(t *testing.T, f *fakeChromium) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream socket remained open")
	}
}

func TestDiscoveryAndWebSocketUseExistingBrowser(t *testing.T) {
	f := newFakeChromium(t, "original")
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": f})
	key := h.issue(t, "stable", `{"label":"agent example"}`)
	if key.CDPURL != h.server.URL+"/rooms/stable/" || key.RoomName != "stable" {
		t.Fatalf("wrong stable room URL: %+v", key)
	}
	if key.ExpiresAt.Sub(key.CreatedAt) != time.Hour {
		t.Fatal("default TTL is not 3600 seconds")
	}
	random, err := base64.RawURLEncoding.DecodeString(key.Key)
	if err != nil || len(random) != 32 {
		t.Fatalf("key is not 32 random bytes: %v", err)
	}
	h.manager.registry.mu.Lock()
	_, stored := h.manager.registry.byHash[sha256.Sum256([]byte(key.Key))]
	h.manager.registry.mu.Unlock()
	if !stored {
		t.Fatal("registry did not store the key by its hash")
	}
	for _, text := range []string{key.CDPURL, "Bearer " + key.Key, key.ExpiresAt.Format(time.RFC3339Nano), "connectOverCDP", "connect_over_cdp", "browser.contexts()[0]", "browser.contexts[0]", "wait for my task", "paused", "resume", "Never send raw CDP Browser.close", "WebSocket handshake", "extraHTTPHeaders"} {
		if !strings.Contains(key.Prompt, text) {
			t.Errorf("Chinese prompt missing %q", text)
		}
	}

	version := h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 200)
	var discovery struct {
		Browser string `json:"Browser"`
		WS      string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(version, &discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.Browser != "existing-chromium-original" || discovery.WS != "ws"+strings.TrimPrefix(key.CDPURL, "http")+"devtools/browser/original" {
		t.Fatalf("wrong discovery %s", version)
	}
	browser := h.socket(t, "/rooms/stable/devtools/browser/original", key.Key)
	command(t, browser, "original")

	for _, path := range []string{"/json", "/json/", "/json/list"} {
		data := h.request(t, "GET", "/rooms/stable"+path, "", key.Key, 200)
		var pages []map[string]any
		if err := json.Unmarshal(data, &pages); err != nil {
			t.Fatal(err)
		}
		if len(pages) != 1 || pages[0]["id"] != "displayed-page" || pages[0]["webSocketDebuggerUrl"] != "ws"+strings.TrimPrefix(key.CDPURL, "http")+"devtools/page/original" {
			t.Fatalf("discovery lost existing tab: %s", data)
		}
	}
	page := h.socket(t, "/rooms/stable/devtools/page/original", key.Key)
	command(t, page, "original")

	data := h.request(t, "GET", "/manage/api/agent/rooms/stable/keys", "", "", 200)
	var metadata struct {
		Keys []KeyMetadata `json:"keys"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Keys) != 1 || metadata.Keys[0].ActiveConnections != 2 || metadata.Keys[0].Label != "agent example" {
		t.Fatalf("wrong metadata %s", data)
	}
	if strings.Contains(string(data), key.Key) || strings.Contains(string(data), `"key":`) || strings.Contains(string(data), "hash") || strings.Contains(string(data), "prompt") {
		t.Fatalf("metadata leaks secret: %s", data)
	}
	for i := 0; i < 6; i++ {
		seen := <-f.seen
		if seen.Authorization != "" || seen.Cookie != "" || seen.Host != "127.0.0.1:9222" {
			t.Fatalf("credentials or incorrect Host reached Chromium: %+v", seen)
		}
	}
}

func TestAuthorizationAndRoomStatus(t *testing.T) {
	f := newFakeChromium(t, "original")
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": f})
	key := h.issue(t, "stable", `{}`)
	for _, header := range []string{"", "Basic dXNlcjpwYXNz", "Bearer", "Bearer bogus", "Bearer " + key.Key + " extra"} {
		req, _ := http.NewRequest("GET", h.server.URL+"/rooms/stable/json/version?key="+key.Key, nil)
		req.Header.Set("Authorization", header)
		req.Header.Set("Cookie", "token="+key.Key)
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("auth %q: got %d", header, res.StatusCode)
		}
	}
	for _, headers := range []http.Header{nil, {"Authorization": {"Basic dXNlcjpwYXNz"}}, {"Authorization": {"Bearer bogus"}}, {"Authorization": {"Bearer " + key.Key, "Bearer " + key.Key}}} {
		conn, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/rooms/stable/devtools/browser/original?token="+key.Key, headers)
		if conn != nil {
			conn.Close()
		}
		if err == nil || res == nil || res.StatusCode != 401 {
			t.Fatalf("unauthenticated WS accepted: %v, %+v", err, res)
		}
	}
	h.request(t, "GET", "/rooms/other/json/version", "", key.Key, 403)
	_, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/rooms/other/devtools/browser/original", http.Header{"Authorization": {"Bearer " + key.Key}})
	if err == nil || res == nil || res.StatusCode != 403 {
		t.Fatal("wrong-room WS was not denied")
	}
	h.rooms.remove("stable")
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 404)
	h.rooms.put(types.RoomEntry{Name: "stable", ID: "aaaaaaaaaaaa", Running: false})
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 503)
	h.rooms.put(types.RoomEntry{Name: "stable", ID: "aaaaaaaaaaaa", Running: true, Paused: true})
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 503)
	h.rooms.put(types.RoomEntry{Name: "stable", ID: "attacker.example:80", Running: true})
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 502)
	h.rooms.put(types.RoomEntry{Name: "stable", ID: "aaaaaaaaaaaa", Running: true})
	h.request(t, "GET", "/rooms/stable/login", "", key.Key, 404)
	h.request(t, "POST", "/rooms/stable/json/version", "", key.Key, 405)
	if len(f.seen) != 0 {
		t.Fatal("rejected requests reached Chromium")
	}
}

func TestInvalidTTLAndIssuance(t *testing.T) {
	h := newHarness(t, nil)
	for _, ttl := range []string{"0", "-1", "86401", "1.5", `"3600"`, "null", "true", "9223372036854775808"} {
		h.request(t, "POST", "/manage/api/agent/rooms/stable/keys", `{"ttl_seconds":`+ttl+`}`, "", 400)
	}
	for _, body := range []string{"", "null", "[]", "{} {}", "{} trailing", `{"label":4}`} {
		h.request(t, "POST", "/manage/api/agent/rooms/stable/keys", body, "", 400)
	}
	for _, ttl := range []int{1, 86400} {
		key := h.issue(t, "stable", fmt.Sprintf(`{"ttl_seconds":%d}`, ttl))
		if key.ExpiresAt.Sub(key.CreatedAt) != time.Duration(ttl)*time.Second {
			t.Fatalf("incorrect TTL for %d", ttl)
		}
	}
	h.request(t, "POST", "/manage/api/agent/rooms/missing/keys", `{}`, "", 404)
	h.request(t, "GET", "/manage/api/agent/rooms/missing/keys", "", "", 404)
	h.manager.Shutdown()
	h.request(t, "POST", "/manage/api/agent/rooms/stable/keys", `{}`, "", 503)
}
