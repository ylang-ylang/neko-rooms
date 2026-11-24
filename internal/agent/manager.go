// Package agent grants temporary CDP access to the Chromium already displayed
// in a managed room. It does not create browsers, contexts, pages or containers.
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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/m1k1o/neko-rooms/internal/types"
)

type roomLookup interface {
	GetEntryByName(context.Context, string) (*types.RoomEntry, error)
}

type Manager struct {
	rooms     roomLookup
	registry  *registry
	publicURL *url.URL
	transport *http.Transport
	dialer    websocket.Dialer
}

func New(rooms roomLookup, publicURL string) (*Manager, error) {
	var base *url.URL
	if publicURL != "" {
		var err error
		base, err = url.Parse(publicURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
			return nil, errors.New("agent.public_url must be an absolute HTTP(S) public prefix without credentials, query or fragment")
		}
		base.Path = strings.TrimRight(base.Path, "/") + "/"
		base.RawPath = ""
	}
	dial := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &Manager{
		rooms:     rooms,
		registry:  newRegistry(),
		publicURL: base,
		transport: &http.Transport{
			DialContext:           dial,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		dialer: websocket.Dialer{NetDialContext: dial, HandshakeTimeout: 10 * time.Second},
	}, nil
}

// Mount is called only from the existing, admin-protected API mount.
func (m *Manager) Mount(r chi.Router) {
	r.Route("/agent/rooms/{roomName}/keys", func(r chi.Router) {
		r.Post("/", m.issueKey)
		r.Get("/", m.listKeys)
		r.Delete("/{keyID}", m.revokeKey)
	})
}

func (m *Manager) Shutdown() {
	m.registry.shutdown()
	m.transport.CloseIdleConnections()
}

func (m *Manager) issueKey(w http.ResponseWriter, r *http.Request) {
	var request *struct {
		TTLSeconds json.RawMessage `json:"ttl_seconds"`
		Label      string          `json:"label"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&request); err != nil || request == nil {
		http.Error(w, "expected a JSON object with optional ttl_seconds and label", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "expected a single JSON object", http.StatusBadRequest)
		return
	}
	ttl := int64(defaultTTLSeconds)
	if request.TTLSeconds != nil {
		var seconds int64
		if err := json.Unmarshal(request.TTLSeconds, &seconds); err != nil || seconds <= 0 || seconds > maxTTLSeconds {
			http.Error(w, "ttl_seconds must be an integer between 1 and 86400", http.StatusBadRequest)
			return
		}
		ttl = seconds
	}
	room := chi.URLParam(r, "roomName")
	if _, ok := m.lookupRoom(w, r, room); !ok {
		return
	}
	base, err := m.roomURL(r, room)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	metadata, token, err := m.registry.issue(room, request.Label, ttl)
	if err != nil {
		http.Error(w, "unable to issue temporary key", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		KeyID     string    `json:"key_id"`
		Key       string    `json:"key"`
		RoomName  string    `json:"room_name"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
		CDPURL    string    `json:"cdp_url"`
		Prompt    string    `json:"prompt"`
	}{
		KeyID: metadata.KeyID, Key: token, RoomName: room,
		CreatedAt: metadata.CreatedAt, ExpiresAt: metadata.ExpiresAt,
		CDPURL: base.String(), Prompt: agentPrompt(base.String(), token, metadata.ExpiresAt),
	})
}

func (m *Manager) listKeys(w http.ResponseWriter, r *http.Request) {
	room := chi.URLParam(r, "roomName")
	if _, ok := m.lookupRoom(w, r, room); !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Keys []KeyMetadata `json:"keys"`
	}{Keys: m.registry.list(room)})
}

