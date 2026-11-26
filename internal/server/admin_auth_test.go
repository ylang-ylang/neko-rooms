package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/m1k1o/neko-rooms/internal/agent"
	"github.com/m1k1o/neko-rooms/internal/api"
	"github.com/m1k1o/neko-rooms/internal/config"
)

const testOwner = "existing-owner"
const testPassword = "existing-password"

type adminHarness struct {
	manager   *ServerManagerCtx
	server    *httptest.Server
	client    *http.Client
	prefix    string
	apiURL    string
	loginURL  string
	logoutURL string
}

func newAdminHarness(t *testing.T, prefix string, change func(*config.Server), tls bool) *adminHarness {
	t.Helper()
	static := t.TempDir()
	for file, body := range map[string]string{
		"index.html": "<!doctype html><title>Manager</title>",
		"app.js":     "window.manager = true;",
		// Auth routes must win even over cached static files in root mode.
		"auth/login": "wrong static login", "auth/status": "wrong static status", "auth/logout": "wrong static logout",
	} {
		p := filepath.Join(static, file)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Server{Admin: config.Admin{
		PathPrefix: prefix, Static: static, AuthMode: "form", Username: testOwner, Password: testPassword,
	}}
	if change != nil {
		change(cfg)
	}
	rooms := agentTestRooms{}
	agents, err := agent.New(rooms, "http://cdp.example.test/rooms/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agents.Shutdown)
	manager := New(api.New(rooms, nil, agents), &config.Room{}, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo only the credentials received by the room dispatch boundary.
		w.Header().Set("X-Room-Cookie", r.Header.Get("Cookie"))
		w.Header().Set("X-Room-Authorization", r.Header.Get("Authorization"))
		w.Header().Set("X-Room-Path", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}), agents)
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(manager.router)
	} else {
		srv = httptest.NewServer(manager.router)
	}
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := strings.TrimSuffix(prefix, "/")
	return &adminHarness{manager: manager, server: srv, client: client, prefix: base,
		apiURL: base + "/api/agent/rooms/stable/keys", loginURL: base + "/auth/login", logoutURL: base + "/auth/logout"}
}

