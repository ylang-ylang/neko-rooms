package aggregate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const maxNativeJSON = 1 << 20

type nativeProfile struct {
	Name               string `json:"name"`
	IsAdmin            bool   `json:"is_admin"`
	CanLogin           bool   `json:"can_login"`
	CanConnect         bool   `json:"can_connect"`
	CanWatch           bool   `json:"can_watch"`
	CanHost            bool   `json:"can_host"`
	CanAccessClipboard bool   `json:"can_access_clipboard"`
}

type nativeSession struct {
	ID      string          `json:"id"`
	Token   string          `json:"token,omitempty"`
	Profile nativeProfile   `json:"profile"`
	State   json.RawMessage `json:"state"`
}

func (s nativeSession) mayWatch() bool {
	return s.ID != "" && s.Profile.CanLogin && s.Profile.CanConnect && s.Profile.CanWatch
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]string{"type": "error", "message": message, "code": code})
}
func methodError(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, 405, "method not allowed", "method_not_allowed")
}

func decodeJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func (g *Gateway) nativeRequest(ctx context.Context, container, method, endpoint, token string, body []byte) ([]byte, int, error) {
	r, err := http.NewRequestWithContext(ctx, method, "http://"+container+":8080"+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	// Fresh credentials only: no owner, Agent, cookie, Origin or query auth.
	r.Header.Set("Accept", "application/json")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := g.client.Do(r)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxNativeJSON+1))
	if len(data) > maxNativeJSON {
		return nil, 0, errors.New("oversized native response")
	}
	return data, res.StatusCode, err
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		methodError(w, "POST")
		return
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		writeError(w, 415, "login requires application/json", "invalid_login")
		return
	}
	var credentials *struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || decodeJSON(data, &credentials) != nil || credentials == nil || len(credentials.Username) > 256 || len(credentials.Password) > 4096 {
		writeError(w, 400, "invalid room login", "invalid_login")
		return
	}
	entry, status := g.lookup(r.Context(), name)
	if status != 0 {
		writeError(w, status, "managed HTTP room unavailable", "room_unavailable")
		return
	}
	body, _ := json.Marshal(credentials)
	data, status, err = g.nativeRequest(r.Context(), entry.ID, http.MethodPost, "/api/login", "", body)
	if err != nil {
		writeError(w, 502, "native room login unavailable", "upstream_unavailable")
		return
	}
	if status != http.StatusOK {
		if status != 400 && status != 401 && status != 403 && status != 422 {
			status = 502
		}
		writeError(w, status, "native room rejected login", "login_denied")
		return
	}
	var session nativeSession
	if decodeJSON(data, &session) != nil || session.ID == "" {
		writeError(w, 502, "invalid native login response", "upstream_invalid")
		return
	}
	if !roomToken.MatchString(session.Token) {
		// Cookie-enabled Neko omits token. Never promote its cookie or the
		// Manager's API/admin session to a viewer credential.
		writeError(w, 502, "native cookie sessions are unsupported; enable native token sessions", "cookie_mode_unsupported")
		return
	}
	var response map[string]any
	_ = json.Unmarshal(data, &response)
	response["member"] = map[string]string{"id": session.ID, "username": session.Profile.Name}
	writeJSON(w, 200, response)
}

func bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !roomToken.MatchString(parts[1]) {
		return ""
	}
	return parts[1]
}

func (g *Gateway) logout(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		methodError(w, "POST")
		return
	}
	token := bearer(r)
	if token == "" {
		writeError(w, 401, "native room bearer token required", "auth_required")
		return
	}
	entry, status := g.lookup(r.Context(), name)
	if status != 0 {
		writeError(w, status, "managed HTTP room unavailable", "room_unavailable")
		return
	}
	key := keyFor(entry.ID, token)
	// Stop only this native viewer, including a concurrent handshake. No
	// owner logout, room-wide invalidation or unrelated viewer cancellation.
	g.stopWhere(func(v *viewer) bool { return v.key == key }, websocket.CloseNormalClosure, "viewer logged out")
	data, status, err := g.nativeRequest(r.Context(), entry.ID, http.MethodPost, "/api/logout", token, nil)
	if err != nil {
		writeError(w, 502, "native room logout unavailable", "upstream_unavailable")
		return
	}
	if status != 200 {
		if status != 401 && status != 403 {
			status = 502
		}
		writeError(w, status, "native room rejected logout", "logout_denied")
		return
	}
	var success bool
	if decodeJSON(data, &success) != nil || !success {
		writeError(w, 502, "invalid native logout response", "upstream_invalid")
		return
	}
	writeJSON(w, 200, true)
}

func (g *Gateway) whoami(ctx context.Context, container, token string) (nativeSession, error) {
	data, status, err := g.nativeRequest(ctx, container, http.MethodGet, "/api/whoami", token, nil)
	var session nativeSession
	if err != nil || status != 200 || decodeJSON(data, &session) != nil || !session.mayWatch() {
		return session, errors.New("native room authentication or viewing permission denied")
	}
	return session, nil
}

func (g *Gateway) dialNative(v *viewer, token string) (*websocket.Conn, error) {
	dialer := g.dialer
	// Own the deadline below. Gorilla otherwise creates and cancels a nested
	// handshake context before DialContext returns, racing socket disarming.
	dialer.HandshakeTimeout = 0
	dial := dialer.NetDialContext
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		// DialContext alone doesn't cancel a blocked HTTP upgrade read.
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		return &cancelConn{Conn: conn, stop: stop}, nil
	}
	ctx, cancel := context.WithTimeout(v.ctx, 5*time.Second)
	defer cancel()
	conn, res, err := dialer.DialContext(ctx, "ws://"+v.container+":8080/api/ws", http.Header{"Authorization": {"Bearer " + token}})
	if res != nil && res.Body != nil {
		res.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	// The handshake timeout's cancellation must no longer own the live socket.
	if raw, ok := conn.UnderlyingConn().(*cancelConn); ok {
		raw.stop()
	}
	return conn, nil
}

type cancelConn struct {
	net.Conn
	stop func() bool
}

func (c *cancelConn) Close() error { c.stop(); return c.Conn.Close() }
