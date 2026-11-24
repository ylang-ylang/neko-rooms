package types

import (
	"strings"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/config"
)

const frontendRelayICE = `[{"urls":["turn:localhost:3478?transport=tcp"],"username":"lab","credential":"relay-secret"}]`

func uniqueEnv(t *testing.T, env []string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatal("malformed environment entry")
		}
		if _, exists := result[key]; exists {
			t.Fatalf("duplicate environment name %s", key)
		}
		result[key] = value
	}
	return result
}

func TestRelayNetworkEnvironmentIsAuthoritative(t *testing.T) {
	cfg := &config.Room{NetworkMode: config.NetworkModeRelay, RelayICEServersFrontend: frontendRelayICE, NAT1To1IPs: []string{"203.0.113.99"}}
	settings := RoomSettings{ApiVersion: 3, NetworkMode: config.NetworkModeDirect, Envs: map[string]string{"CUSTOM_ENV": "retained", "NEKO_MEMBER_MULTIUSER_USER_PASSWORD": "existing-custom-password"}}
	for _, key := range append(append([]string(nil), relayNetworkEnvsV3...), "NEKO_WEBRTC_ICELITE", "NEKO_WEBRTC_NAT1TO1", "NEKO_WEBRTC_UDPMUX", "NEKO_WEBRTC_TCPMUX", "NEKO_WEBRTC_EPR") {
		if key != "NEKO_CONFIG" {
			settings.Envs[key] = "forbidden-custom-value"
		}
	}
	settings.Envs["NEKO_WEBRTC_NAT1TO1=203.0.113.88"] = "forbidden-custom-value"
	settings.Envs["NEKO_IPFETCH=https://ip.example.test/?x"] = "forbidden-custom-value"
	env, err := settings.ToEnv(cfg, PortSettings{FrontendPort: 8080, EprMin: 59000, EprMax: 59001})
	if err != nil {
		t.Fatal(err)
	}
	values := uniqueEnv(t, env)
	for _, value := range values {
		if value == "forbidden-custom-value" {
			t.Fatal("per-room custom networking escaped the manager policy")
		}
	}
	if values["NEKO_WEBRTC_ICELITE"] != "false" || values["NEKO_WEBRTC_NAT1TO1"] != "" || values["NEKO_WEBRTC_ICESERVERS_FRONTEND"] != frontendRelayICE || values["NEKO_WEBRTC_ICESERVERS_BACKEND"] != "[]" || values["NEKO_WEBRTC_EPR"] != "59000-59001" || values["NEKO_LEGACY"] != "false" || values["NEKO_WEBRTC_UDPMUX"] != "0" || values["NEKO_WEBRTC_TCPMUX"] != "0" {
		t.Fatal("read-only settings metadata or custom env overrode relay policy")
	}
	if values["CUSTOM_ENV"] != "retained" || values["NEKO_MEMBER_MULTIUSER_USER_PASSWORD"] != "existing-custom-password" {
		t.Fatal("unrelated custom env precedence changed")
	}
	readback := RoomSettings{NetworkMode: config.NetworkModeRelay}
	if err := readback.FromEnv(3, env); err != nil {
		t.Fatal(err)
	}
	if readback.Envs["CUSTOM_ENV"] != "retained" || readback.UserPass != "existing-custom-password" {
		t.Fatal("non-network settings did not round trip")
	}
	for key := range readback.Envs {
		if IsManagedEnvV3(key, true) {
			t.Fatalf("managed network key %s leaked into editable room settings", key)
		}
	}
}