func (h *adminHarness) request(t *testing.T, method, target, body string, headers http.Header, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	if cookie != nil {
		req.AddCookie(cookie)
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
	if h.manager.auth.mode == "form" && strings.HasPrefix(strings.ToLower(res.Header.Get("WWW-Authenticate")), "basic") {
		t.Fatalf("form-mode %s %s emitted native Basic challenge", method, target)
	}
	return res, string(data)
}

func basicHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func (h *adminHarness) login(t *testing.T, next string) *http.Cookie {
	t.Helper()
	res, body := h.request(t, "POST", h.loginURL, url.Values{
		"username": {testOwner}, "password": {testPassword}, "next": {next},
	}.Encode(), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("native login failed: %d %s", res.StatusCode, body)
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == adminSessionCookie || cookie.Name == adminPlainSessionCookie {
			return cookie
		}
	}
	t.Fatal("successful login did not set owner session cookie")
	return nil
}

func expectStatus(t *testing.T, res *http.Response, want int, body string) {
	t.Helper()
	if res.StatusCode != want {
		t.Fatalf("HTTP %d, want %d: %s", res.StatusCode, want, body)
	}
}

func TestFormAuthPublicEndpointsAndNoBasicBypass(t *testing.T) {
	for _, prefix := range []string{"/manage", "/nested/owner", "/"} {
		t.Run(prefix, func(t *testing.T) {
			h := newAdminHarness(t, prefix, nil, false)
			for _, authorization := range []string{"", basicHeader(testOwner, testPassword), basicHeader(testOwner, "wrong"), "Bearer temporary-agent-key"} {
				headers := http.Header{"Authorization": {authorization}}
				res, body := h.request(t, "GET", h.prefix+"/", "", headers, nil)
				expectStatus(t, res, http.StatusSeeOther, body)
				if !strings.HasPrefix(res.Header.Get("Location"), h.loginURL+"?") {
					t.Fatalf("manager HTML did not redirect to public login: %q", res.Header.Get("Location"))
				}
				for _, target := range []string{h.apiURL, h.prefix + "/api/events", h.prefix + "/app.js"} {
					res, body := h.request(t, "GET", target, "", headers, nil)
					expectStatus(t, res, http.StatusUnauthorized, body)
					if strings.Contains(res.Header.Get("Content-Type"), "text/html") || res.Header.Get("Location") != "" {
						t.Fatalf("API/assets/SSE got login HTML redirect: %s", target)
					}
				}
				res, body = h.request(t, "GET", h.prefix+"/auth/status", "", headers, nil)
				expectStatus(t, res, http.StatusOK, body)
				var status map[string]any
				if err := json.Unmarshal([]byte(body), &status); err != nil {
					t.Fatal(err)
				}
				if status["mode"] != "form" || status["authenticated"] != false || status["login_url"] != h.loginURL || status["logout_url"] != h.logoutURL || len(status) != 4 {
					t.Fatalf("unexpected public auth contract: %s", body)
				}
				res, body = h.request(t, "GET", h.loginURL, "", headers, nil)
				expectStatus(t, res, http.StatusOK, body)
				assertNativeForm(t, body, h.loginURL)
			}
			res, body := h.request(t, "GET", h.logoutURL, "", nil, nil)
			expectStatus(t, res, http.StatusMethodNotAllowed, body)
			if res.Header.Get("Allow") != "POST" {
				t.Fatal("GET logout lacks Allow: POST")
			}
			// The manager's slashless entry point also needs protection.
			if h.prefix != "" {
				res, body = h.request(t, "GET", h.prefix, "", nil, nil)
				expectStatus(t, res, http.StatusSeeOther, body)
				if !strings.HasPrefix(res.Header.Get("Location"), h.loginURL) {
					t.Fatal("slashless manager entry bypassed form auth")
				}
			}
		})
	}
}

func assertNativeForm(t *testing.T, body, loginURL string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var form, username, password, submit bool
	labels := map[string]bool{}
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		attrs := map[string]string{}
		for _, attr := range node.Attr {
			attrs[attr.Key] = attr.Val
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "form":
				form = attrs["method"] == "post" && attrs["action"] == loginURL && attrs["autocomplete"] == "on"
			case "input":
				if attrs["name"] == "username" {
					username = attrs["autocomplete"] == "username" && attrs["id"] == "username"
				}
				if attrs["name"] == "password" {
					password = attrs["type"] == "password" && attrs["autocomplete"] == "current-password" && attrs["id"] == "password"
					if attrs["value"] != "" {
						t.Fatal("login form echoes a password")
					}
				}
			case "label":
				labels[attrs["for"]] = node.FirstChild != nil
			case "button":
				submit = attrs["type"] == "submit"
			case "script":
				t.Fatal("native login form depends on JavaScript")
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	if !form || !username || !password || !submit || !labels["username"] || !labels["password"] {
		t.Fatalf("password-manager-friendly native form missing fields/labels: %s", body)
	}
}

func TestFormLoginErrorsAndEscaping(t *testing.T) {
	h := newAdminHarness(t, "/manage", nil, false)
	password := "do-not-echo-this-password"
	res, body := h.request(t, "POST", h.loginURL, url.Values{
		"username": {`"><script>alert(1)</script>`}, "password": {password}, "next": {"https://evil.test/"},
	}.Encode(), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}, "Authorization": {basicHeader(testOwner, testPassword)}}, nil)
	expectStatus(t, res, http.StatusUnauthorized, body)
	if !strings.Contains(body, "Invalid username or password.") || strings.Contains(body, password) || strings.Contains(body, "<script>") || len(res.Cookies()) != 0 {
		t.Fatalf("unsafe or non-HTML login error: %s", body)
	}
	assertNativeForm(t, body, h.loginURL)
	res, body = h.request(t, "POST", h.loginURL, `{}`, http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/json"}}, nil)
	expectStatus(t, res, http.StatusUnsupportedMediaType, body)
	res, body = h.request(t, "POST", h.loginURL, "invalid=%zz", http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, nil)
	expectStatus(t, res, http.StatusBadRequest, body)
	res, body = h.request(t, "POST", h.loginURL, "username="+strings.Repeat("x", 20<<10), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, nil)
	expectStatus(t, res, http.StatusBadRequest, body)
}

func TestFormLoginIgnoresCachedAuthorization(t *testing.T) {
	h := newAdminHarness(t, "/manage", nil, false)
	for _, header := range []string{basicHeader(testOwner, testPassword), basicHeader(testOwner, "wrong-cached-password"), "Bearer agent-key"} {
		res, body := h.request(t, "POST", h.loginURL, url.Values{"username": {testOwner}, "password": {testPassword}}.Encode(), http.Header{
			"Authorization": {header}, "Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"},
		}, nil)
		expectStatus(t, res, http.StatusSeeOther, body)
		if len(res.Cookies()) != 1 {
			t.Fatal("cached Authorization interfered with correct native form credentials")
		}
	}
}

