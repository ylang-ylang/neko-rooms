package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/m1k1o/neko-rooms/internal/types"
)

type recreateRooms struct {
	types.RoomManager
	validationError error
	calls           []string
}

func (f *recreateRooms) GetSettings(context.Context, string) (*types.RoomSettings, error) {
	return &types.RoomSettings{ApiVersion: 2, Name: "legacy", NekoImage: "neko:v2"}, nil
}

func (f *recreateRooms) ValidateSettings(_ context.Context, settings types.RoomSettings) error {
	f.calls = append(f.calls, "validate")
	if settings.ApiVersion != 2 {
		return fmt.Errorf("unexpected settings")
	}
	return f.validationError
}

func (f *recreateRooms) Remove(context.Context, string) error {
	f.calls = append(f.calls, "remove")
	return nil
}

func (f *recreateRooms) Create(context.Context, types.RoomSettings) (string, error) {
	f.calls = append(f.calls, "create")
	return "aaaaaaaaaaaa", nil
}

func (f *recreateRooms) GetEntry(context.Context, string) (*types.RoomEntry, error) {
	return &types.RoomEntry{ID: "aaaaaaaaaaaa", Name: "legacy"}, nil
}

func TestRecreateValidatesNetworkingBeforeRemovingExistingRoom(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			rooms := &recreateRooms{}
			if reject {
				rooms.validationError = fmt.Errorf("relay networking requires Neko API version 3")
			}
			manager := &ApiManagerCtx{rooms: rooms}
			router := chi.NewRouter()
			router.Post("/rooms/{roomId}/recreate", manager.roomRecreate)
			request := httptest.NewRequest("POST", "/rooms/aaaaaaaaaaaa/recreate?start=false", nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if reject {
				if response.Code != http.StatusBadRequest || strings.Join(rooms.calls, ",") != "validate" || !strings.Contains(response.Body.String(), "Neko API version 3") {
					t.Fatalf("unsupported recreation changed the existing room: HTTP %d, calls %v", response.Code, rooms.calls)
				}
			} else if response.Code != http.StatusOK || strings.Join(rooms.calls, ",") != "validate,remove,create" {
				t.Fatalf("supported explicit recreation changed: HTTP %d, calls %v", response.Code, rooms.calls)
			}
		})
	}
}
