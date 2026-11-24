package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/m1k1o/neko-rooms/internal/aggregate"
	"github.com/m1k1o/neko-rooms/internal/api"
	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type aggregateRooms struct{ types.RoomManager }

func (aggregateRooms) GetEntryByName(_ context.Context, name string) (*types.RoomEntry, error) {
	if name != "alpha" {
		return nil, types.ErrRoomNotFound
	}
	return &types.RoomEntry{ID: "aaaaaaaaaaaa", Name: name, NetworkMode: "http", Running: true}, nil
}

type unusedMediaFactory struct{}

func (unusedMediaFactory) NewPeer(context.Context, aggregate.PeerCallbacks) (aggregate.Peer, error) {
	return nil, aggregate.ErrNotReady
}

func TestAggregationRealRouterPublicAssetsSeparateFromOwner(t *testing.T) {
	for _, prefix := range []string{"/manage", "/"} {
		t.Run(prefix, func(t *testing.T) {
			static := t.TempDir()
			_ = os.MkdirAll(filepath.Join(static, "js"), 0755)
			_ = os.WriteFile(filepath.Join(static, "aggregate.html"), []byte("owned aggregate page"), 0644)
			_ = os.WriteFile(filepath.Join(static, "js", "aggregate.fixture.js"), []byte("owned aggregate js"), 0644)
			_ = os.WriteFile(filepath.Join(static, "index.html"), []byte("owner manager page"), 0644)
			roomConfig := &config.Room{PathPrefix: "/room", NetworkMode: "http"}
			serverConfig := &config.Server{Admin: config.Admin{PathPrefix: prefix, AuthMode: "form", Username: "owner", Password: "owner-secret", Static: static}}
			rooms := aggregateRooms{}
			gateway := NewAggregation(rooms, roomConfig, serverConfig, unusedMediaFactory{})
			defer gateway.Shutdown()
			manager := New(api.New(rooms, nil, nil), roomConfig, serverConfig, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) }), http.NotFoundHandler(), gateway)
			for _, test := range []struct {
				path, body string
				status     int
			}{{"/room/alpha/", "owned aggregate page", 200}, {"/__aggregate/js/aggregate.fixture.js", "owned aggregate js", 200}, {"/room/legacy/", "", 418}} {
				w := httptest.NewRecorder()
				manager.router.ServeHTTP(w, httptest.NewRequest("GET", test.path, nil))
				if w.Code != test.status || (test.body != "" && !strings.Contains(w.Body.String(), test.body)) || w.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("public viewer called Owner gate: %s %d", test.path, w.Code)
				}
			}
			owner := strings.TrimSuffix(prefix, "/") + "/api/config/rooms"
			w := httptest.NewRecorder()
			manager.router.ServeHTTP(w, httptest.NewRequest("GET", owner, nil))
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("viewer middleware bypassed Owner API form authentication")
			}
			w = httptest.NewRecorder()
			manager.router.ServeHTTP(w, httptest.NewRequest("POST", "/room/alpha/__aggregate/login", strings.NewReader("bad json")))
			if w.Code != 415 || strings.Contains(w.Body.String(), "owner-secret") || w.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("room login was challenged by Owner auth")
			}
		})
	}
}
func TestAggregationOriginUsesExistingForwardedHeaderTrust(t *testing.T) {
	for _, test := range []struct {
		name   string
		proxy  bool
		origin string
		status int
	}{{"trusted HTTPS proxy", true, "https://public.example.test", 400}, {"different public port", true, "https://public.example.test:444", 403}, {"untrusted forwarded HTTPS", false, "https://public.example.test", 403}} {
		t.Run(test.name, func(t *testing.T) {
			rooms := aggregateRooms{}
			roomConfig := &config.Room{PathPrefix: "/room"}
			serverConfig := &config.Server{Proxy: test.proxy, Admin: config.Admin{PathPrefix: "/manage", Username: "owner", Password: "secret"}}
			gateway := NewAggregation(rooms, roomConfig, serverConfig, unusedMediaFactory{})
			defer gateway.Shutdown()
			manager := New(api.New(rooms, nil, nil), roomConfig, serverConfig, http.NotFoundHandler(), http.NotFoundHandler(), gateway)
			r := httptest.NewRequest("POST", "http://internal:8080/room/alpha/__aggregate/login", strings.NewReader("bad JSON"))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", test.origin)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-Host", "public.example.test")
			w := httptest.NewRecorder()
			manager.router.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("forwarded origin policy: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestAggregationRealRouterFirstAuthNotOwnerCookieOrBearer(t *testing.T) {
	rooms := aggregateRooms{}
	roomConfig := &config.Room{PathPrefix: "/room"}
	serverConfig := &config.Server{Admin: config.Admin{PathPrefix: "/manage", Username: "owner", Password: "secret"}}
	gateway := NewAggregation(rooms, roomConfig, serverConfig, unusedMediaFactory{})
	defer gateway.Shutdown()
	manager := New(api.New(rooms, nil, nil), roomConfig, serverConfig, http.NotFoundHandler(), http.NotFoundHandler(), gateway)
	server := httptest.NewServer(manager.router)
	defer server.Close()
	conn, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/room/alpha/__aggregate/stream", http.Header{"Authorization": {"Bearer agent-fixture-key"}, "Cookie": {"neko_rooms_admin_session=owner-fixture-cookie"}, "Origin": {server.URL}})
	if err != nil {
		t.Fatalf("Owner-independent viewer upgrade: %v %+v", err, res)
	}
	defer conn.Close()
	_ = conn.WriteJSON(map[string]string{"type": "control", "action": "request"})
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	for range 5 {
		_, _, err = conn.ReadMessage()
		if err != nil {
			closeErr, ok := err.(*websocket.CloseError)
			if !ok || closeErr.Code != 1008 {
				t.Fatalf("bad first auth was not rejected: %v", err)
			}
			return
		}
	}
	t.Fatal("Owner cookie/Agent bearer substituted for first native viewer auth")
}