func TestFormSessionCookieJarLogoutAndReplay(t *testing.T) {
	for _, prefix := range []string{"/manage", "/"} {
		t.Run(prefix, func(t *testing.T) {
			h := newAdminHarness(t, prefix, nil, false)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			h.client.Jar = jar
			cookie := h.login(t, h.prefix+"/")
			if !cookie.HttpOnly || cookie.Secure || cookie.Domain != "" || cookie.Path != prefix || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != int(adminSessionLifetime.Seconds()) || cookie.Expires.Before(time.Now().Add(6*24*time.Hour)) {
				t.Fatalf("incorrect direct-HTTP persistent session cookie: %+v", cookie)
			}
			raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
			if err != nil || len(raw) != 32 {
				t.Fatalf("opaque token must contain 32 random bytes: %v", err)
			}
			h.manager.auth.mu.Lock()
			_, stored := h.manager.auth.sessions[sha256.Sum256([]byte(cookie.Value))]
			h.manager.auth.mu.Unlock()
			if !stored {
				t.Fatal("server session was not indexed by token hash")
			}
			cachedBasic := http.Header{"Authorization": {basicHeader(testOwner, "wrong-cached-password")}}
			res, body := h.request(t, "GET", h.apiURL, "", cachedBasic, nil)
			expectStatus(t, res, http.StatusOK, body)
			res, body = h.request(t, "GET", h.prefix+"/auth/status", "", nil, nil)
			expectStatus(t, res, http.StatusOK, body)
			var status map[string]any
			if err := json.Unmarshal([]byte(body), &status); err != nil {
				t.Fatal(err)
			}
			if status["authenticated"] != true || status["username"] != testOwner || len(status) != 5 || strings.Contains(body, cookie.Value) || strings.Contains(body, testPassword) {
				t.Fatalf("incorrect or leaking authenticated status: %s", body)
			}
			res, body = h.request(t, "GET", h.loginURL, "", nil, nil)
			expectStatus(t, res, http.StatusSeeOther, body)
			if res.Header.Get("Location") != h.prefix+"/" {
				t.Fatal("valid-session login did not redirect to manager root")
			}
			// GET logout must neither revoke nor clear the session.
			res, body = h.request(t, "GET", h.logoutURL, "", nil, nil)
			expectStatus(t, res, http.StatusMethodNotAllowed, body)
			res, body = h.request(t, "GET", h.apiURL, "", nil, nil)
			expectStatus(t, res, http.StatusOK, body)
			res, body = h.request(t, "POST", h.logoutURL, "", http.Header{"Origin": {h.server.URL}, "Authorization": {basicHeader(testOwner, testPassword)}}, nil)
			expectStatus(t, res, http.StatusSeeOther, body)
			if res.Header.Get("Location") != h.loginURL || len(res.Cookies()) != 1 || res.Cookies()[0].MaxAge != -1 || res.Cookies()[0].Path != prefix {
				t.Fatal("logout did not clear cookie / redirect to login")
			}
			cachedBasic.Set("Authorization", basicHeader(testOwner, testPassword))
			res, body = h.request(t, "GET", h.apiURL, "", cachedBasic, nil)
			expectStatus(t, res, http.StatusUnauthorized, body)
			res, body = h.request(t, "GET", h.apiURL, "", cachedBasic, cookie)
			expectStatus(t, res, http.StatusUnauthorized, body)
			res, body = h.request(t, "GET", h.prefix+"/auth/status", "", cachedBasic, cookie)
			expectStatus(t, res, http.StatusOK, body)
			if strings.Contains(body, `"authenticated":true`) {
				t.Fatal("cached Basic or revoked session silently undid logout")
			}
			res, body = h.request(t, "POST", h.logoutURL, "", http.Header{"Origin": {h.server.URL}}, cookie)
			expectStatus(t, res, http.StatusSeeOther, body)
		})
	}
}