func TestDirectV3CustomICEAndOtherEnvPrecedenceRemainCompatible(t *testing.T) {
	settings := RoomSettings{ApiVersion: 3, UserPass: "field-password", Envs: map[string]string{
		"NEKO_WEBRTC_ICESERVERS_FRONTEND":     frontendRelayICE,
		"NEKO_WEBRTC_IP_RETRIEVAL_URL":        "https://ip.example.test/",
		"NEKO_MEMBER_MULTIUSER_USER_PASSWORD": "custom-password",
		"NEKO_WEBRTC_ICELITE":                 "false",
		"NEKO_WEBRTC_NAT1TO1":                 "203.0.113.99",
	}}
	env, err := settings.ToEnv(&config.Room{}, PortSettings{FrontendPort: 8080, EprMin: 59000, EprMax: 59001})
	if err != nil {
		t.Fatal(err)
	}
	values := uniqueEnv(t, env)
	if values["NEKO_WEBRTC_ICELITE"] != "true" || values["NEKO_WEBRTC_NAT1TO1"] != "" || values["NEKO_MEMBER_MULTIUSER_USER_PASSWORD"] != "custom-password" || values["NEKO_WEBRTC_ICESERVERS_FRONTEND"] != frontendRelayICE || values["NEKO_WEBRTC_IP_RETRIEVAL_URL"] != "https://ip.example.test/" {
		t.Fatal("direct mode no longer preserves existing env precedence and network controls")
	}
	readback := RoomSettings{}
	if err := readback.FromEnv(3, env); err != nil {
		t.Fatal(err)
	}
	if readback.UserPass != "custom-password" || readback.Envs["NEKO_WEBRTC_ICESERVERS_FRONTEND"] != frontendRelayICE {
		t.Fatal("direct custom ICE settings no longer round trip")
	}
}

func TestRelayLoggingDefaultAndExplicitOptIn(t *testing.T) {
	for _, level := range []string{"", "warn", "info", "debug", "error"} {
		for _, mode := range []string{config.NetworkModeDirect, config.NetworkModeRelay} {
			settings := RoomSettings{ApiVersion: 3, Envs: map[string]string{}}
			if level != "" {
				settings.Envs["NEKO_LOG_LEVEL"] = level
			}
			cfg := &config.Room{NetworkMode: mode, RelayICEServersFrontend: frontendRelayICE}
			env, err := settings.ToEnv(cfg, PortSettings{})
			if err != nil {
				t.Fatal(err)
			}
			values := uniqueEnv(t, env)
			want := level
			if mode == config.NetworkModeRelay && want == "" {
				want = "warn"
			}
			if values["NEKO_LOG_LEVEL"] != want {
				t.Fatalf("mode %s: got log level %s, want %s", mode, values["NEKO_LOG_LEVEL"], want)
			}
			readback := RoomSettings{NetworkMode: mode}
			if err := readback.FromEnv(3, env); err != nil {
				t.Fatal(err)
			}
			if level != "" && !(mode == config.NetworkModeRelay && level == "warn") && readback.Envs["NEKO_LOG_LEVEL"] != level {
				t.Fatal("explicit logging choice did not survive settings readback")
			}
		}
	}
}

func TestRelayRejectsExplicitNativeConfigWithoutChangingDirect(t *testing.T) {
	for _, key := range []string{"NEKO_CONFIG", "NEKO_CONFIG=/tmp/custom.yaml"} {
		settings := RoomSettings{ApiVersion: 3, Envs: map[string]string{key: "/custom-secret.yaml"}}
		cfg := &config.Room{NetworkMode: config.NetworkModeRelay, RelayICEServersFrontend: frontendRelayICE}
		if env, err := settings.ToEnv(cfg, PortSettings{}); err == nil || len(env) != 0 || !strings.Contains(err.Error(), "NEKO_CONFIG") || strings.Contains(err.Error(), "custom-secret") {
			t.Fatal("relay silently ignored a custom native config or disclosed its value")
		}
		cfg.NetworkMode = config.NetworkModeDirect
		env, err := settings.ToEnv(cfg, PortSettings{})
		if err != nil || !strings.Contains(uniqueEnv(t, env)["NEKO_CONFIG"], "/custom-secret.yaml") {
			t.Fatal("direct custom native configuration was changed")
		}
	}
}

func TestToEnvRejectsInvalidNetworkingAndV2Relay(t *testing.T) {
	for _, test := range []struct {
		cfg config.Room
		api int
	}{
		{config.Room{NetworkMode: "unknown"}, 3},
		{config.Room{NetworkMode: config.NetworkModeRelay, RelayICEServersFrontend: "bad-secret"}, 3},
		{config.Room{NetworkMode: config.NetworkModeRelay, RelayICEServersFrontend: frontendRelayICE}, 2},
	} {
		settings := RoomSettings{ApiVersion: test.api}
		if env, err := settings.ToEnv(&test.cfg, PortSettings{}); err == nil || len(env) != 0 || strings.Contains(err.Error(), "bad-secret") {
			t.Fatal("invalid policy generated a room environment or leaked input")
		}
	}
}
