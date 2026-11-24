package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestRevokeClosesOpenSocketsBeforeDeleteReturnsAndIsolatesKeys(t *testing.T) {
	f := newFakeChromium(t, "original")
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": f})
	key := h.issue(t, "stable", `{}`)
	other := h.issue(t, "stable", `{}`)
	socket := h.socket(t, "/rooms/stable/devtools/browser/original", key.Key)
	page := h.socket(t, "/rooms/stable/devtools/page/original", key.Key)
	survivor := h.socket(t, "/rooms/stable/devtools/browser/original", other.Key)
	command(t, socket, "original")
	command(t, page, "original")
	command(t, survivor, "original")
	h.request(t, "DELETE", "/manage/api/agent/rooms/other/keys/"+key.KeyID, "", "", 404)
	h.request(t, "DELETE", "/manage/api/agent/rooms/stable/keys/unknown", "", "", 404)
	command(t, socket, "original")
	h.request(t, "DELETE", "/manage/api/agent/rooms/stable/keys/"+key.KeyID, "", "", 204)

	// These are the SAME sockets that successfully executed commands above,
	// not a reconnect check. A post-DELETE command cannot reach Chromium.
	before := f.commands.Load()
	socket.SetWriteDeadline(time.Now().Add(time.Second))
	socket.WriteJSON(map[string]any{"id": 2, "method": "Browser.getVersion"})
	assertSocketClosed(t, socket)
	assertSocketClosed(t, page)
	awaitUpstreamClosed(t, f)
	awaitUpstreamClosed(t, f)
	if got := f.commands.Load(); got != before {
		t.Fatalf("revoked OPEN socket executed a command after DELETE: %d -> %d", before, got)
	}
	command(t, survivor, "original")
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 401)
	h.request(t, "DELETE", "/manage/api/agent/rooms/stable/keys/"+key.KeyID, "", "", 404)
	data := h.request(t, "GET", "/manage/api/agent/rooms/stable/keys", "", "", 200)
	if strings.Contains(string(data), key.KeyID) || !strings.Contains(string(data), other.KeyID) {
		t.Fatalf("wrong keys survived revocation: %s", data)
	}
}

func TestOneSecondExpiryClosesOpenSocketWhileOtherKeySurvives(t *testing.T) {
	f := newFakeChromium(t, "original")
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": f})
	key := h.issue(t, "stable", `{"ttl_seconds":1}`)
	other := h.issue(t, "stable", `{}`)
	socket := h.socket(t, "/rooms/stable/devtools/browser/original", key.Key)
	survivor := h.socket(t, "/rooms/stable/devtools/browser/original", other.Key)
	command(t, socket, "original")
	command(t, survivor, "original")
	assertSocketClosed(t, socket) // blocks on the expiry event, not a sleep/poll
	if time.Now().Before(key.ExpiresAt) {
		t.Fatal("socket closed before its advertised expiry")
	}
	awaitUpstreamClosed(t, f)
	before := f.commands.Load()
	socket.WriteJSON(map[string]any{"id": 2, "method": "Browser.getVersion"})
	assertSocketClosed(t, socket)
	if got := f.commands.Load(); got != before {
		t.Fatal("expired socket still executed commands")
	}
	command(t, survivor, "original")
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 401)
	data := h.request(t, "GET", "/manage/api/agent/rooms/stable/keys", "", "", 200)
	if strings.Contains(string(data), key.KeyID) || !strings.Contains(string(data), other.KeyID) {
		t.Fatalf("expiry affected the wrong key: %s", data)
	}
}

func TestStableNameResolvesReplacementOnEachDiscoveryAndWebSocket(t *testing.T) {
	old := newFakeChromium(t, "old")
	replacement := newFakeChromium(t, "replacement")
	h := newHarness(t, map[string]*fakeChromium{
		"aaaaaaaaaaaa:9223": old,
		"bbbbbbbbbbbb:9223": replacement,
	})
	key := h.issue(t, "stable", `{}`)
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 200)
	socket := h.socket(t, "/rooms/stable/devtools/browser/old", key.Key)
	command(t, socket, "old")
	h.rooms.put(types.RoomEntry{Name: "stable", ID: "bbbbbbbbbbbb", Running: true})
	data := h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 200)
	if !strings.Contains(string(data), "/rooms/stable/devtools/browser/replacement") || strings.Contains(string(data), "bbbbbbbbbbbb") {
		t.Fatalf("discovery did not retain stable name: %s", data)
	}
	current := h.socket(t, "/rooms/stable/devtools/browser/replacement", key.Key)
	command(t, current, "replacement")
	// Name lookup is independent for the socket even when discovery saw an
	// older container. No container ID is cached inside a temporary key.
	h.rooms.put(types.RoomEntry{Name: "stable", ID: "aaaaaaaaaaaa", Running: true})
	currentAgain := h.socket(t, "/rooms/stable/devtools/page/old", key.Key)
	command(t, currentAgain, "old")
	h.rooms.mu.Lock()
	calls := append([]string(nil), h.rooms.calls...)
	h.rooms.mu.Unlock()
	if len(calls) != 6 {
		t.Fatalf("wanted name lookup for issuance + every HTTP/WS connection, got %v", calls)
	}
	for _, call := range calls {
		if call != "stable" {
			t.Fatalf("lookup used non-name identity %q", call)
		}
	}
}