func TestFormSessionRotationExpiryAndRestart(t *testing.T) {
	h := newAdminHarness(t, "/manage", nil, false)
	old := h.login(t, "")
	res, body := h.request(t, "POST", h.loginURL, url.Values{"username": {testOwner}, "password": {testPassword}}.Encode(), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, old)
	expectStatus(t, res, http.StatusSeeOther, body)
	fresh := res.Cookies()[0]
	if fresh.Value == old.Value {
		t.Fatal("re-login reused an owner session token")
	}
	res, body = h.request(t, "GET", h.apiURL, "", nil, old)
	expectStatus(t, res, http.StatusUnauthorized, body)
	res, body = h.request(t, "GET", h.apiURL, "", nil, fresh)
	expectStatus(t, res, http.StatusOK, body)
	restarted := newAdminHarness(t, "/manage", nil, false)
	res, body = restarted.request(t, "GET", restarted.apiURL, "", nil, fresh)
	expectStatus(t, res, http.StatusUnauthorized, body)
	h.manager.auth.mu.Lock()
	h.manager.auth.sessions[sha256.Sum256([]byte(fresh.Value))] = time.Now().Add(-time.Second)
	h.manager.auth.mu.Unlock()
	res, body = h.request(t, "GET", h.apiURL, "", http.Header{"Authorization": {basicHeader(testOwner, testPassword)}}, fresh)
	expectStatus(t, res, http.StatusUnauthorized, body)
	res, body = h.request(t, "GET", h.prefix+"/auth/status", "", nil, fresh)
	expectStatus(t, res, http.StatusOK, body)
	if strings.Contains(body, `"authenticated":true`) {
		t.Fatal("expired session remains authenticated")
	}
	res, body = h.request(t, "POST", h.logoutURL, "", http.Header{"Origin": {h.server.URL}}, fresh)
	expectStatus(t, res, http.StatusSeeOther, body)
}

