package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const testRelayICE = `[{"urls":["turns:turn.example.test:5349?transport=tcp"],"username":"relay-user","credential":"relay-secret"}]`

func newRoomConfig(t *testing.T) (*Room, *cobra.Command) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetEnvPrefix("NEKO_ROOMS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	cfg := &Room{}
	cmd := &cobra.Command{}
	if err := cfg.Init(cmd); err != nil {
		t.Fatal(err)
	}
	return cfg, cmd
}

func TestRoomNetworkingDefaultsAndEnvironmentContract(t *testing.T) {
	cfg, cmd := newRoomConfig(t)
	cfg.Set()
	if cfg.EffectiveNetworkMode() != NetworkModeDirect {
		t.Fatal("default networking is not direct")
	}
	if mode := (&Room{}).EffectiveNetworkMode(); mode != NetworkModeDirect {
		t.Fatalf("zero-value Go configuration changed: %s", mode)
	}
	if err := (&Room{}).ValidateNetworking(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NEKO_ROOMS_NETWORK_MODE", "relay")
	t.Setenv("NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND", testRelayICE)
	cfg.Set()
	if cfg.NetworkMode != NetworkModeRelay || cfg.RelayICEServersFrontend != testRelayICE {
		t.Fatal("network.mode or relay.ice_servers_frontend environment mapping changed")
	}
	if err := cmd.PersistentFlags().Set("network.mode", "direct"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.PersistentFlags().Set("relay.ice_servers_frontend", `[{"urls":"turn:localhost:3478?transport=tcp","username":"lab","credential":"lab-secret"}]`); err != nil {
		t.Fatal(err)
	}
	cfg.Set()
	if cfg.NetworkMode != NetworkModeDirect || !strings.Contains(cfg.RelayICEServersFrontend, "localhost") {
		t.Fatal("explicit CLI flags did not take precedence over environment")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "lab-secret") || strings.Contains(string(data), "ICEServers") {
		t.Fatal("serializing the room configuration disclosed relay credentials")
	}
}

func TestRelayFrontendICEValidation(t *testing.T) {
	for _, test := range []struct {
		name, ice string
		valid     bool
	}{
		{"tls", testRelayICE, true},
		{"tls default TCP", `[{"urls":"turns:turn.example.test","username":"u","credential":"p"}]`, true},
		{"plain TCP lab", `[{"urls":"turn:localhost:3478?transport=tcp","username":"u","credential":"p"}]`, true},
		{"IPv6", `[{"urls":"turn:[::1]:3478?transport=tcp","username":"u","credential":"p"}]`, true},
		{"mixed STUN TURN", `[{"urls":["stun:stun.example.test:3478"]},{"urls":["turn:turn.example.test:443?transport=tcp","turn:turn.example.test:443?transport=udp"],"username":"u","credential":"p"}]`, true},
		{"empty", "", false},
		{"bad JSON", `[{"urls":"redacted-secret"`, false},
		{"null", `null`, false},
		{"empty array", `[]`, false},
		{"not array", `{}`, false},
		{"null server", `[null]`, false},
		{"null URLs", `[{"urls":null}]`, false},
		{"empty URLs", `[{"urls":[]}]`, false},
		{"missing URLs", `[{"username":"u","credential":"redacted-secret"}]`, false},
		{"unknown field", `[{"url":"turn:localhost:3478?transport=tcp","username":"u","credential":"redacted-secret"}]`, false},
		{"numeric URL", `[{"urls":[5],"username":"u","credential":"redacted-secret"}]`, false},
		{"numeric credential", `[{"urls":["turn:localhost:3478?transport=tcp"],"username":"u","credential":2345}]`, false},
		{"trailing JSON", testRelayICE + `{}`, false},
		{"STUN only", `[{"urls":["stun:stun.example.test:3478"]}]`, false},
		{"UDP only", `[{"urls":"turn:turn.example.test:3478","username":"u","credential":"redacted-secret"}]`, false},
		{"missing username", `[{"urls":"turns:turn.example.test","credential":"redacted-secret"}]`, false},
		{"missing credential", `[{"urls":"turns:turn.example.test","username":"u"}]`, false},
		{"wrong scheme", `[{"urls":"https://redacted-secret.example.test","username":"u","credential":"p"}]`, false},
		{"wrong syntax", `[{"urls":"turns://turn.example.test","username":"u","credential":"redacted-secret"}]`, false},
		{"embedded auth", `[{"urls":"turns:u:redacted-secret@turn.example.test","username":"u","credential":"p"}]`, false},
		{"bad port", `[{"urls":"turns:turn.example.test:65536","username":"u","credential":"redacted-secret"}]`, false},
		{"port zero", `[{"urls":"turns:turn.example.test:0","username":"u","credential":"redacted-secret"}]`, false},
		{"empty port", `[{"urls":"turns:turn.example.test:","username":"u","credential":"redacted-secret"}]`, false},
		{"missing host", `[{"urls":"turns::5349","username":"u","credential":"redacted-secret"}]`, false},
		{"bad transport", `[{"urls":"turn:turn.example.test?transport=redacted-secret","username":"u","credential":"p"}]`, false},
		{"unknown query", `[{"urls":"turns:turn.example.test?credential=redacted-secret","username":"u","credential":"p"}]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Room{NetworkMode: NetworkModeRelay, RelayICEServersFrontend: test.ice}
			canonical, err := cfg.RelayFrontendICEJSON()
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, err=%v", test.valid, err)
			}
			if err != nil && (strings.Contains(err.Error(), "redacted-secret") || strings.Contains(err.Error(), "relay-secret")) {
				t.Fatal("configuration error disclosed operator input")
			}
			if (cfg.ValidateNetworking() == nil) != test.valid {
				t.Fatal("network validation disagrees with ICE validation")
			}
			if test.valid {
				var servers []struct {
					URLs []string `json:"urls"`
				}
				if err := json.Unmarshal([]byte(canonical), &servers); err != nil || len(servers) == 0 || len(servers[0].URLs) == 0 {
					t.Fatal("ICE URLs were not normalized to Neko's array format")
				}
			}
		})
	}
}

func TestInvalidRoomNetworkingFailsAtStartupWithoutSecrets(t *testing.T) {
	for _, values := range []map[string]string{
		{"network.mode": "redacted-secret"},
		{"network.mode": "relay"},
		{"network.mode": "relay", "relay.ice_servers_frontend": `[{"urls":"turns:localhost","credential":"redacted-secret"}]`},
		{"relay.ice_servers_frontend": "redacted-secret"},
	} {
		t.Run("invalid", func(t *testing.T) {
			cfg, _ := newRoomConfig(t)
			for key, value := range values {
				viper.Set(key, value)
			}
			defer func() {
				recovered := recover()
				text := fmt.Sprint(recovered)
				if recovered == nil || !strings.Contains(text, "invalid room networking configuration") || strings.Contains(text, "redacted-secret") {
					t.Fatalf("invalid configuration did not fail clearly and privately: %v", recovered)
				}
			}()
			cfg.Set()
		})
	}
}