func TestShutdownClosesHijackedSocketsAndInvalidatesKeys(t *testing.T) {
	f := newFakeChromium(t, "original")
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": f})
	key := h.issue(t, "stable", `{}`)
	socket := h.socket(t, "/rooms/stable/devtools/browser/original", key.Key)
	command(t, socket, "original")
	h.manager.Shutdown()
	assertSocketClosed(t, socket)
	awaitUpstreamClosed(t, f)
	h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 401)
	// A new in-memory manager (a process restart) knows no previous keys.
	restarted, err := New(h.rooms, "")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	if _, _, err := restarted.registry.acquire(context.Background(), key.Key, "stable"); err != errUnauthorized {
		t.Fatalf("key survived manager restart: %v", err)
	}
}

func TestRevocationDuringPendingUpstreamHandshake(t *testing.T) {
	entered := make(chan struct{})
	resume := make(chan struct{})
	upstreamClosed := make(chan struct{})
	defer close(resume)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
			close(upstreamClosed)
			return
		case <-resume:
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, _ = conn.ReadMessage()
		}
	}))
	t.Cleanup(upstream.Close)
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": {server: upstream}})
	key := h.issue(t, "stable", `{}`)
	result := make(chan error, 1)
	go func() {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/rooms/stable/devtools/browser/pending", http.Header{"Authorization": {"Bearer " + key.Key}})
		if conn != nil {
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, err = conn.ReadMessage()
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream handshake never started")
	}
	h.request(t, "DELETE", "/manage/api/agent/rooms/stable/keys/"+key.KeyID, "", "", 204)
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("DELETE did not close raw upstream during a stalled handshake")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("revoked in-flight handshake produced a usable connection")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not cancel the pending handshake")
	}
}

type countedCloser struct{ count atomic.Int64 }

func (c *countedCloser) Close() error {
	c.count.Add(1)
	return nil
}

func TestConcurrentAcquireAttachRevokeAndShutdown(t *testing.T) {
	// Registration and attach deliberately race revocation. Whether acquire
	// succeeds or not, a late socket must be closed exactly once, never escape.
	for i := 0; i < 100; i++ {
		r := newRegistry()
		metadata, token, err := r.issue("stable", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		closers := make([]*countedCloser, 8)
		for j := range closers {
			closers[j] = &countedCloser{}
			wg.Add(1)
			go func(c *countedCloser) {
				defer wg.Done()
				<-start
				record, s, err := r.acquire(context.Background(), token, "stable")
				if err != nil {
					c.Close() // a dial could not start without authorization
					return
				}
				s.attachUpstream(c)
				r.release(record, s)
			}(closers[j])
		}
		wg.Add(2)
		go func() { defer wg.Done(); <-start; r.revoke("stable", metadata.KeyID) }()
		go func() { defer wg.Done(); <-start; r.shutdown() }()
		close(start)
		wg.Wait()
		for _, c := range closers {
			if got := c.count.Load(); got != 1 {
				t.Fatalf("iteration %d: socket closed %d times", i, got)
			}
		}
		if len(r.byID) != 0 || len(r.byHash) != 0 {
			t.Fatal("revoked record remained in registry")
		}
	}
}

func TestUpstreamErrorsAreExplicitAndNeverLoginHTML(t *testing.T) {
	for _, status := range []int{200, 302, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "/login")
				w.WriteHeader(status)
				io.WriteString(w, "<html>login-secret</html>")
			}))
			t.Cleanup(upstream.Close)
			h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": {server: upstream}})
			key := h.issue(t, "stable", `{}`)
			body := h.request(t, "GET", "/rooms/stable/json/version", "", key.Key, 502)
			if strings.Contains(string(body), "<html>") || strings.Contains(string(body), "login-secret") {
				t.Fatalf("HTML login leaked through CDP proxy: %s", body)
			}
		})
	}
}

type handshakeConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *handshakeConn) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(data)
}

type hijackWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestExpiryClosesRawClientDuringStalledUpgradeWrite(t *testing.T) {
	r := newRegistry()
	defer r.shutdown()
	_, token, err := r.issue("stable", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	record, pending, err := r.acquire(context.Background(), token, "stable")
	if err != nil {
		t.Fatal(err)
	}
	defer r.release(record, pending)
	upstream := &countedCloser{}
	pending.attachUpstream(upstream)
	_, established, err := r.acquire(context.Background(), token, "stable")
	if err != nil {
		t.Fatal(err)
	}
	establishedClient, establishedUpstream := &countedCloser{}, &countedCloser{}
	established.mu.Lock()
	established.client = establishedClient
	established.mu.Unlock()
	established.attachUpstream(establishedUpstream)
	local, remote := net.Pipe()
	defer remote.Close()
	conn := &handshakeConn{Conn: local, started: make(chan struct{})}
	w := hijackWriter{ResponseRecorder: httptest.NewRecorder(), conn: conn}
	request := httptest.NewRequest("GET", "/rooms/stable/devtools/browser/existing", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-Websocket-Version", "13")
	request.Header.Set("Sec-Websocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	result := make(chan error, 1)
	go func() {
		upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
		client, err := upgrader.Upgrade(&sessionResponseWriter{ResponseWriter: w, session: pending}, request, nil)
		if client != nil {
			client.Close()
		}
		result <- err
	}()
	select {
	case <-conn.started:
	case <-time.After(2 * time.Second):
		t.Fatal("client handshake did not start")
	}
	// The peer deliberately never reads the HTTP 101. Expiry still owns and
	// closes this raw socket; no registry/session lock waits on network I/O.
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled handshake unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled handshake delayed temporary-key expiry")
	}
	// Wait on the registry lock, not a timing guess, for the expiry callback
	// to finish closing every session under this key.
	if r.revoke("stable", record.metadata.KeyID) {
		t.Fatal("handshake closed before expiry removed the key")
	}
	if upstream.count.Load() != 1 || establishedClient.count.Load() != 1 || establishedUpstream.count.Load() != 1 {
		t.Fatal("stalled handshake prevented closure of another established session")
	}
}