func TestFormCookieTLSAndTrustedProxy(t *testing.T) {
	t.Run("TLS", func(t *testing.T) {
		h := newAdminHarness(t, "/manage", nil, true)
		if cookie := h.login(t, ""); !cookie.Secure {
			t.Fatal("TLS session lacks Secure cookie")
		}
	})
	for _, trust := range []bool{false, true} {
		name := "ignore untrusted forwarded headers"
		if trust {
			name = "trusted HTTPS proxy"
		}
		t.Run(name, func(t *testing.T) {
			h := newAdminHarness(t, "/manage", func(cfg *config.Server) { cfg.Proxy = trust }, false)
			origin := h.server.URL
			if trust {
				origin = "https://owner.example.test:8443"
			}
			headers := http.Header{"Origin": {origin}, "Content-Type": {"application/x-www-form-urlencoded"}, "X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"owner.example.test:8443"}}
			res, body := h.request(t, "POST", h.loginURL, url.Values{"username": {testOwner}, "password": {testPassword}}.Encode(), headers, nil)
			expectStatus(t, res, http.StatusSeeOther, body)
			if len(res.Cookies()) != 1 || res.Cookies()[0].Secure != trust {
				t.Fatal("Secure cookie did not follow TLS / explicit proxy trust")
			}
			if trust {
				headers.Set("Origin", "https://owner.example.test:9443")
				res, body = h.request(t, "POST", h.logoutURL, "", headers, res.Cookies()[0])
				expectStatus(t, res, http.StatusForbidden, body)
			}
		})
	}
}

func TestFormOriginChecksProtectLoginLogoutAndMutations(t *testing.T) {
	h := newAdminHarness(t, "/manage", nil, false)
	cookie := h.login(t, "")
	parsed, _ := url.Parse(h.server.URL)
	for _, headers := range []http.Header{
		nil,
		{"Origin": {"http://evil.test"}},
		{"Origin": {parsed.Scheme + "://" + parsed.Hostname() + ":1"}}, // same NAS, foreign port
		{"Origin": {"null"}},
		{"Origin": {h.server.URL + "/path"}},
		{"Origin": {h.server.URL, "http://evil.test"}},
		{"Referer": {"http://evil.test/manage/"}},
		{"Origin": {"http://evil.test"}, "Referer": {h.server.URL + "/manage/"}},
	} {
		for _, target := range []string{h.loginURL, h.logoutURL, h.apiURL, h.prefix + "/api/rooms", h.prefix + "/api/rooms/aaaaaaaaaaaa/start", h.prefix + "/api/pull"} {
			headers = headers.Clone()
			if headers == nil {
				headers = http.Header{}
			}
			headers.Set("Content-Type", "application/x-www-form-urlencoded")
			res, body := h.request(t, "POST", target, url.Values{"username": {testOwner}, "password": {testPassword}}.Encode(), headers, cookie)
			expectStatus(t, res, http.StatusForbidden, body)
		}
	}
	res, body := h.request(t, "GET", h.apiURL, "", nil, cookie)
	expectStatus(t, res, http.StatusOK, body)
	if strings.Contains(body, `"key_id":`) {
		t.Fatal("foreign Origin issued an Agent key")
	}
	for _, headers := range []http.Header{{"Origin": {h.server.URL}}, {"Referer": {h.server.URL + "/manage/"}}} {
		res, body = h.request(t, "POST", h.apiURL, `{}`, headers, cookie)
		expectStatus(t, res, http.StatusCreated, body)
		var issued struct {
			KeyID string `json:"key_id"`
		}
		if err := json.Unmarshal([]byte(body), &issued); err != nil {
			t.Fatal(err)
		}
		res, body = h.request(t, "DELETE", h.apiURL+"/"+issued.KeyID, "", http.Header{"Origin": {"http://evil.test"}}, cookie)
		expectStatus(t, res, http.StatusForbidden, body)
		res, body = h.request(t, "DELETE", h.apiURL+"/"+issued.KeyID, "", headers, cookie)
		expectStatus(t, res, http.StatusNoContent, body)
	}
	// Browser forms without Origin may use their same-origin Referer.
	res, body = h.request(t, "POST", h.loginURL, url.Values{"username": {testOwner}, "password": {testPassword}}.Encode(), http.Header{"Referer": {h.server.URL + h.loginURL}, "Content-Type": {"application/x-www-form-urlencoded"}}, cookie)
	expectStatus(t, res, http.StatusSeeOther, body)
}

func TestFormSafeNextOnNativePost(t *testing.T) {
	for _, prefix := range []string{"/manage", "/"} {
		t.Run(prefix, func(t *testing.T) {
			h := newAdminHarness(t, prefix, nil, false)
			root := h.prefix + "/"
			for _, next := range []string{"", "https://evil.test/", "//evil.test/", "///evil.test/", "javascript:alert(1)", "relative", "/outside/", "/manage-other/", root + "../rooms/", root + "api", root + "api/rooms", root + "auth", root + "auth/login", root + "auth/status", root + "auth/logout", root + "app.js", root + "%2f%2fevil.test/", root + "%5Cevil.test/", root + "%0D%0ALocation:evil", root + "\\evil.test/"} {
				res, body := h.request(t, "POST", h.loginURL, url.Values{"username": {testOwner}, "password": {testPassword}, "next": {next}}.Encode(), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, nil)
				expectStatus(t, res, http.StatusSeeOther, body)
				if got := res.Header.Get("Location"); got != root {
					t.Errorf("unsafe next %q redirected to %q, want manager root %q", next, got, root)
				}
			}
			for _, next := range []string{root, root + "index.html?tab=rooms#view"} {
				res, body := h.request(t, "POST", h.loginURL, url.Values{"username": {testOwner}, "password": {testPassword}, "next": {next}}.Encode(), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, nil)
				expectStatus(t, res, http.StatusSeeOther, body)
				if got := res.Header.Get("Location"); got != next {
					t.Errorf("safe next %q changed to %q", next, got)
				}
			}
		})
	}
}

func TestFormOwnerSessionAndAgentBearerStaySeparate(t *testing.T) {
	for _, prefix := range []string{"/manage", "/"} {
		t.Run(prefix, func(t *testing.T) {
			h := newAdminHarness(t, prefix, nil, false)
			cookie := h.login(t, "")
			res, body := h.request(t, "POST", h.apiURL, `{}`, http.Header{"Origin": {h.server.URL}}, cookie)
			expectStatus(t, res, http.StatusCreated, body)
			var issued struct {
				Key   string `json:"key"`
				KeyID string `json:"key_id"`
			}
			if err := json.Unmarshal([]byte(body), &issued); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"GET", "POST", "DELETE"} {
				target := h.apiURL
				if method == "DELETE" {
					target += "/" + issued.KeyID
				}
				res, body = h.request(t, method, target, `{}`, http.Header{"Authorization": {"Bearer " + issued.Key}, "Origin": {h.server.URL}}, nil)
				expectStatus(t, res, http.StatusUnauthorized, body)
			}
			for _, target := range []string{"/rooms/stable/json/version", "/rooms/stable/devtools/browser/existing"} {
				for _, authorization := range []string{"", basicHeader(testOwner, testPassword), "Bearer " + cookie.Value} {
					res, body = h.request(t, "GET", target, "", http.Header{"Authorization": {authorization}}, cookie)
					expectStatus(t, res, http.StatusUnauthorized, body)
					if res.Header.Get("WWW-Authenticate") != "Bearer" || strings.Contains(body, "<html") {
						t.Fatal("owner cookie/Basic/session-as-Bearer authenticated Agent CDP")
					}
				}
			}
			res, body = h.request(t, "GET", "/rooms/other/json/version", "", http.Header{"Authorization": {"Bearer " + issued.Key}}, cookie)
			expectStatus(t, res, http.StatusForbidden, body)
			for _, target := range []string{"/legacy-room/", "/rooms/stable/", "/rooms/stable/ws", "/rooms/stable/json-assets.js"} {
				res, body = h.request(t, "GET", target, "", http.Header{"Authorization": {basicHeader(testOwner, testPassword)}, "Cookie": {"neko_viewer=preserve-me"}}, cookie)
				expectStatus(t, res, http.StatusTeapot, body)
				if res.Header.Get("X-Room-Path") != target || res.Header.Get("X-Room-Cookie") != "neko_viewer=preserve-me" || res.Header.Get("X-Room-Authorization") != "" {
					t.Fatalf("room dispatch leaked owner credentials or changed viewer behavior: %v", res.Header)
				}
			}
		})
	}
}

