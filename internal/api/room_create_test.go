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

type createErrorRooms struct {
	types.RoomManager
	err error
}

func (f *createErrorRooms) Create(context.Context, types.RoomSettings) (string, error) {
	return "", f.err
}

func TestRoomCreateClassifiesPolicyErrorsWithoutFlatteningDockerFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"unsupported relay API", fmt.Errorf("%w: relay networking requires Neko API version 3", types.ErrInvalidRoomSettings), http.StatusBadRequest},
		{"wrapped policy rejection", fmt.Errorf("preparation: %w", types.ErrInvalidRoomSettings), http.StatusBadRequest},
		{"Docker failure mentioning relay", fmt.Errorf("Docker failed creating relay networking container"), http.StatusInternalServerError},
		{"Docker timeout", context.DeadlineExceeded, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := &ApiManagerCtx{rooms: &createErrorRooms{err: test.err}}
			router := chi.NewRouter()
			router.Post("/rooms", manager.roomCreate)
			request := httptest.NewRequest("POST", "/rooms?start=false", strings.NewReader(`{"api_version":2,"neko_image":"neko:v2"}`))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("HTTP %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}
