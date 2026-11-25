package room

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

	"github.com/gorilla/websocket"
)

// A paused filesystem snapshot includes opaque profile identity but cannot
// include Chrome's pending cookie writes or memory-only session cookies. CDP
// supplements that snapshot using the existing default browser context only.
// Values stay private: never logs/API; durable profiles use a 0600 checkpoint.
type browserCDP struct {
	conn *websocket.Conn
	id   int
	stop func() bool
}

func (manager *RoomManagerCtx) connectBrowser(ctx context.Context, address string) (*browserCDP, error) {
	dial := manager.cdpDial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	}
	transport := &http.Transport{DialContext: dial, ResponseHeaderTimeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/json/version", nil)
	if err != nil {
		return nil, errors.New("invalid private browser address")
	}
	request.Host = "127.0.0.1:9222"
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("private browser discovery unavailable")
	}
	defer response.Body.Close()
	var discovery struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&discovery) != nil {
		return nil, errors.New("invalid private browser discovery")
	}
	upstream, err := url.Parse(discovery.URL)
	if err != nil || !strings.HasPrefix(upstream.Path, "/devtools/browser/") {
		return nil, errors.New("invalid private browser WebSocket path")
	}
	// Never trust a discovery-provided host (Chrome normally advertises its
	// own localhost). Route to the managed container, just like Agent CDP.
	upstream.Scheme, upstream.Host, upstream.User = "ws", address, nil
	upstream.RawQuery, upstream.Fragment = "", ""
	dialer := websocket.Dialer{NetDialContext: dial, HandshakeTimeout: 3 * time.Second}
	conn, _, err := dialer.DialContext(ctx, upstream.String(), http.Header{"Host": {"127.0.0.1:9222"}})
	if err != nil {
		return nil, errors.New("private browser WebSocket unavailable")
	}
	browser := &browserCDP{conn: conn}
	browser.stop = context.AfterFunc(ctx, func() { conn.Close() })
	return browser, nil
}

func (browser *browserCDP) close() {
	browser.stop()
	browser.conn.Close() // Disconnect only. Never Browser.close/context.close.
}

func (browser *browserCDP) call(ctx context.Context, method string, params any, result any) error {
	return browser.callSession(ctx, "", method, params, result)
}

func (browser *browserCDP) send(ctx context.Context, session, method string, params any) (int, error) {
	browser.id++
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	browser.conn.SetWriteDeadline(deadline)
	browser.conn.SetReadDeadline(deadline)
	if err := browser.conn.WriteJSON(struct {
		ID        int    `json:"id"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
		SessionID string `json:"sessionId,omitempty"`
	}{browser.id, method, params, session}); err != nil {
		return 0, fmt.Errorf("browser %s transport failed", method)
	}
	return browser.id, nil
}

func (browser *browserCDP) callSession(ctx context.Context, session, method string, params any, result any) error {
	id, err := browser.send(ctx, session, method, params)
	if err != nil {
		return err
	}
	for {
		var message struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := browser.conn.ReadJSON(&message); err != nil {
			return fmt.Errorf("browser %s response unavailable", method)
		}
		if message.ID != id {
			continue
		}
		if len(message.Error) != 0 {
			// CDP errors can include cookie parameters. Do not propagate them.
			return fmt.Errorf("browser rejected %s", method)
		}
		if result != nil && json.Unmarshal(message.Result, result) != nil {
			return fmt.Errorf("invalid browser %s response", method)
		}
		return nil
	}
}

func (manager *RoomManagerCtx) snapshotCookies(ctx context.Context, address string) ([]map[string]json.RawMessage, error) {
	browser, err := manager.connectBrowser(ctx, address)
	if err != nil {
		return nil, err
	}
	defer browser.close()
	var result struct {
		Cookies []map[string]json.RawMessage `json:"cookies"`
	}
	if err := browser.call(ctx, "Storage.getCookies", nil, &result); err != nil {
		return nil, err
	}
	cookies := make([]map[string]json.RawMessage, 0, len(result.Cookies))
	for _, cookie := range result.Cookies {
		if string(cookie["partitionKeyOpaque"]) == "true" {
			return nil, errors.New("browser has an opaque cookie partition that cannot be cloned")
		}
		// CookieParam differs from Cookie: session/size are read-only, and
		// session cookies must omit expires rather than passing -1.
		param := map[string]json.RawMessage{}
		for _, key := range []string{"name", "value", "domain", "path", "secure", "httpOnly", "sameSite", "priority", "sameParty", "sourceScheme", "sourcePort", "partitionKey"} {
			if value, exists := cookie[key]; exists {
				param[key] = value
			}
		}
		if string(cookie["session"]) != "true" {
			if expires, exists := cookie["expires"]; exists {
				param["expires"] = expires
			}
		}
		cookies = append(cookies, param)
	}
	return cookies, nil
}

func (manager *RoomManagerCtx) restoreCookies(ctx context.Context, address string, cookies []map[string]json.RawMessage) error {
	// ContainerStart completes before supervisord/Chromium have bound CDP.
	// Bound this readiness wait, preserving the request's cancellation.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		browser, err := manager.connectBrowser(ctx, address)
		if err == nil {
			defer browser.close()
			// Buffered deletions matter too: replace the target's default
			// cookie jar rather than resurrecting stale disk-only cookies.
			if err := browser.call(ctx, "Storage.clearCookies", nil, nil); err != nil {
				return err
			}
			return browser.call(ctx, "Storage.setCookies", struct {
				Cookies []map[string]json.RawMessage `json:"cookies"`
			}{cookies}, nil)
		}
		select {
		case <-ctx.Done():
			return errors.New("target browser was not ready to restore identity")
		case <-ticker.C:
		}
	}
}