func TestExplicitBasicAndNoAuthCompatibility(t *testing.T) {
	for _, mode := range []string{"", "basic", "none"} {
		t.Run(mode, func(t *testing.T) {
			h := newAdminHarness(t, "/manage", func(cfg *config.Server) {
				cfg.Admin.AuthMode = mode
				if mode == "none" {
					cfg.Admin.AuthMode, cfg.Admin.Username, cfg.Admin.Password = "form", "", ""
				}
			}, false)
			res, body := h.request(t, "GET", h.apiURL, "", nil, nil)
			if mode == "none" {
				expectStatus(t, res, http.StatusOK, body)
			} else {
				expectStatus(t, res, http.StatusUnauthorized, body)
				if !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic ") {
					t.Fatal("legacy Basic challenge changed")
				}
			}
			res, body = h.request(t, "POST", h.apiURL, `{}`, http.Header{"Authorization": {basicHeader(testOwner, testPassword)}}, nil)
			expectStatus(t, res, http.StatusCreated, body)
			res, body = h.request(t, "GET", h.prefix+"/auth/status", "", nil, nil)
			expectStatus(t, res, http.StatusOK, body)
			want := "basic"
			if mode == "none" {
				want = "none"
			}
			if !strings.Contains(body, `"mode":"`+want+`"`) || res.Header.Get("WWW-Authenticate") != "" {
				t.Fatalf("legacy/no-auth status: %s", body)
			}
			res, body = h.request(t, "GET", h.prefix+"/auth/status", "", http.Header{"Authorization": {basicHeader(testOwner, testPassword)}}, nil)
			expectStatus(t, res, http.StatusOK, body)
			if !strings.Contains(body, `"authenticated":true`) || res.Header.Get("WWW-Authenticate") != "" {
				t.Fatalf("authenticated legacy/no-auth status: %s", body)
			}
		})
	}
}

