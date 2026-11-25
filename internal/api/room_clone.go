package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/m1k1o/neko-rooms/internal/types"
)

func (manager *ApiManagerCtx) roomClone(w http.ResponseWriter, r *http.Request) {
	var request *types.RoomCloneRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request == nil {
		http.Error(w, "expected a JSON object containing only name", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || len(r.URL.Query()) != 0 {
		http.Error(w, "expected one name-only JSON object without query parameters", http.StatusBadRequest)
		return
	}
	entry, err := manager.rooms.Clone(r.Context(), chi.URLParam(r, "roomId"), request.Name)
	if err != nil {
		status := roomSettingsErrorStatus(err)
		switch {
		case errors.Is(err, types.ErrRoomNotFound):
			status = http.StatusNotFound
		case errors.Is(err, types.ErrRoomNameConflict):
			status = http.StatusConflict
		}
		// Clone errors contain operation names, never cookie/CDP payloads.
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(entry)
}
