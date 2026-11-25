package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/m1k1o/neko-rooms/internal/types"
)

type cloneAPIRooms struct {
	types.RoomManager
	err    error
	called bool
	id     string
	name   string
}

func (rooms *cloneAPIRooms) Clone(ctx context.Context, id, name string) (*types.RoomEntry, error) {
	rooms.called, rooms.id, rooms.name = true, id, name
	return &types.RoomEntry{ID: "bbbbbbbbbbbb", Name: name, URL: "/rooms/" + name + "/", Running: true}, rooms.err
}

func TestRoomCloneNameOnlyContractAndStatuses(t *testing.T) {
	for _, test := range []struct {
		name, body, query string
		err               error
		status            int
		called            bool
	}{
		{name: "success", body: `{"name":"new-room"}`, status: 201, called: true},
		{name: "unknown settings rejected", body: `{"name":"new-room","envs":{}}`, status: 400},
		{name: "null rejected", body: `null`, status: 400},
		{name: "array rejected", body: `[]`, status: 400},
		{name: "multiple objects rejected", body: `{"name":"new-room"}{}`, status: 400},
		{name: "query rejected", body: `{"name":"new-room"}`, query: "?start=false", status: 400},
		{name: "invalid settings", body: `{"name":"x"}`, err: types.ErrInvalidRoomSettings, status: 400, called: true},
		{name: "missing source", body: `{"name":"new-room"}`, err: types.ErrRoomNotFound, status: 404, called: true},
		{name: "name conflict", body: `{"name":"new-room"}`, err: fmt.Errorf("wrapped: %w", types.ErrRoomNameConflict), status: 409, called: true},
		{name: "Docker failure", body: `{"name":"new-room"}`, err: fmt.Errorf("Docker failed"), status: 500, called: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rooms := &cloneAPIRooms{err: test.err}
			manager := &ApiManagerCtx{rooms: rooms}
			router := chi.NewRouter()
			router.Post("/rooms/{roomId}/clone", manager.roomClone)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/rooms/aaaaaaaaaaaa/clone"+test.query, strings.NewReader(test.body)))
			if response.Code != test.status || rooms.called != test.called {
				t.Fatalf("HTTP %d, want %d; called=%t", response.Code, test.status, rooms.called)
			}
			if response.Code == 201 {
				var entry types.RoomEntry
				if json.Unmarshal(response.Body.Bytes(), &entry) != nil || entry.ID != "bbbbbbbbbbbb" || entry.Name != "new-room" || !entry.Running || rooms.id != "aaaaaaaaaaaa" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("201 was not an auto-started RoomEntry from the source ID")
				}
			}
		})
	}
}
