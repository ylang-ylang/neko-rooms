package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/m1k1o/neko-rooms/internal/types"
)

func profileErrorStatus(err error) int {
	switch {
	case errors.Is(err, types.ErrProfileInUse), errors.Is(err, types.ErrRoomNameConflict):
		return http.StatusConflict
	case errors.Is(err, types.ErrProfileNotFound), errors.Is(err, types.ErrRoomNotFound):
		return http.StatusNotFound
	case errors.Is(err, types.ErrInvalidRoomSettings):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
func (manager *ApiManagerCtx) profilesList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	profiles, err := manager.rooms.ListProfiles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), profileErrorStatus(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(profiles)
}
func (manager *ApiManagerCtx) profileCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request *types.ProfileCreateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request == nil {
		http.Error(w, "expected a profile name", 400)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "expected one profile request", 400)
		return
	}
	profile, err := manager.rooms.CreateProfile(r.Context(), request.Name)
	if err != nil {
		http.Error(w, err.Error(), profileErrorStatus(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(profile)
}
func (manager *ApiManagerCtx) profileDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := manager.rooms.DeleteProfile(r.Context(), chi.URLParam(r, "profileId")); err != nil {
		http.Error(w, err.Error(), profileErrorStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
