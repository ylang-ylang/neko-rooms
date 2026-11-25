package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/m1k1o/neko-rooms/internal/config"
)

const adminSessionCookie = "neko_rooms_admin_session"
const adminSessionLifetime = 7 * 24 * time.Hour

// Only hashes of opaque, cryptographically random tokens are kept in memory.
// A manager restart deliberately invalidates every owner session.
type adminAuth struct {
	config   *config.Server
	mode     string
	prefix   string
	mu       sync.Mutex
	sessions map[[32]byte]time.Time
}

func newAdminAuth(cfg *config.Server) *adminAuth {
	mode, err := cfg.Admin.AuthenticationMode()
	if err != nil {
		panic(fmt.Errorf("invalid admin authentication configuration: %w", err))
	}
	return &adminAuth{
		config: cfg, mode: mode,
		prefix:   strings.TrimSuffix(path.Join("/", cfg.Admin.PathPrefix), "/"),
		sessions: make(map[[32]byte]time.Time),
	}
}

func (a *adminAuth) root() string      { return a.prefix + "/" }
func (a *adminAuth) loginURL() string  { return a.prefix + "/auth/login" }
func (a *adminAuth) logoutURL() string { return a.prefix + "/auth/logout" }

func (a *adminAuth) isAuthPath(p string) bool {
	return p == a.prefix+"/auth/status" || p == a.loginURL() || p == a.logoutURL()
}

func (a *adminAuth) isAPIPath(p string) bool {
	return p == a.prefix+"/api" || strings.HasPrefix(p, a.prefix+"/api/")
}

func (a *adminAuth) mount(r chi.Router) {
	r.HandleFunc(a.prefix+"/auth/status", a.status)
	r.HandleFunc(a.loginURL(), a.login)
	r.HandleFunc(a.logoutURL(), a.logout)
}

func (a *adminAuth) validCredentials(username, password string) bool {
	user, pass := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
	wantUser, wantPass := sha256.Sum256([]byte(a.config.Admin.Username)), sha256.Sum256([]byte(a.config.Admin.Password))
	return subtle.ConstantTimeCompare(user[:], wantUser[:])&subtle.ConstantTimeCompare(pass[:], wantPass[:]) == 1
}

func (a *adminAuth) sessionValid(r *http.Request) bool {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || len(cookie.Value) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	key := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[key]
	if ok && !time.Now().Before(expires) {
		delete(a.sessions, key)
		return false
	}
	return ok
}

func (a *adminAuth) revoke(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Revoke every matching cookie, including ambiguous duplicate paths.
	for _, cookie := range r.Cookies() {
		if cookie.Name == adminSessionCookie {
			delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
		}
	}
}

func (a *adminAuth) cookie(r *http.Request, token string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: adminSessionCookie, Value: token,
		Path: path.Join("/", a.prefix), HttpOnly: true,
		Secure: a.requestOrigin(r).Scheme == "https", SameSite: http.SameSiteLaxMode,
		Expires: expires, MaxAge: maxAge,
	}
}

func (a *adminAuth) createSession(w http.ResponseWriter, r *http.Request) error {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	expires := now.Add(adminSessionLifetime)
	a.revoke(r)
	a.mu.Lock()
	// Sweep expired entries on issuance, without a background lifecycle.
	for key, expiry := range a.sessions {
		if !now.Before(expiry) {
			delete(a.sessions, key)
		}
	}
	a.sessions[key] = expires
	a.mu.Unlock()
	http.SetCookie(w, a.cookie(r, token, expires, int(adminSessionLifetime.Seconds())))
	return nil
}