func (m *Manager) revokeKey(w http.ResponseWriter, r *http.Request) {
	// A deleted/recreated container must not prevent revocation of its keys.
	if !m.registry.revoke(chi.URLParam(r, "roomName"), chi.URLParam(r, "keyID")) {
		http.Error(w, "temporary key not found in this room", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) lookupRoom(w http.ResponseWriter, r *http.Request, name string) (*types.RoomEntry, bool) {
	entry, err := m.rooms.GetEntryByName(r.Context(), name)
	if errors.Is(err, types.ErrRoomNotFound) || (entry == nil && err == nil) {
		http.Error(w, "managed room not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(w, "unable to resolve managed room", http.StatusServiceUnavailable)
		return nil, false
	}
	return entry, true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func (m *Manager) roomURL(r *http.Request, room string) (*url.URL, error) {
	var base url.URL
	if m.publicURL != nil {
		base = *m.publicURL
	} else {
		base.Scheme = "http"
		if r.TLS != nil {
			base.Scheme = "https"
		}
		base.Host = r.Host
		// Use the first forwarded value, matching the outermost public proxy.
		for _, field := range strings.Split(strings.Split(r.Header.Get("Forwarded"), ",")[0], ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if !ok {
				continue
			}
			value = strings.Trim(value, `"`)
			switch strings.ToLower(key) {
			case "proto":
				base.Scheme = value
			case "host":
				base.Host = value
			}
		}
		if proto := firstHeader(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			base.Scheme = proto
		}
		if host := firstHeader(r.Header.Get("X-Forwarded-Host")); host != "" {
			base.Host = host
		}
		if base.Scheme != "http" && base.Scheme != "https" {
			return nil, errors.New("invalid forwarded public scheme")
		}
		hostURL, err := url.Parse(base.Scheme + "://" + base.Host)
		if err != nil || hostURL.Host == "" || hostURL.User != nil || hostURL.Path != "" || hostURL.RawQuery != "" || hostURL.Fragment != "" {
			return nil, errors.New("invalid public host")
		}
		base.Path = "/rooms/"
	}
	base.Path += room + "/"
	return &base, nil
}

func firstHeader(value string) string {
	return strings.TrimSpace(strings.Split(value, ",")[0])
}

func agentPrompt(cdpURL, token string, expires time.Time) string {
	return fmt.Sprintf(`请通过 CDP 连接这个 Neko 房间正在显示的同一个 Chromium，不要启动浏览器。
CDP URL：%s
临时认证请求头：Authorization: Bearer %s
到期时间：%s（UTC）；密钥到期、被撤销或管理器重启后连接会被断开。

Node.js（Playwright）：
const browser = await chromium.connectOverCDP(%q, { headers: { Authorization: %q } });
const context = browser.contexts()[0];
console.log(context.pages().map((page, index) => ({ index, url: page.url() })));

Python（Playwright 异步 API）：
browser = await playwright.chromium.connect_over_cdp(%q, headers={"Authorization": %q})
context = browser.contexts[0]
print([{"index": i, "url": page.url} for i, page in enumerate(context.pages)])

HTTP discovery（如 /json/version、/json/list）和 WebSocket 握手都必须使用上面相同的 Authorization 请求头；不要把密钥放进 URL/query，也不要用网页全局 extraHTTPHeaders / extra_http_headers 代替 CDP 连接认证。
只复用现有的默认 context 和已有 pages；禁止 launch、newContext / new_context，禁止创建新的浏览器或独立上下文。连接后先列出当前标签页，报告给我，然后等待我的任务，不要自行操作。
收到“暂停”时停止操作并回复“已暂停”，等待我明确说“继续”后先回复“继续”再恢复；没有“继续”指令不得自行恢复。
禁止发送原始 CDP Browser.close，禁止关闭共享 context 或共享 pages；不要使用会关闭共享浏览器的清理逻辑。任务完成后停止操作并等待我撤销临时密钥。
不需要也不要索取 Neko 登录密码、管理后台凭据或长期 Basic 凭据；只使用本次临时 Bearer 密钥。`,
		cdpURL, token, expires.UTC().Format(time.RFC3339Nano),
		cdpURL, "Bearer "+token, cdpURL, "Bearer "+token)
}
