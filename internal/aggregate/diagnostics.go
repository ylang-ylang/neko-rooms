package aggregate

import (
	"encoding/json"
	"sort"

	"github.com/rs/zerolog/log"
)

// Trace native protocol ordering without logging SDP, ICE credentials, room
// tokens, clipboard text, member profiles or other payload values.
func traceNative(direction string, message nativeMessage) {
	event := log.Debug().Str("module", "aggregate").Str("direction", direction).Str("event", message.Event)
	if !event.Enabled() {
		return
	}
	var object map[string]json.RawMessage
	var fields []string
	if json.Unmarshal(message.Payload, &object) == nil {
		for key := range object {
			fields = append(fields, key)
		}
		sort.Strings(fields)
	}
	event.Strs("payload_fields", fields).Int("payload_bytes", len(message.Payload)).Msg("private native event")
}