func (a *adminAuth) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch a.mode {
		case "proxy":
			res, err := a.proxyAuthenticate(r)
			if err != nil {
				http.Error(w, "proxy auth failed", http.StatusForbidden)
				return
			}
			defer res.Body.Close()
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				for k, values := range res.Header {
					for _, value := range values {
						w.Header().Add(k, value)
					}
				}
				w.WriteHeader(res.StatusCode)
				_, _ = io.Copy(w, res.Body)
				return
			}
			_, _ = io.Copy(io.Discard, res.Body)
		case "basic":
			user, pass, ok := r.BasicAuth()
			if !ok || !a.validCredentials(user, pass) {
				w.Header().Set("WWW-Authenticate", `Basic realm="neko-rooms admin"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "form":
			w.Header().Set("Cache-Control", "no-store")
			// Deliberately never inspect Authorization: cached Basic credentials
			// and temporary Agent Bearer keys cannot establish an owner session.
			if !a.sessionValid(r) {
				ext := path.Ext(r.URL.Path)
				html := ext == ".html" || r.URL.Path == a.prefix || r.URL.Path == a.root() ||
					(ext == "" && strings.Contains(r.Header.Get("Accept"), "text/html"))
				if r.Method == http.MethodGet && !a.isAPIPath(r.URL.Path) && html {
					http.Redirect(w, r, a.loginURL()+"?next="+url.QueryEscape(a.safeNext(r.URL.RequestURI())), http.StatusSeeOther)
				} else {
					http.Error(w, "management login required", http.StatusUnauthorized)
				}
				return
			}
			if !safeMethod(r.Method) && !a.checkOrigin(w, r) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// Forwarded headers are meaningful only under the existing explicit --proxy
// trust setting. Include the port: another service on the same NAS is foreign.
func (a *adminAuth) requestOrigin(r *http.Request) *url.URL {
	origin := &url.URL{Scheme: "http", Host: r.Host}
	if r.TLS != nil {
		origin.Scheme = "https"
	}
	if a.config.Proxy {
		if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto == "https" {
			origin.Scheme = "https"
		}
		if host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); host != "" {
			origin.Host = host
		}
	}
	return origin
}

func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func (a *adminAuth) sameOrigin(r *http.Request, value string, referer bool) bool {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if !referer && (u.Path != "" || u.RawQuery != "" || u.ForceQuery) {
		return false
	}
	want := a.requestOrigin(r)
	return u.Scheme == want.Scheme && strings.EqualFold(u.Hostname(), want.Hostname()) && originPort(u) == originPort(want)
}

func (a *adminAuth) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origins := r.Header.Values("Origin")
	valid := false
	if len(origins) != 0 {
		valid = len(origins) == 1 && a.sameOrigin(r, origins[0], false)
	} else if referers := r.Header.Values("Referer"); len(referers) == 1 {
		valid = a.sameOrigin(r, referers[0], true)
	}
	if !valid {
		http.Error(w, "same-origin Origin or Referer required", http.StatusForbidden)
	}
	return valid
}

// Only canonical paths within this manager UI may be used as return targets.
// Reject encoded separators, traversal, auth/API routes and file-like targets
// other than HTML; for the deprecated root prefix, allow just the manager root.
func (a *adminAuth) safeNext(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" ||
		!strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.ContainsAny(value, "\\\r\n\x00") || strings.ContainsAny(u.Path, "\\\r\n\x00") || strings.Contains(u.Path, "//") || u.RawPath != "" {
		return a.root()
	}
	clean := path.Clean(u.Path)
	if u.Path != clean && u.Path != clean+"/" {
		return a.root()
	}
	if a.prefix == "" {
		if u.Path != "/" && u.Path != "/index.html" {
			return a.root()
		}
	} else if u.Path != a.prefix && !strings.HasPrefix(u.Path, a.root()) {
		return a.root()
	}
	if a.isAPIPath(u.Path) || u.Path == a.prefix+"/auth" || strings.HasPrefix(u.Path, a.prefix+"/auth/") {
		return a.root()
	}
	if ext := path.Ext(u.Path); ext != "" && ext != ".html" {
		return a.root()
	}
	return u.String()
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (a *adminAuth) status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	authenticated := false
	switch a.mode {
	case "none":
		authenticated = true
	case "form":
		authenticated = a.sessionValid(r)
	case "basic":
		user, pass, ok := r.BasicAuth()
		authenticated = ok && a.validCredentials(user, pass)
	case "proxy":
		res, err := a.proxyAuthenticate(r)
		if err == nil {
			authenticated = res.StatusCode >= 200 && res.StatusCode < 300
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}
	response := struct {
		Mode          string `json:"mode"`
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username,omitempty"`
		LoginURL      string `json:"login_url"`
		LogoutURL     string `json:"logout_url"`
	}{Mode: a.mode, Authenticated: authenticated, LoginURL: a.loginURL(), LogoutURL: a.logoutURL()}
	if authenticated && (a.mode == "form" || a.mode == "basic") {
		response.Username = a.config.Admin.Username
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (a *adminAuth) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, "GET, POST")
		return
	}
	if a.mode != "form" {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, a.root(), http.StatusSeeOther)
		} else {
			http.Error(w, "form login is not enabled", http.StatusNotFound)
		}
		return
	}
	if r.Method == http.MethodGet {
		if a.sessionValid(r) {
			http.Redirect(w, r, a.root(), http.StatusSeeOther)
			return
		}
		a.loginPage(w, http.StatusOK, "", "", a.safeNext(r.URL.Query().Get("next")))
		return
	}
	if !a.checkOrigin(w, r) {
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		http.Error(w, "login requires application/x-www-form-urlencoded", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid login form", http.StatusBadRequest)
		return
	}
	next := a.safeNext(r.PostForm.Get("next"))
	username := r.PostForm.Get("username")
	if !a.validCredentials(username, r.PostForm.Get("password")) {
		a.loginPage(w, http.StatusUnauthorized, "Invalid username or password.", username, next)
		return
	}
	if err := a.createSession(w, r); err != nil {
		http.Error(w, "unable to create management session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *adminAuth) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	if a.mode != "form" {
		http.Error(w, "form login is not enabled", http.StatusNotFound)
		return
	}
	if !a.checkOrigin(w, r) {
		return
	}
	a.revoke(r)
	http.SetCookie(w, a.cookie(r, "", time.Unix(1, 0), -1))
	http.Redirect(w, r, a.loginURL(), http.StatusSeeOther)
}

// Keep native proxy authentication's forwarding and challenge/redirect behavior
// outside the form mode. Status probes consume the result without forwarding it.
func (a *adminAuth) proxyAuthenticate(r *http.Request) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.config.Admin.ProxyAuth, nil)
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		req.Header.Add("X-Forwarded-For", clientIP)
	}
	req.Header.Add("X-Forwarded-Method", r.Method)
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	req.Header.Add("X-Forwarded-Proto", proto)
	if r.Host != "" {
		req.Header.Add("X-Forwarded-Host", r.Host)
	}
	req.Header.Add("X-Forwarded-Uri", r.URL.RequestURI())
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client.Do(req)
}

