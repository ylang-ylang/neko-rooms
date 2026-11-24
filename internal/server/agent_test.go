package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/agent"
	"github.com/m1k1o/neko-rooms/internal/api"
	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type agentTestRooms struct{ types.RoomManager }

func (agentTestRooms) GetEntryByName(_ context.Context, name string) (*types.RoomEntry, error) {
	if name != "stable" {
		return nil, types.ErrRoomNotFound
	}
	return &types.RoomEntry{Name: name, ID: "aaaaaaaaaaaa", Running: true}, nil
}

func TestAgentManagementUsesNativeAdminAuthButPublicCDPDoesNot(t *testing.T) {
	for _, prefix := range []string{"/manage", "/"} {
		t.Run(prefix, func(t *testing.T) {
			rooms := agentTestRooms{}
			agentManager, err := agent.New(rooms, "http://cdp.example.test/rooms/")
			if err != nil {
				t.Fatal(err)
			}
			defer agentManager.Shutdown()
			manager := New(api.New(rooms, nil, agentManager), &config.Room{}, &config.Server{
				Admin: config.Admin{PathPrefix: prefix, Username: "native-admin", Password: "native-password"},
			}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			}), agentManager)
			apiPrefix := strings.TrimSuffix(prefix, "/") + "/api/agent/rooms/stable/keys"
			call := func(method, path, body, auth string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				if auth == "admin" {
					r.SetBasicAuth("native-admin", "native-password")
				} else if auth != "" {
					r.Header.Set("Authorization", auth)
				}
				w := httptest.NewRecorder()
				manager.router.ServeHTTP(w, r)
				return w
			}
			for _, method := range []string{"GET", "POST", "DELETE"} {
				path := apiPrefix
				if method == "DELETE" {
					path += "/unknown"
				}
				w := call(method, path, `{}`, "")
				if w.Code != 401 || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") {
					t.Fatalf("unprotected management %s: %d", method, w.Code)
				}
			}
			issued := call("POST", apiPrefix, `{}`, "admin")
			if issued.Code != 201 {
				t.Fatalf("native admin could not issue: %d %s", issued.Code, issued.Body.String())
			}
			var key struct {
				Key   string `json:"key"`
				KeyID string `json:"key_id"`
			}
			if err := json.Unmarshal(issued.Body.Bytes(), &key); err != nil {
				t.Fatal(err)
			}
			if w := call("GET", apiPrefix, "", "Bearer "+key.Key); w.Code != 401 {
				t.Fatal("temporary key bypassed native admin authentication")
			}
			if w := call("GET", "/rooms/stable/json/version", "", "admin"); w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("public CDP used Basic/native auth or fallback: %d", w.Code)
			}
			if w := call("GET", "/rooms/other/json/version", "", "Bearer "+key.Key); w.Code != 403 {
				t.Fatalf("public CDP did not use key's room identity: %d", w.Code)
			}
			if w := call("GET", "/legacy-room", "", ""); w.Code != http.StatusTeapot {
				t.Fatal("original room proxy fallback changed")
			}
			if w := call("DELETE", apiPrefix+"/"+key.KeyID, "", "admin"); w.Code != 204 {
				t.Fatalf("native admin could not revoke: %d", w.Code)
			}
		})
	}
}

func TestPublicCDPDoesNotCallNativeProxyAuthentication(t *testing.T) {
	var authCalls atomic.Int64
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls.Add(1)
		w.WriteHeader(401)
		w.Write([]byte("<html>native login</html>"))
	}))
	defer auth.Close()
	rooms := agentTestRooms{}
	agentManager, err := agent.New(rooms, "")
	if err != nil {
		t.Fatal(err)
	}
	defer agentManager.Shutdown()
	manager := New(api.New(rooms, nil, agentManager), &config.Room{}, &config.Server{
		Admin: config.Admin{PathPrefix: "/manage", ProxyAuth: auth.URL},
	}, http.NotFoundHandler(), agentManager)
	for _, path := range []string{"/rooms/stable/json/version", "/rooms/stable/devtools/browser/existing"} {
		w := httptest.NewRecorder()
		manager.router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 || strings.Contains(w.Body.String(), "<html>") {
			t.Fatalf("agent got native login HTML: %d %s", w.Code, w.Body.String())
		}
	}
	if authCalls.Load() != 0 {
		t.Fatal("public CDP invoked native proxy auth")
	}
	w := httptest.NewRecorder()
	manager.router.ServeHTTP(w, httptest.NewRequest("GET", "/manage/api/agent/rooms/stable/keys", nil))
	if w.Code != 401 || authCalls.Load() != 1 {
		t.Fatal("management no longer uses native proxy auth")
	}
}
