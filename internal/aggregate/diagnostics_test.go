package aggregate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestNativeTraceRecordsFieldsWithoutPayloadSecrets(t *testing.T) {
	previousLogger, previousLevel := log.Logger, zerolog.GlobalLevel()
	defer func() { log.Logger = previousLogger; zerolog.SetGlobalLevel(previousLevel) }()
	var output bytes.Buffer
	log.Logger = zerolog.New(&output)
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	traceNative("receive", nativeMessage{Event: "signal/provide", Payload: json.RawMessage(`{"sdp":"sensitive-ice-password","iceservers":[{"credential":"sensitive-turn-password"}],"video":{"id":"main"},"audio":{"disabled":false}}`)})
	text := output.String()
	if !strings.Contains(text, `"event":"signal/provide"`) || !strings.Contains(text, `"payload_fields":["audio","iceservers","sdp","video"]`) {
		t.Fatalf("native trace lacks ordered event/field shape: %s", text)
	}
	for _, secret := range []string{"sensitive-ice-password", "sensitive-turn-password", `"id":"main"`} {
		if strings.Contains(text, secret) {
			t.Fatal("native trace logged a payload value")
		}
	}
}
