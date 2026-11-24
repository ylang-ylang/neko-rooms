package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

var containerID = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ServeHTTP is mounted outside native admin authentication. It never reads
// Basic credentials, cookies or query parameters to authenticate an agent.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authorization := r.Header.Values("Authorization")
	var token string
	if len(authorization) == 1 {
		parts := strings.Fields(authorization[0])
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token = parts[1]
		}
	}
	room := chi.URLParam(r, "roomName")
	record, session, err := m.registry.acquire(r.Context(), token, room)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, errWrongRoom) {
			status = http.StatusForbidden
		} else {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer m.registry.release(record, session)
	r = r.WithContext(session.ctx)

	// Always resolve the stable name anew through the instance-label-scoped
	// room manager, for BOTH discovery and WebSocket handshakes.
	entry, ok := m.lookupRoom(w, r, room)
	if !ok {
		return
	}
	if !entry.Running || entry.Paused {
		http.Error(w, "managed room is stopped or paused", http.StatusServiceUnavailable)
		return
	}
	if !containerID.MatchString(entry.ID) {
		http.Error(w, "managed room has an invalid container ID", http.StatusBadGateway)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "CDP discovery and WebSocket connections require GET", http.StatusMethodNotAllowed)
		return
	}
	upstreamPath := "/" + chi.URLParam(r, "*")
	upstream := &url.URL{
		Scheme: "http", Host: entry.ID + ":9223",
		Path: upstreamPath, RawQuery: r.URL.RawQuery,
	}
	if isDevToolsPath(upstreamPath) && websocket.IsWebSocketUpgrade(r) {
		upstream.Scheme = "ws"
		m.proxyWebSocket(w, r, session, upstream)
		return
	}
	if upstreamPath != "/json" && !strings.HasPrefix(upstreamPath, "/json/") {
		http.Error(w, "CDP endpoint not found", http.StatusNotFound)
		return
	}
	base, err := m.roomURL(r, room)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.proxyDiscovery(w, r, upstream, base)
}

func isDevToolsPath(path string) bool {
	for _, prefix := range []string{"/devtools/browser/", "/devtools/page/"} {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			return true
		}
	}
	return false
}

func (m *Manager) proxyDiscovery(w http.ResponseWriter, r *http.Request, upstream, public *url.URL) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.String(), nil)
	if err != nil {
		http.Error(w, "unable to build Chromium CDP request", http.StatusBadGateway)
		return
	}
	// Start from fresh headers: neither the temporary key nor native admin,
	// Neko, proxy or cookie credentials ever reach Chromium.
	request.Host = "127.0.0.1:9222"
	request.Header.Set("Accept", "application/json")
	client := &http.Client{
		Transport: m.transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "Chromium CDP discovery is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("Chromium CDP returned HTTP %d", response.StatusCode), http.StatusBadGateway)
		return
	}
	// Discovery must be JSON, not a login page, redirect, or internal HTML.
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<20))
	decoder.UseNumber()
	var discovery any
	if err := decoder.Decode(&discovery); err != nil {
		http.Error(w, "Chromium CDP returned invalid discovery JSON", http.StatusBadGateway)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "Chromium CDP returned invalid discovery JSON", http.StatusBadGateway)
		return
	}
	if err := rewriteDebuggerURLs(discovery, public); err != nil {
		http.Error(w, "Chromium CDP returned an invalid debugger URL", http.StatusBadGateway)
		return
	}
	writeJSON(w, response.StatusCode, discovery)
}

func rewriteDebuggerURLs(value any, public *url.URL) error {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "webSocketDebuggerUrl" {
				raw, ok := item.(string)
				if !ok {
					return errors.New("debugger URL is not a string")
				}
				upstream, err := url.Parse(raw)
				if err != nil || !isDevToolsPath(upstream.Path) {
					return errors.New("unsupported debugger path")
				}
				external := *public
				external.Scheme = "ws"
				if public.Scheme == "https" {
					external.Scheme = "wss"
				}
				external.Path += strings.TrimPrefix(upstream.Path, "/")
				external.RawQuery = upstream.RawQuery
				v[key] = external.String()
			} else if err := rewriteDebuggerURLs(item, public); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := rewriteDebuggerURLs(item, public); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) proxyWebSocket(w http.ResponseWriter, r *http.Request, session *session, upstreamURL *url.URL) {
	dialer := m.dialer
	dial := dialer.NetDialContext
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		// Gorilla's context only bounds the dial/deadline, not cancellation of
		// its subsequent HTTP upgrade read. Own the raw socket before that read.
		if !session.attachUpstream(conn) {
			return nil, errUnauthorized
		}
		return conn, nil
	}
	upstream, _, err := dialer.DialContext(session.ctx, upstreamURL.String(), http.Header{
		"Host": {"127.0.0.1:9222"},
	})
	if err != nil {
		if session.ctx.Err() != nil {
			http.Error(w, errUnauthorized.Error(), http.StatusUnauthorized)
		} else {
			http.Error(w, "Chromium CDP WebSocket is unavailable", http.StatusBadGateway)
		}
		return
	}
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		// Authorization is exclusively an explicit Bearer header, never an
		// ambient browser cookie, so agents may use their own Origin.
		CheckOrigin: func(*http.Request) bool { return true },
		Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
			if session.ctx.Err() != nil {
				status, reason = http.StatusUnauthorized, errUnauthorized
			}
			http.Error(w, reason.Error(), status)
		},
	}
	client, err := upgrader.Upgrade(&sessionResponseWriter{ResponseWriter: w, session: session}, r, nil)
	if err != nil {
		return
	}

	// Keep the handler alive for the SOCKET lifetime. A request context alone
	// does not own hijacked connections; registry teardown closes both ends.
	done := make(chan struct{})
	go func() {
		relayWebSocket(upstream, client)
		session.close()
		close(done)
	}()
	relayWebSocket(client, upstream)
	session.close()
	<-done
}

func relayWebSocket(destination, source *websocket.Conn) {
	for {
		kind, reader, err := source.NextReader()
		if err != nil {
			return
		}
		writer, err := destination.NextWriter(kind)
		if err != nil {
			return
		}
		if _, err := io.Copy(writer, reader); err != nil {
			// Abort both sockets rather than flush a truncated CDP message.
			return
		}
		if err := writer.Close(); err != nil {
			return
		}
	}
}
