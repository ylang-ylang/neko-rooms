package server

import (
	"net/http"

	"github.com/m1k1o/neko-rooms/internal/aggregate"
	"github.com/m1k1o/neko-rooms/internal/config"
)

// NewAggregation uses exactly the management server's existing proxy trust and
// origin comparison, but never its Owner authentication/session state.
func NewAggregation(rooms aggregate.RoomLookup, roomConfig *config.Room, serverConfig *config.Server, factory aggregate.PeerFactory) *aggregate.Gateway {
	origin := &adminAuth{config: serverConfig}
	return aggregate.New(rooms, aggregate.Options{
		PathPrefix: roomConfig.PathPrefix, Static: serverConfig.Admin.Static,
		CheckOrigin: func(r *http.Request, value string) bool { return origin.sameOrigin(r, value, false) },
	}, factory)
}
