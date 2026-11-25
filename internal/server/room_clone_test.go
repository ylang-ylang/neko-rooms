package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/agent"
	"github.com/m1k1o/neko-rooms/internal/api"
	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type authorizedCloneRooms struct {
	types.RoomManager
	calls atomic.Int32
}

func (rooms *authorizedCloneRooms) GetEntryByName(context.Context, string) (*types.RoomEntry, error) {
	return &types.RoomEntry{ID: "aaaaaaaaaaaa", Name: "source", Running: true}, nil
}

func (rooms *authorizedCloneRooms) Clone(ctx context.Context, id, name string) (*types.RoomEntry, error) {
	rooms.calls.Add(1)
	if name == "existing" {
		return nil, types.ErrRoomNameConflict
	}
	return &types.RoomEntry{ID: "bbbbbbbbbbbb", Name: name, Running: true}, nil
}

func TestRoomCloneRequiresOwnerAndSameOrigin(t *testing.T) {
	for _, prefix := range []string{"/manage", "/nested/owner", ""} {
		t.Run(prefix, func(t *testing.T) {
			rooms := &authorizedCloneRooms{}
			agents, err := agent.New(rooms, "http://cdp.example.test/rooms/")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(agents.Shutdown)
			cfg := &config.Server{Admin: config.Admin{PathPrefix: prefix, AuthMode: "form", Username: testOwner, Password: testPassword}}
			manager := New(api.New(rooms, nil, agents), &config.Room{}, cfg, http.NotFoundHandler(), agents)
			server := httptest.NewServer(manager.router)
			t.Cleanup(server.Close)
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			h := &adminHarness{manager: manager, server: server, client: client, prefix: prefix, loginURL: prefix + "/auth/login"}
			target := prefix + "/api/rooms/aaaaaaaaaaaa/clone"
			owner := h.login(t, prefix+"/")
			res, body := h.request(t, "POST", prefix+"/api/agent/rooms/source/keys", `{}`, http.Header{"Origin": {server.URL}}, owner)
			expectStatus(t, res, http.StatusCreated, body)
			var issued struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal([]byte(body), &issued); err != nil {
				t.Fatal(err)
			}
			for _, authorization := range []string{"", "Bearer " + issued.Key, basicHeader(testOwner, testPassword)} {
				res, body := h.request(t, "POST", target, `{"name":"new-room"}`, http.Header{"Origin": {server.URL}, "Authorization": {authorization}}, nil)
				expectStatus(t, res, http.StatusUnauthorized, body)
			}
			for _, headers := range []http.Header{{"Origin": {"https://foreign.example"}}, {"Origin": {"null"}}, {"Sec-Fetch-Site": {"cross-site"}}} {
				res, body := h.request(t, "POST", target, `{"name":"new-room"}`, headers, owner)
				expectStatus(t, res, http.StatusForbidden, body)
			}
			if rooms.calls.Load() != 0 {
				t.Fatal("unauthorized or cross-origin request reached clone")
			}
			res, body = h.request(t, "POST", target, `{"name":"new-room"}`, http.Header{"Origin": {server.URL}}, owner)
			expectStatus(t, res, http.StatusCreated, body)
			if rooms.calls.Load() != 1 {
				t.Fatal("Owner clone did not reach room manager exactly once")
			}
			res, body = h.request(t, "POST", target, `{"name":"existing"}`, http.Header{"Origin": {server.URL}}, owner)
			expectStatus(t, res, http.StatusConflict, body)
		})
	}
}
