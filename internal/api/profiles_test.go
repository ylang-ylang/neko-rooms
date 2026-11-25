package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type profileAPIRooms struct {
	types.RoomManager
	err    error
	called bool
}

func (rooms *profileAPIRooms) ListProfiles(context.Context) ([]types.BrowserProfile, error) {
	return []types.BrowserProfile{{ID: "stable-profile", Name: "Retained profile"}}, rooms.err
}
func (rooms *profileAPIRooms) CreateProfile(_ context.Context, name string) (*types.BrowserProfile, error) {
	rooms.called = true
	return &types.BrowserProfile{ID: "stable-profile", Name: name}, rooms.err
}
func (rooms *profileAPIRooms) DeleteProfile(context.Context, string) error {
	rooms.called = true
	return rooms.err
}
func TestProfileAPIContractsAndBindingConflicts(t *testing.T) {
	for _, fixture := range []struct {
		method, path, body string
		err                error
		status             int
		called             bool
	}{
		{"GET", "/profiles", "", nil, 200, false},
		{"POST", "/profiles", `{"name":"Work account"}`, nil, 201, true},
		{"POST", "/profiles", `{"name":"x","cookies":[]}`, nil, 400, false},
		{"POST", "/profiles", `null`, nil, 400, false},
		{"POST", "/profiles", `{"name":"x"}{}`, nil, 400, false},
		{"DELETE", "/profiles/stable-profile", "", nil, 204, true},
		{"DELETE", "/profiles/stable-profile", "", types.ErrProfileInUse, 409, true},
		{"DELETE", "/profiles/missing", "", types.ErrProfileNotFound, 404, true},
	} {
		t.Run(fixture.method+fixture.path+fixture.body, func(t *testing.T) {
			rooms := &profileAPIRooms{err: fixture.err}
			manager := &ApiManagerCtx{rooms: rooms}
			router := chi.NewRouter()
			router.Get("/profiles", manager.profilesList)
			router.Post("/profiles", manager.profileCreate)
			router.Delete("/profiles/{profileId}", manager.profileDelete)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(fixture.method, fixture.path, strings.NewReader(fixture.body)))
			if response.Code != fixture.status || rooms.called != fixture.called {
				t.Fatalf("HTTP %d, called %t", response.Code, rooms.called)
			}
			if fixture.status == 200 {
				var metadata []map[string]any
				if json.Unmarshal(response.Body.Bytes(), &metadata) != nil || metadata[0]["id"] != "stable-profile" {
					t.Fatal("profile list lost stable metadata")
				}
				for field := range metadata[0] {
					if field != "id" && field != "name" && field != "created" && field != "room_id" && field != "room_name" {
						t.Fatal("list exposed private profile metadata")
					}
				}
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("profile metadata can be cached")
			}
		})
	}
	for _, err := range []error{types.ErrProfileInUse, types.ErrRoomNameConflict} {
		if roomSettingsErrorStatus(err) != 409 {
			t.Fatal("Create/Start profile conflicts are not HTTP 409")
		}
	}
}
