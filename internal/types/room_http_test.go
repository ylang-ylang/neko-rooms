package types

import (
	"strings"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/config"
)

func TestHTTPNativeNetworkPolicyOwnsAliasesAndReadback(t *testing.T) {
	settings := RoomSettings{ApiVersion: 3, Envs: map[string]string{"CUSTOM_ENV": "retained"}}
	for _, key := range append(append([]string{}, relayNetworkEnvsV3...), blacklistedEnvsV3...) {
		if key != "NEKO_CONFIG" {
			settings.Envs[key] = "forbidden-override"
		}
	}
	env, err := settings.ToEnv(&config.Room{NetworkMode: "http", NAT1To1IPs: []string{"203.0.113.10"}}, PortSettings{FrontendPort: 8080, EprMin: 59000, EprMax: 59001})
	if err != nil {
		t.Fatal(err)
	}
	values := uniqueEnv(t, env)
	if values["NEKO_WEBRTC_NAT1TO1"] != "" || values["NEKO_WEBRTC_IP_RETRIEVAL_URL"] != "" || values["NEKO_WEBRTC_ICELITE"] != "true" || values["NEKO_WEBRTC_ICESERVERS_FRONTEND"] != "[]" || values["NEKO_WEBRTC_ICESERVERS_BACKEND"] != `[{"urls":[]}]` {
		t.Fatal("custom env bypassed private no-NAT/no-STUN policy")
	}
	for key, value := range values {
		if value == "forbidden-override" {
			t.Fatalf("managed env %s escaped private policy", key)
		}
	}
	readback := RoomSettings{NetworkMode: "http"}
	if err = readback.FromEnv(3, env); err != nil {
		t.Fatal(err)
	}
	if len(readback.Envs) != 1 || readback.Envs["CUSTOM_ENV"] != "retained" {
		t.Fatalf("HTTP readback exposes policy-owned environment: %v", readback.Envs)
	}
}
func TestHTTPRejectsKnownUnsupportedNativeCodecAndCookieOverrides(t *testing.T) {
	for key, value := range map[string]string{"NEKO_SESSION_COOKIE_ENABLED": "true", "NEKO_CAPTURE_VIDEO_CODEC": "h264", "NEKO_CAPTURE_AUDIO_CODEC": "g722"} {
		s := RoomSettings{ApiVersion: 3, Envs: map[string]string{key: value}}
		if _, err := s.ToEnv(&config.Room{NetworkMode: "http"}, PortSettings{}); err == nil || strings.Contains(err.Error(), value) {
			t.Fatal("unsupported custom HTTP configuration accepted or value disclosed")
		}
	}
}