// With the deprecated root prefix the session cookie also accompanies room
// requests. Remove only this owner's cookie, preserving Neko/viewer cookies.
// Form mode additionally discards stale browser Basic credentials at dispatch.
func (a *adminAuth) withoutOwnerCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := r.Clone(r.Context())
		if _, err := r.Cookie(adminSessionCookie); err == nil {
			clean.Header.Del("Cookie")
			for _, cookie := range r.Cookies() {
				if cookie.Name != adminSessionCookie {
					clean.AddCookie(cookie)
				}
			}
		}
		if a.mode == "form" {
			for _, header := range r.Header.Values("Authorization") {
				parts := strings.Fields(header)
				if len(parts) != 0 && strings.EqualFold(parts[0], "Basic") {
					clean.Header.Del("Authorization")
					break
				}
			}
		}
		next.ServeHTTP(w, clean)
	})
}

//go:embed admin_auth_login.html
var adminLoginHTML string

var adminLoginTemplate = template.Must(template.New("admin-login").Parse(adminLoginHTML))

func (a *adminAuth) loginPage(w http.ResponseWriter, status int, message, username, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	_ = adminLoginTemplate.Execute(w, struct{ Action, Next, Error, Username string }{
		Action: a.loginURL(), Next: next, Error: message, Username: username,
	})
}
