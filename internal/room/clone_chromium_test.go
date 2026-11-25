//go:build linux

package room

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Opt in with NEKO_CLONE_TEST_CHROMIUM=/usr/bin/chromium. Only two new local,
// disposable browser profiles are used; no Docker or existing rooms are touched.
func TestCloneChromiumFreshIdentity(t *testing.T) {
	binary := os.Getenv("NEKO_CLONE_TEST_CHROMIUM")
	if binary == "" {
		t.Skip("set NEKO_CLONE_TEST_CHROMIUM for the isolated browser identity check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var unexpectedNetwork atomic.Int32
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.neko-clone/") {
			unexpectedNetwork.Add(1)
		}
		w.Header().Set("Content-Type", "text/html")
		seed := ""
		if r.URL.Query().Has("seed") {
			seed = "localStorage.setItem('partition-marker','source-account');"
		}
		w.Write([]byte("<!doctype html><script>" + seed + "parent.postMessage({partitioned:localStorage.getItem('partition-marker')==='source-account'},'*')</script>"))
	}))
	defer child.Close()
	childOrigin := strings.Replace(child.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.neko-clone/") {
			unexpectedNetwork.Add(1)
		}
		if r.URL.Path == "/identity" {
			persistent, pErr := r.Cookie("fresh-persistent")
			session, sErr := r.Cookie("fresh-session")
			json.NewEncoder(w).Encode(pErr == nil && sErr == nil && persistent.Value == "source-account" && session.Value == "source-account")
			return
		}
		query := ""
		if r.URL.Query().Has("seed") {
			query = "?seed"
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><title>Disposable clone identity fixture</title><script>window.partitionedReady=new Promise(resolve=>addEventListener('message',e=>{if(e.data&&'partitioned' in e.data)resolve(e.data.partitioned)}));</script><iframe src='" + childOrigin + "/child" + query + "'></iframe>"))
	}))
	defer origin.Close()
	source, target := t.TempDir(), t.TempDir()
	manager := &RoomManagerCtx{}
	sourceAddress, sourcePID := startCloneTestChromium(t, ctx, binary, source)
	sourcePage := cloneTestPage(t, ctx, manager, sourceAddress)
	defer sourcePage.close()
	// Runtime's awaited promise confirms the IndexedDB transaction committed,
	// not merely that JS queued a write. Cookies are set immediately afterward.
	cloneTestNavigate(t, ctx, sourcePage, origin.URL+"/?seed")
	cloneTestEvaluate(t, ctx, sourcePage, `window.partitionedReady`)
	cloneTestEvaluate(t, ctx, sourcePage, `(async()=>{localStorage.setItem('clone-marker','source-account');await new Promise((resolve,reject)=>{const open=indexedDB.open('clone-fixture',1);open.onupgradeneeded=()=>open.result.createObjectStore('identity');open.onerror=()=>reject(open.error);open.onsuccess=()=>{const db=open.result;const tx=db.transaction('identity','readwrite');tx.objectStore('identity').put('source-account','account');tx.oncomplete=()=>{db.close();resolve()};tx.onerror=()=>reject(tx.error)}});return true})()`)
	// Keep a separate browser-level client attached to the existing HTTP page,
	// and retain an about:blank tab in this disposable local fixture. Capture
	// must handle the mixture without excluding inherited/opaque blank frames.
	observer, err := manager.connectBrowser(ctx, sourceAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.close()
	var sourceTargets struct {
		Infos []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := observer.call(ctx, "Target.getTargets", nil, &sourceTargets); err != nil {
		t.Fatal(err)
	}
	for _, target := range sourceTargets.Infos {
		if target.Type == "page" && strings.HasPrefix(target.URL, origin.URL+"/") {
			if err := observer.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	// This creates a tab only in our new local test browser, never in the
	// separate optional real-native owned canary regression.
	if err := observer.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sourcePage.call(ctx, "Network.setCookies", map[string]any{"cookies": []any{
		map[string]any{"name": "fresh-persistent", "value": "source-account", "url": origin.URL, "httpOnly": true, "sameSite": "Lax", "expires": float64(time.Now().Add(time.Hour).Unix())},
		map[string]any{"name": "fresh-session", "value": "source-account", "url": origin.URL, "httpOnly": true, "sameSite": "Lax"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	seeded := time.Now()
	cookies, err := manager.snapshotCookies(ctx, sourceAddress)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := manager.snapshotLocalStorage(ctx, sourceAddress)
	if err != nil {
		t.Fatal(err)
	}
	var describe func(*cloneFrame)
	describe = func(frame *cloneFrame) {
		t.Logf("captured frame origin=%s children=%d entriesBytes=%d", frame.Frame.Origin, len(frame.Children), len(frame.Entries))
		for _, child := range frame.Children {
			describe(child)
		}
	}
	for _, page := range storage {
		describe(page)
	}
	if err := syscall.Kill(-sourcePID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resume := func() { syscall.Kill(-sourcePID, syscall.SIGCONT) }
	defer resume()
	if err := snapshotTree(ctx, source, target); err != nil {
		resume()
		t.Fatal(err)
	}
	resume()
	if time.Since(seeded) > time.Second {
		t.Log("fixture snapshot took over one second on this host")
	}
	targetAddress, _ := startCloneTestChromium(t, ctx, binary, target)
	before, err := manager.snapshotCookies(ctx, targetAddress)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("filesystem-only fresh cookie availability: persistent=%t session=%t", hasFixtureCookie(before, "fresh-persistent"), hasFixtureCookie(before, "fresh-session"))
	if err := manager.restoreCookies(ctx, targetAddress, cookies); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreLocalStorage(ctx, targetAddress, storage); err != nil {
		t.Fatal(err)
	}
	if unexpectedNetwork.Load() != 0 {
		t.Fatal("controlled localStorage restoration contacted a website")
	}
	after, err := manager.snapshotCookies(ctx, targetAddress)
	if err != nil || !hasFixtureCookie(after, "fresh-persistent") || !hasFixtureCookie(after, "fresh-session") {
		t.Fatal("fresh persistent/HttpOnly/session cookies did not survive target startup and supplementation")
	}
	targetPage := cloneTestPage(t, ctx, manager, targetAddress)
	defer targetPage.close()
	cloneTestNavigate(t, ctx, targetPage, origin.URL)
	t.Run("HttpOnlyAccountIdentity", func(t *testing.T) {
		cloneTestEvaluate(t, ctx, targetPage, `(async()=>await (await fetch('/identity')).json())()`)
	})
	t.Run("partitionedLocalStorage", func(t *testing.T) {
		cloneTestEvaluate(t, ctx, targetPage, `window.partitionedReady`)
	})
	t.Run("localStorage", func(t *testing.T) {
		cloneTestEvaluate(t, ctx, targetPage, `localStorage.getItem('clone-marker')==='source-account'`)
	})
	t.Run("IndexedDB", func(t *testing.T) {
		cloneTestEvaluate(t, ctx, targetPage, `(async()=>{const value=await new Promise((resolve,reject)=>{const open=indexedDB.open('clone-fixture',1);open.onerror=()=>reject(open.error);open.onsuccess=()=>{const db=open.result;const tx=db.transaction('identity');const request=tx.objectStore('identity').get('account');request.onsuccess=()=>resolve(request.result);request.onerror=()=>reject(request.error)}});return value==='source-account'})()`)
	})
	cloneTestEvaluate(t, ctx, targetPage, `localStorage.setItem('clone-marker','target-account');true`)
	cloneTestEvaluate(t, ctx, sourcePage, `localStorage.getItem('clone-marker')==='source-account'`)
	if !t.Failed() {
		t.Log("target retained real localStorage/IndexedDB identity and fresh HttpOnly persistent/session cookies; target write left source identity unchanged")
	}
}

func hasFixtureCookie(cookies []map[string]json.RawMessage, name string) bool {
	for _, cookie := range cookies {
		var cookieName, value string
		json.Unmarshal(cookie["name"], &cookieName)
		json.Unmarshal(cookie["value"], &value)
		if cookieName == name && value == "source-account" && string(cookie["httpOnly"]) == "true" {
			return true
		}
	}
	return false
}

func startCloneTestChromium(t *testing.T, ctx context.Context, binary, profile string) (string, int) {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "--headless", "--no-sandbox", "--disable-dev-shm-usage", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--no-first-run", "--no-default-browser-check", "--password-store=basic", "--user-data-dir="+profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "about:blank")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = io.Discard
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	address := make(chan string, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		syscall.Kill(-command.Process.Pid, syscall.SIGCONT)
		syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if text, ok := strings.CutPrefix(scanner.Text(), "DevTools listening on "); ok {
				endpoint, err := url.Parse(text)
				if err == nil {
					address <- endpoint.Host
				}
			}
		}
	}()
	select {
	case host := <-address:
		return host, command.Process.Pid
	case <-done:
		t.Fatal("isolated Chromium exited before CDP became ready")
	case <-ctx.Done():
		t.Fatal("isolated Chromium readiness timed out")
	}
	return "", 0
}

func cloneTestPage(t *testing.T, ctx context.Context, manager *RoomManagerCtx, address string) *browserCDP {
	t.Helper()
	browser, err := manager.connectBrowser(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.close()
	var targets struct {
		Infos []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := browser.call(ctx, "Target.getTargets", nil, &targets); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets.Infos {
		if target.Type != "page" {
			continue
		}
		conn, _, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).DialContext(ctx, "ws://"+address+"/devtools/page/"+target.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		return &browserCDP{conn: conn, stop: context.AfterFunc(ctx, func() { conn.Close() })}
	}
	t.Fatal("isolated Chromium has no initial page")
	return nil
}

func cloneTestNavigate(t *testing.T, ctx context.Context, browser *browserCDP, address string) {
	t.Helper()
	if err := browser.call(ctx, "Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := browser.call(ctx, "Page.navigate", map[string]any{"url": address}, nil); err != nil {
		t.Fatal(err)
	}
	for {
		var event struct {
			Method string `json:"method"`
		}
		if browser.conn.ReadJSON(&event) != nil {
			t.Fatal("test page navigation did not complete")
		}
		if event.Method == "Page.loadEventFired" {
			return
		}
	}
}

func cloneTestEvaluate(t *testing.T, ctx context.Context, browser *browserCDP, expression string) {
	t.Helper()
	var evaluation struct {
		Result struct {
			Value bool `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if err := browser.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}, &evaluation); err != nil || evaluation.Exception != nil || !evaluation.Result.Value {
		t.Fatal("real browser identity assertion failed")
	}
}
