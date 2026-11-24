package aggregate

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

var (
	roomName    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	containerID = regexp.MustCompile(`^[0-9a-f]{12}$`)
	roomToken   = regexp.MustCompile(`^[a-zA-Z0-9_-]{64}$`)
)

type RoomLookup interface {
	// The room manager owns instance-label scoping. Resolve the stable name on
	// every request/connection, never from a cached or user-supplied address.
	GetEntryByName(context.Context, string) (*types.RoomEntry, error)
}

type Options struct {
	PathPrefix string
	Static     string
	// CheckOrigin must use the server's existing forwarded-header trust policy.
	// Absent Origin is allowed for explicit-token programmatic clients.
	CheckOrigin func(*http.Request, string) bool
}

type sessionKey struct {
	container string
	token     [sha256.Size]byte
}

type Gateway struct {
	rooms     RoomLookup
	opts      Options
	factory   PeerFactory
	client    *http.Client
	dialer    websocket.Dialer
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	viewers   map[*viewer]struct{}
	bySession map[sessionKey]*viewer
	wg        sync.WaitGroup
}

func New(rooms RoomLookup, opts Options, factory PeerFactory) *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	opts.PathPrefix = strings.TrimSuffix(path.Join("/", opts.PathPrefix), "/")
	dial := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &Gateway{
		rooms: rooms, opts: opts, factory: factory, ctx: ctx, cancel: cancel,
		viewers: make(map[*viewer]struct{}), bySession: make(map[sessionKey]*viewer),
		client: &http.Client{Transport: &http.Transport{DialContext: dial, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second}, Timeout: 8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		dialer: websocket.Dialer{NetDialContext: dial, HandshakeTimeout: 5 * time.Second},
	}
}

// WatchRooms subscribes before requests are accepted. An invalidated container
// tears down all of its viewers; a recreated stable name gets a fresh lookup.
func (g *Gateway) WatchRooms(source interface {
	Events(context.Context) (<-chan types.RoomEvent, <-chan error)
}) {
	events, errs := source.Events(g.ctx)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		for {
			select {
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				if event.Action == types.RoomEventStopped || event.Action == types.RoomEventDestroyed || event.Action == types.RoomEventPaused {
					g.stopWhere(func(v *viewer) bool { return v.container == event.ID }, websocket.CloseGoingAway, "room stopped or recreated")
				}
			case _, ok := <-errs:
				// Keep draining until the source unregisters this listener and
				// closes errs. This also unblocks an in-flight room broadcast.
				g.stopWhere(func(*viewer) bool { return true }, websocket.CloseGoingAway, "room event stream unavailable")
				if !ok {
					return
				}
			}
		}
	}()
}

func (g *Gateway) Shutdown() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.cancel()
	g.stopWhere(func(*viewer) bool { return true }, websocket.CloseGoingAway, "manager shutting down")
	g.wg.Wait()
	g.client.CloseIdleConnections()
}

// Middleware reserves only the owned asset namespace, http-mode viewer entry,
// and aggregation endpoints. Other modes retain the native room UI/proxy.
func (g *Gateway) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__aggregate" || strings.HasPrefix(r.URL.Path, "/__aggregate/") {
			g.asset(w, r)
			return
		}
		prefix := g.opts.PathPrefix + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			next.ServeHTTP(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if !canonicalPath(r.URL) && strings.Contains(rest, "/__aggregate") {
			writeError(w, http.StatusBadRequest, "invalid room path", "invalid_path")
			return
		}
		name, suffix, hasSlash := strings.Cut(rest, "/")
		reserved := suffix == "__aggregate" || strings.HasPrefix(suffix, "__aggregate/")
		root := !hasSlash || suffix == ""
		if !reserved && !root {
			if suffix == "api/ws" || websocket.IsWebSocketUpgrade(r) {
				entry, err := g.rooms.GetEntryByName(r.Context(), name)
				if err == nil && entry != nil && entry.NetworkMode == config.NetworkModeHTTP {
					writeError(w, 404, "native room signaling is private; use the aggregation stream", "private_signaling")
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if !canonicalPath(r.URL) || !roomName.MatchString(name) {
			writeError(w, http.StatusBadRequest, "invalid room path", "invalid_path")
			return
		}
		if reserved {
			if r.URL.RawQuery != "" {
				writeError(w, 400, "aggregation URLs do not accept query parameters", "invalid_path")
				return
			}
			if !g.originAllowed(r) {
				writeError(w, 403, "same-origin request required", "origin_denied")
				return
			}
			switch suffix {
			case "__aggregate/login":
				g.login(w, r, name)
			case "__aggregate/logout":
				g.logout(w, r, name)
			case "__aggregate/stream":
				g.stream(w, r, name)
			default:
				writeError(w, 404, "aggregation endpoint not found", "not_found")
			}
			return
		}
		entry, err := g.rooms.GetEntryByName(r.Context(), name)
		if err != nil || entry == nil || entry.NetworkMode != config.NetworkModeHTTP {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodError(w, "GET, HEAD")
			return
		}
		if !hasSlash {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
			return
		}
		g.serveFile(w, r, filepath.Join(g.opts.Static, "aggregate.html"))
	})
}

func canonicalPath(u *url.URL) bool {
	return u.RawPath == "" && !strings.ContainsAny(u.Path, "\\\x00\r\n%") && !strings.Contains(u.Path, "//") && (path.Clean(u.Path) == u.Path || path.Clean(u.Path)+"/" == u.Path)
}

func (g *Gateway) originAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	if len(values) != 1 {
		return false
	}
	if g.opts.CheckOrigin != nil {
		return g.opts.CheckOrigin(r, values[0])
	}
	u, err := url.Parse(values[0])
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func (g *Gateway) asset(w http.ResponseWriter, r *http.Request) {
	if !canonicalPath(r.URL) {
		writeError(w, 400, "invalid asset path", "invalid_path")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodError(w, "GET, HEAD")
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, "/__aggregate/")
	if sub == "" || sub == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	g.serveFile(w, r, filepath.Join(g.opts.Static, sub))
}

func (g *Gateway) serveFile(w http.ResponseWriter, r *http.Request, file string) {
	if g.opts.Static == "" {
		writeError(w, 503, "aggregation viewer assets are not installed", "not_ready")
		return
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, file)
}

func (g *Gateway) lookup(ctx context.Context, name string) (*types.RoomEntry, int) {
	entry, err := g.rooms.GetEntryByName(ctx, name)
	if errors.Is(err, types.ErrRoomNotFound) || (err == nil && entry == nil) {
		return nil, 404
	}
	if err != nil {
		return nil, 503
	}
	if entry.NetworkMode != config.NetworkModeHTTP {
		return nil, 404
	}
	if !entry.Running || entry.Paused {
		return nil, 503
	}
	if !containerID.MatchString(entry.ID) {
		return nil, 502
	}
	return entry, 0
}

func (g *Gateway) stopWhere(matches func(*viewer) bool, code int, reason string) {
	g.mu.Lock()
	var viewers []*viewer
	for v := range g.viewers {
		if matches(v) {
			viewers = append(viewers, v)
		}
	}
	g.mu.Unlock()
	for _, v := range viewers {
		v.stop(code, reason)
	}
}
