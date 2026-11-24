package room

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestHTTPNetworkNoBindingsPrivateICEAndRelativeRoundTrip(t *testing.T) {
	for _, mux := range []bool{false, true} {
		t.Run(map[bool]string{false: "epr", true: "mux"}[mux], func(t *testing.T) {
			cfg := networkConfig(config.NetworkModeHTTP, mux)
			cfg.RelayICEServersFrontend = ""
			cfg.NAT1To1IPs = []string{"203.0.113.99"}
			cfg.PathPrefix = "/room"
			cfg.InstanceUrl, _ = url.Parse("http://192.0.2.1:8080/")
			cfg.Traefik.Enabled = true
			manager, fake := newNetworkManager(t, cfg)
			settings := networkSettings("http-viewer")
			settings.Envs["CUSTOM_ENV"] = "literal-$VALUE"
			id, err := manager.Create(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			payload := fake.payloads()[0]
			if len(payload.HostConfig.PortBindings) != 0 || payload.HostConfig.PublishAllPorts || len(payload.NetworkingConfig.EndpointsConfig) != 1 {
				t.Fatal("http room escaped private instance networking")
			}
			if payload.Labels["traefik.enable"] != "false" || payload.Labels["m1k1o.neko_rooms.proxy.enabled"] != "true" {
				t.Fatal("http room bypasses Manager via native proxy routing")
			}
			env := envValues(t, payload.Env)
			if env["NEKO_WEBRTC_ICELITE"] != "true" || env["NEKO_WEBRTC_NAT1TO1"] != "" || env["NEKO_WEBRTC_IP_RETRIEVAL_URL"] != "" || env["NEKO_WEBRTC_ICESERVERS_FRONTEND"] != "[]" || env["NEKO_WEBRTC_ICESERVERS_BACKEND"] != `[{"urls":[]}]` || env["NEKO_LEGACY"] != "false" {
				t.Fatal("http room enabled public ICE/NAT or lost the no-STUN sentinel")
			}
			if mux {
				if env["NEKO_WEBRTC_UDPMUX"] != "59000" || env["NEKO_WEBRTC_TCPMUX"] != "59000" {
					t.Fatal("private mux disabled")
				}
			} else if env["NEKO_WEBRTC_EPR"] != "59000-59001" {
				t.Fatal("private EPR disabled")
			}
			if len(payload.Cmd) < 3 || payload.Cmd[0] != strings.ReplaceAll(relayStartupScript, "relay", "http") || !reflect.DeepEqual([]string(payload.Cmd[2:]), fake.imageCmd) {
				t.Fatal("http NAT-free startup adapter lost original image argv")
			}
			entry, err := manager.GetEntryByName(context.Background(), settings.Name)
			if err != nil || entry.URL != "/room/http-viewer/" || entry.NetworkMode != "http" {
				t.Fatalf("room URL isn't proxy-transparent: %+v %v", entry, err)
			}
			readback, err := manager.GetSettings(context.Background(), id)
			if err != nil || readback.NetworkMode != "http" || !reflect.DeepEqual(readback.Envs, settings.Envs) || readback.UserPass != settings.UserPass || readback.AdminPass != settings.AdminPass {
				t.Fatalf("http readback changed credentials/custom env: %+v %v", readback, err)
			}
			compose, err := manager.ExportAsDockerCompose(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var exported struct {
				Services map[string]struct {
					Ports                            []string
					Command, Entrypoint, Environment []string
				}
			}
			if yaml.Unmarshal(compose, &exported) != nil {
				t.Fatal("invalid Compose")
			}
			service := exported.Services["test-rooms-http-viewer"]
			if len(service.Ports) != 0 || len(service.Command) != len(payload.Cmd) || !strings.Contains(strings.Join(service.Environment, "\n"), "CUSTOM_ENV=literal-$$VALUE") {
				t.Fatal("Compose lost private adapter/escaping or publishes ports")
			}
			for i, arg := range service.Command {
				if strings.ReplaceAll(arg, "$$", "$") != payload.Cmd[i] {
					t.Fatal("Compose changes startup arguments")
				}
			}
		})
	}
}
func TestHTTPValidationBeforeAllocationAndRemove(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*types.RoomSettings)
	}{
		{"v2", func(s *types.RoomSettings) { s.ApiVersion = 2 }},
		{"cookie mode", func(s *types.RoomSettings) { s.Envs["NEKO_SESSION_COOKIE_ENABLED"] = "true" }},
		{"custom config", func(s *types.RoomSettings) { s.Envs["NEKO_CONFIG"] = "/custom-secret.yaml" }},
		{"video codec", func(s *types.RoomSettings) { s.VideoCodec = "H264" }},
		{"audio codec", func(s *types.RoomSettings) { s.AudioCodec = "G722" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := networkConfig(config.NetworkModeHTTP, false)
			cfg.RelayICEServersFrontend = ""
			manager, fake := newNetworkManager(t, cfg)
			settings := networkSettings("invalid")
			test.edit(&settings)
			if err := manager.ValidateSettings(context.Background(), settings); !errors.Is(err, types.ErrInvalidRoomSettings) {
				t.Fatalf("unsafe recreate policy not classified: %v", err)
			}
			if _, err := manager.Create(context.Background(), settings); !errors.Is(err, types.ErrInvalidRoomSettings) {
				t.Fatalf("unsafe create policy not classified: %v", err)
			}
			for _, request := range fake.requests {
				if strings.Contains(request, "/containers/") {
					t.Fatal("invalid http policy reached room allocation/removal")
				}
			}
		})
	}
}
func TestHTTPStartupExecutesOriginalImageWithoutPublicIPFetch(t *testing.T) {
	cfg := networkConfig(config.NetworkModeHTTP, false)
	cfg.RelayICEServersFrontend = ""
	manager, fake := newNetworkManager(t, cfg)
	fake.imageCmd = []string{"/bin/sh", "-c", `printf '%s\n' "$NEKO_CONFIG" "$0" "$@"`, "original", "literal-$VALUE", "argument with spaces"}
	if _, err := manager.Create(context.Background(), networkSettings("startup")); err != nil {
		t.Fatal(err)
	}
	payload := fake.payloads()[0]
	args := append([]string{}, payload.Entrypoint[1:]...)
	args = append(args, payload.Cmd...)
	cmd := exec.Command(payload.Entrypoint[0], args...)
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 4 || !reflect.DeepEqual(lines[1:], []string{"original", "literal-$VALUE", "argument with spaces"}) {
		t.Fatalf("original image argv lost: %q", out)
	}
	defer os.Remove(lines[0])
	data, err := os.ReadFile(lines[0])
	if err != nil || string(data) != "webrtc:\n  ip_retrieval_url: \"\"\n  nat1to1: []\n" {
		t.Fatal("native config did not disable NAT/public IP lookup")
	}
}
