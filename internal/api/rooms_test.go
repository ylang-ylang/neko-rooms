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

func (f *recreateRooms) Recreate(ctx context.Context, id string, settings types.RoomSettings, start bool) (*types.RoomEntry, error) {
	if err := f.ValidateSettings(ctx, settings); err != nil {
		return nil, err
	}
	if err := f.Remove(ctx, id); err != nil {
		return nil, err
	}
	replacement, err := f.Create(ctx, settings)
	if err != nil {
		return nil, err
	}
	return f.GetEntry(ctx, replacement)
}

func TestRecreateValidatesNetworkingBeforeRemovingExistingRoom(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"supported", nil, http.StatusOK},
		{"policy rejection", fmt.Errorf("%w: relay networking requires Neko API version 3", types.ErrInvalidRoomSettings), http.StatusBadRequest},
		{"Docker inspect failure", fmt.Errorf("Docker image inspection failed"), http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			rooms := &recreateRooms{validationError: test.err}
			manager := &ApiManagerCtx{rooms: rooms}
			router := chi.NewRouter()
			router.Post("/rooms/{roomId}/recreate", manager.roomRecreate)
			request := httptest.NewRequest("POST", "/rooms/aaaaaaaaaaaa/recreate?start=false", nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("HTTP %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if test.err != nil {
				if strings.Join(rooms.calls, ",") != "validate" || !strings.Contains(response.Body.String(), test.err.Error()) {
					t.Fatalf("rejected recreation changed the existing room: HTTP %d, calls %v", response.Code, rooms.calls)
				}
			} else if strings.Join(rooms.calls, ",") != "validate,remove,create" {
				t.Fatalf("supported explicit recreation changed: HTTP %d, calls %v", response.Code, rooms.calls)
			}
		})
	}
}