func TestProxyAuthenticationCompatibilityAndPublicStatus(t *testing.T) {
	var calls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if cookie, err := r.Cookie("native_proxy"); err == nil {
			if cookie.Value == "valid" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if cookie.Value == "redirect" {
				http.Redirect(w, r, "/native-login", http.StatusFound)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="native proxy"`)
		w.Header().Set("X-Native-Auth", "preserved")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "native auth denied")
	}))
	defer proxy.Close()
	h := newAdminHarness(t, "/manage", func(cfg *config.Server) { cfg.Admin.ProxyAuth = proxy.URL }, false)
	res, body := h.request(t, "GET", h.apiURL, "", nil, nil)
	expectStatus(t, res, http.StatusUnauthorized, body)
	if res.Header.Get("WWW-Authenticate") != `Basic realm="native proxy"` || res.Header.Get("X-Native-Auth") != "preserved" || body != "native auth denied" {
		t.Fatal("native proxy response changed")
	}
	res, body = h.request(t, "GET", h.prefix+"/auth/status", "", nil, nil)
	expectStatus(t, res, http.StatusOK, body)
	if !strings.Contains(body, `"mode":"proxy","authenticated":false`) || res.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("proxy status challenged / leaked response: %s", body)
	}
	res, body = h.request(t, "GET", h.apiURL, "", nil, &http.Cookie{Name: "native_proxy", Value: "redirect"})
	expectStatus(t, res, http.StatusFound, body)
	if res.Header.Get("Location") != "/native-login" {
		t.Fatal("native proxy redirect was not preserved")
	}
	cookie := &http.Cookie{Name: "native_proxy", Value: "valid"}
	res, body = h.request(t, "POST", h.apiURL, `{}`, nil, cookie)
	expectStatus(t, res, http.StatusCreated, body)
	res, body = h.request(t, "GET", h.prefix+"/auth/status", "", nil, cookie)
	expectStatus(t, res, http.StatusOK, body)
	if !strings.Contains(body, `"authenticated":true`) {
		t.Fatalf("authenticated proxy status incorrect: %s", body)
	}
	before := calls.Load()
	res, body = h.request(t, "GET", "/rooms/stable/json/version", "", nil, cookie)
	expectStatus(t, res, http.StatusUnauthorized, body)
	if calls.Load() != before || res.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("Agent CDP called native proxy authentication")
	}
}

func TestFormManagementCORSDoesNotExposeCookiesToForeignOrigin(t *testing.T) {
	h := newAdminHarness(t, "/manage", func(cfg *config.Server) { cfg.CORS = true }, false)
	cookie := h.login(t, "")
	res, body := h.request(t, "GET", h.apiURL, "", http.Header{"Origin": {"http://evil.test"}}, cookie)
	expectStatus(t, res, http.StatusOK, body)
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS exposed owner API to foreign origins")
	}
	res, body = h.request(t, "GET", "/rooms/stable/", "", http.Header{"Origin": {"http://viewer.example.test"}}, nil)
	expectStatus(t, res, http.StatusTeapot, body)
	if res.Header.Get("Access-Control-Allow-Origin") != "http://viewer.example.test" {
		t.Fatal("manager auth changed room viewer CORS")
	}
}

func TestFormOriginDefaultPorts(t *testing.T) {
	a := newAdminAuth(&config.Server{Admin: config.Admin{AuthMode: "form", Username: testOwner, Password: testPassword}})
	for _, test := range []struct {
		target, origin string
		valid          bool
	}{
		{"http://nas.test/manage/", "http://nas.test:80", true},
		{"https://nas.test/manage/", "https://NAS.test:443", true},
		{"http://nas.test:8080/manage/", "http://nas.test:8081", false},
		{"http://nas.test/manage/", "https://nas.test", false},
		{"http://nas.test/manage/", "http://nas.test.evil.test", false},
	} {
		r := httptest.NewRequest("POST", test.target, nil)
		if got := a.sameOrigin(r, test.origin, false); got != test.valid {
			t.Errorf("target %s origin %s: got %v", test.target, test.origin, got)
		}
	}
}

func TestOwnerSessionCookiesAreSeparatedAcrossTransport(t *testing.T) {
	plain := newAdminHarness(t, "/manage", nil, false).login(t, "/manage/")
	secure := newAdminHarness(t, "/manage", nil, true).login(t, "/manage/")
	if plain.Name == secure.Name || plain.Secure || !secure.Secure || !plain.HttpOnly || !secure.HttpOnly {
		t.Fatal("HTTP transport switch can collide with an existing Secure cookie")
	}
}
