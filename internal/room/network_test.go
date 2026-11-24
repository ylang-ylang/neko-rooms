package room

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	dockerContainer "github.com/docker/docker/api/types/container"
	dockerFilters "github.com/docker/docker/api/types/filters"
	dockerNetwork "github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

const relayICE = `[{"urls":["turns:turn.example.test:443?transport=tcp"],"username":"relay-user","credential":"relay-secret"}]`

// Capture the JSON sent by the real Docker SDK, not an independently constructed
// host config. The daemon implements only the endpoints these tests exercise.
type createPayload struct {
	dockerContainer.Config
	HostConfig       *dockerContainer.HostConfig
	NetworkingConfig *dockerNetwork.NetworkingConfig
}

type fakeDocker struct {
	mu              sync.Mutex
	containers      map[string]dockerContainer.InspectResponse
	created         []createPayload
	requests        []string
	imageAPI        string
	imageEntrypoint []string
	imageCmd        []string
	imageEnv        []string
	imageWorkingDir string
}

func newNetworkManager(t *testing.T, cfg *config.Room) (*RoomManagerCtx, *fakeDocker) {
	t.Helper()
	fake := &fakeDocker{containers: map[string]dockerContainer.InspectResponse{}, imageAPI: "3", imageCmd: []string{"/usr/bin/supervisord", "-c", "/etc/neko/supervisord.conf"}}
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(server.Close)
	client, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.51"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &RoomManagerCtx{config: cfg, client: client, logger: zerolog.Nop(), events: &events{roomsReady: map[string]struct{}{}}}, fake
}

func (f *fakeDocker) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.51")
	f.requests = append(f.requests, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": map[string]string{"net.m1k1o.neko.api-version": f.imageAPI}, "Entrypoint": f.imageEntrypoint, "Cmd": f.imageCmd, "Env": f.imageEnv, "WorkingDir": f.imageWorkingDir}})
	case r.Method == "GET" && path == "/containers/json":
		filters, err := dockerFilters.FromJSON(r.URL.Query().Get("filters"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		result := []dockerContainer.Summary{}
		for id, c := range f.containers {
			match := true
			for _, label := range filters.Get("label") {
				key, value, _ := strings.Cut(label, "=")
				match = match && c.Config.Labels[key] == value
			}
			for _, prefix := range filters.Get("id") {
				match = match && strings.HasPrefix(id, prefix)
			}
			if match {
				result = append(result, dockerContainer.Summary{ID: id, Names: []string{c.Name}, Image: c.Config.Image, Labels: c.Config.Labels, State: c.State.Status, Status: "Up 1 minute (healthy)", Mounts: c.Mounts})
			}
		}
		json.NewEncoder(w).Encode(result)
	case r.Method == "POST" && path == "/containers/create":
		var payload createPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.created = append(f.created, payload)
		id := fmt.Sprintf("%012x", len(f.created)) + strings.Repeat("a", 52)
		mounts := []dockerContainer.MountPoint{}
		for _, mount := range payload.HostConfig.Mounts {
			mounts = append(mounts, dockerContainer.MountPoint{Type: mount.Type, Source: mount.Source, Destination: mount.Target, RW: !mount.ReadOnly})
		}
		f.containers[id] = dockerContainer.InspectResponse{
			ContainerJSONBase: &dockerContainer.ContainerJSONBase{ID: id, Name: "/" + r.URL.Query().Get("name"), HostConfig: payload.HostConfig, State: &dockerContainer.State{Status: "running", Running: true}},
			Config:            &payload.Config, Mounts: mounts,
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(dockerContainer.CreateResponse{ID: id})
	case r.Method == "GET" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		for fullID, c := range f.containers {
			if strings.HasPrefix(fullID, id) {
				json.NewEncoder(w).Encode(c)
				return
			}
		}
		http.Error(w, `{"message":"not found"}`, 404)
	default:
		http.Error(w, `{"message":"unexpected fake Docker endpoint"}`, 500)
	}
}

func (f *fakeDocker) payloads() []createPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]createPayload(nil), f.created...)
}

func networkConfig(mode string, mux bool) *config.Room {
	return &config.Room{NetworkMode: mode, RelayICEServersFrontend: relayICE, InstanceName: "test-rooms", InstanceNetwork: "private-rooms", NekoImages: []string{"neko:test"}, EprMin: 59000, EprMax: 59005, Mux: mux}
}

func networkSettings(name string) types.RoomSettings {
	return types.RoomSettings{ApiVersion: 3, Name: name, NekoImage: "neko:test", MaxConnections: 2, UserPass: "member-secret", AdminPass: "admin-secret", Screen: "1920x1080@60", VideoCodec: "VP8", AudioCodec: "OPUS", ImplicitControl: true, Envs: map[string]string{"CUSTOM_ENV": "unchanged"}, Labels: map[string]string{"purpose": "test"}}
}

func envValues(t *testing.T, env []string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatal("environment entry has no separator")
		}
		if _, exists := values[key]; exists {
			t.Fatalf("duplicate environment key %s", key)
		}
		values[key] = value
	}
	return values
}

func TestNetworkModeDockerCreatePayload(t *testing.T) {
	for _, mode := range []string{"", config.NetworkModeDirect, config.NetworkModeRelay} {
		for _, mux := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mux=%t", mode, mux), func(t *testing.T) {
				cfg := networkConfig(mode, mux)
				cfg.NAT1To1IPs = []string{"203.0.113.10"}
				// Operator-added labels cannot misreport the generated policy.
				cfg.Labels = []string{networkModeLabel + "=wrong"}
				manager, fake := newNetworkManager(t, cfg)
				if _, err := manager.Create(context.Background(), networkSettings("alpha")); err != nil {
					t.Fatal(err)
				}
				payload := fake.payloads()[0]
				if len(payload.NetworkingConfig.EndpointsConfig) != 1 || payload.NetworkingConfig.EndpointsConfig[cfg.InstanceNetwork] == nil {
					t.Fatal("room is not attached to the existing private instance network")
				}
				if payload.HostConfig.PublishAllPorts || payload.HostConfig.NetworkMode == "host" {
					t.Fatal("room bypasses explicit port policy")
				}
				if payload.Labels[networkModeLabel] != cfg.EffectiveNetworkMode() || payload.Labels["m1k1o.neko_rooms.instance"] != cfg.InstanceName {
					t.Fatal("instance-scoped network metadata does not match the payload")
				}
				env := envValues(t, payload.Env)
				if env["NEKO_MEMBER_MULTIUSER_USER_PASSWORD"] != "member-secret" || env["NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD"] != "admin-secret" || env["NEKO_SESSION_API_TOKEN"] != "admin-secret" {
					t.Fatal("member/admin authentication changed")
				}
				if _, published := payload.HostConfig.PortBindings["8080/tcp"]; published {
					t.Fatal("room HTTP was published")
				}
				if _, published := payload.HostConfig.PortBindings["9222/tcp"]; published {
					t.Fatal("CDP was published")
				}
				if cfg.EffectiveNetworkMode() == config.NetworkModeRelay {
					if len(payload.HostConfig.PortBindings) != 0 {
						t.Fatal("relay room published media host ports")
					}
					if !reflect.DeepEqual([]string(payload.Entrypoint), []string{"/bin/sh", "-c"}) || len(payload.Cmd) != 2+len(fake.imageCmd) || payload.Cmd[0] != relayStartupScript || !reflect.DeepEqual([]string(payload.Cmd[2:]), fake.imageCmd) {
						t.Fatal("relay adapter did not preserve the inspected original startup argv")
					}
					if env["NEKO_WEBRTC_ICELITE"] != "false" || env["NEKO_WEBRTC_NAT1TO1"] != "" || env["NEKO_WEBRTC_ICESERVERS_FRONTEND"] != relayICE || env["NEKO_WEBRTC_ICESERVERS_BACKEND"] != "[]" || env["NEKO_LOG_LEVEL"] != "warn" {
						t.Fatal("relay room did not receive authoritative full/private ICE and frontend TURN settings")
					}
				} else {
					want := nat.PortMap{"59000/udp": {{HostIP: "0.0.0.0", HostPort: "59000"}}}
					if mux {
						want["59000/tcp"] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "59000"}}
					} else {
						want["59001/udp"] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "59001"}}
					}
					if !reflect.DeepEqual(payload.HostConfig.PortBindings, want) || env["NEKO_WEBRTC_ICELITE"] != "true" || env["NEKO_WEBRTC_NAT1TO1"] != "203.0.113.10" {
						t.Fatal("existing direct bindings or ICE-lite/NAT policy changed")
					}
					if _, exists := env["NEKO_WEBRTC_ICESERVERS_FRONTEND"]; exists {
						t.Fatal("relay master configuration leaked into direct rooms")
					}
					if len(payload.Entrypoint) != 0 || len(payload.Cmd) != 0 {
						t.Fatal("direct image startup was overridden")
					}
				}
				if mux && (env["NEKO_WEBRTC_UDPMUX"] != "59000" || env["NEKO_WEBRTC_TCPMUX"] != "59000") {
					t.Fatal("internal mux allocation changed")
				}
				if !mux && env["NEKO_WEBRTC_EPR"] != "59000-59001" {
					t.Fatal("internal EPR allocation changed")
				}
			})
		}
	}
}

func TestRelayRoomsRoundTripLegacyAdoptionAndCapacity(t *testing.T) {
	cfg := networkConfig(config.NetworkModeRelay, false)
	cfg.StorageEnabled = true
	cfg.StorageInternal = t.TempDir()
	cfg.StorageExternal = "/srv/neko-test"
	manager, fake := newNetworkManager(t, cfg)
	ctx := context.Background()
	for _, name := range []string{"alpha", "beta"} {
		settings := networkSettings(name)
		settings.Envs["CUSTOM_ENV"] = "literal-$DO_NOT_SUBSTITUTE"
		settings.Mounts = []types.RoomMount{{Type: types.MountPrivate, HostPath: "/profile", ContainerPath: "/home/neko/.config/chromium"}, {Type: types.MountPrivate, HostPath: "/Downloads", ContainerPath: "/home/neko/Downloads"}}
		for _, mount := range settings.Mounts {
			if err := os.MkdirAll(filepath.Join(cfg.StorageInternal, privateStoragePath, name, mount.HostPath), 0755); err != nil {
				t.Fatal(err)
			}
		}
		id, err := manager.Create(ctx, settings)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := manager.GetEntryByName(ctx, name)
		if err != nil || entry.ID != id || entry.NetworkMode != config.NetworkModeRelay || entry.MaxConnections != 2 || !entry.Running || !entry.IsReady {
			t.Fatalf("bad room entry: %+v, %v", entry, err)
		}
		got, err := manager.GetSettings(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.NetworkMode != config.NetworkModeRelay || got.ApiVersion != 3 || got.UserPass != settings.UserPass || got.AdminPass != settings.AdminPass || got.Screen != settings.Screen || got.MaxConnections != 2 || !reflect.DeepEqual(got.Mounts, settings.Mounts) || !reflect.DeepEqual(got.Envs, settings.Envs) || !reflect.DeepEqual(got.Labels, settings.Labels) {
			t.Fatal("relay settings/profile/downloads/labels did not round trip")
		}
	}
	payloads := fake.payloads()
	if payloads[0].Labels["m1k1o.neko_rooms.epr.min"] != "59000" || payloads[1].Labels["m1k1o.neko_rooms.epr.min"] != "59002" {
		t.Fatal("two rooms no longer receive distinct internal ranges")
	}
	// Adopt a container created before network_mode existed, even when the
	// manager's new-creation policy is relay. Its existing bindings stay intact.
	legacySettings := networkSettings("legacy")
	legacyEnv, err := legacySettings.ToEnv(&config.Room{}, types.PortSettings{FrontendPort: 8080, EprMin: 59004, EprMax: 59005})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	legacy := dockerContainer.InspectResponse{ContainerJSONBase: &dockerContainer.ContainerJSONBase{ID: strings.Repeat("b", 64), Name: "/test-rooms-legacy", HostConfig: &dockerContainer.HostConfig{PortBindings: nat.PortMap{"59004/udp": {{HostPort: "59004"}}}}, State: &dockerContainer.State{Status: "running", Running: true}}, Config: &dockerContainer.Config{Image: "neko:test", Env: legacyEnv, Labels: map[string]string{"m1k1o.neko_rooms.name": "legacy", "m1k1o.neko_rooms.instance": cfg.InstanceName, "m1k1o.neko_rooms.neko_image": "neko:test", "m1k1o.neko_rooms.api_version": "3", "m1k1o.neko_rooms.epr.min": "59004", "m1k1o.neko_rooms.epr.max": "59005"}}}
	fake.containers[legacy.ID] = legacy
	foreign := legacy
	foreign.ContainerJSONBase = &dockerContainer.ContainerJSONBase{ID: strings.Repeat("f", 64), State: legacy.State, HostConfig: legacy.HostConfig}
	foreign.Config = &dockerContainer.Config{Labels: map[string]string{"m1k1o.neko_rooms.instance": "another-instance"}}
	fake.containers[foreign.ID] = foreign
	fake.mu.Unlock()
	entries, err := manager.List(ctx, nil)
	if err != nil || len(entries) != 3 {
		t.Fatalf("list/adoption lost rooms or instance scope: %v %v", entries, err)
	}
	entry, err := manager.GetEntry(ctx, legacy.ID[:12])
	if err != nil || entry.NetworkMode != config.NetworkModeDirect {
		t.Fatal("legacy room was relabeled with the new manager policy")
	}
	if legacySettings, err := manager.GetSettings(ctx, legacy.ID[:12]); err != nil || legacySettings.NetworkMode != config.NetworkModeDirect {
		t.Fatal("legacy settings did not retain direct mode")
	}
	if manager.Config().Connections != 6 || manager.Config().NetworkMode != config.NetworkModeRelay {
		t.Fatal("configuration capacity or new-room policy changed")
	}
	if _, err := manager.Create(ctx, networkSettings("full")); err == nil || !strings.Contains(err.Error(), "not enough ports") || len(fake.payloads()) != 2 {
		t.Fatal("internal capacity was silently changed or overflow room created")
	}
	compose, err := manager.ExportAsDockerCompose(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Services map[string]struct {
			Ports       []string
			Environment []string
			Labels      []string
			Entrypoint  []string
			Command     []string
		}
	}
	if err := yaml.Unmarshal(compose, &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Services["test-rooms-alpha"].Ports) != 0 || len(exported.Services["test-rooms-beta"].Ports) != 0 || len(exported.Services["test-rooms-legacy"].Ports) != 1 {
		t.Fatal("compose did not preserve actual relay/legacy bindings")
	}
	alpha := exported.Services["test-rooms-alpha"]
	if len(alpha.Environment) == 0 || len(alpha.Labels) == 0 {
		t.Fatal("compose lost room configuration")
	}
	if !reflect.DeepEqual(alpha.Entrypoint, []string(payloads[0].Entrypoint)) || len(alpha.Command) != len(payloads[0].Cmd) || !strings.Contains(strings.Join(alpha.Environment, "\n"), "CUSTOM_ENV=literal-$$DO_NOT_SUBSTITUTE") {
		t.Fatal("compose lost the relay adapter or failed to protect literal environment values")
	}
	for i, arg := range alpha.Command {
		if strings.ReplaceAll(arg, "$$", "$") != payloads[0].Cmd[i] {
			t.Fatal("Compose interpolation would modify the relay startup argv")
		}
	}
	if len(exported.Services["test-rooms-legacy"].Command) != 0 || len(exported.Services["test-rooms-legacy"].Entrypoint) != 0 {
		t.Fatal("export changed legacy direct image startup")
	}
	data, _ := json.Marshal(manager.Config())
	if strings.Contains(string(data), "relay-secret") || strings.Contains(string(data), "ice_servers") {
		t.Fatal("public rooms config disclosed relay master configuration")
	}
}

func TestInvalidNetworkingFailsBeforeDockerCreate(t *testing.T) {
	for _, test := range []struct {
		name, mode, ice, imageAPI string
		apiVersion                int
	}{
		{"unknown mode", "typo", relayICE, "3", 3},
		{"bad ICE", "relay", "redacted-secret", "3", 3},
		{"explicit V2", "relay", relayICE, "2", 2},
		{"detected V2", "relay", relayICE, "2", 0},
		{"unsupported API", "direct", relayICE, "4", 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := networkConfig(test.mode, false)
			cfg.RelayICEServersFrontend = test.ice
			manager, fake := newNetworkManager(t, cfg)
			fake.imageAPI = test.imageAPI
			settings := networkSettings("invalid")
			settings.ApiVersion = test.apiVersion
			if _, err := manager.Create(context.Background(), settings); err == nil || strings.Contains(err.Error(), "redacted-secret") {
				t.Fatal("invalid networking was accepted or error disclosed config input")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.created) != 0 {
				t.Fatal("invalid network/API configuration reached Docker create")
			}
			for _, request := range fake.requests {
				if strings.Contains(request, "/containers/") {
					t.Fatal("invalid network/API configuration reached allocation or creation")
				}
			}
		})
	}
}

func TestDirectV2AndLegacyLookupSurviveRelayPolicySwitch(t *testing.T) {
	cfg := networkConfig("", false)
	cfg.RelayICEServersFrontend = ""
	cfg.NAT1To1IPs = []string{"203.0.113.12"}
	manager, fake := newNetworkManager(t, cfg)
	settings := networkSettings("legacy-v2")
	settings.ApiVersion = 2
	id, err := manager.Create(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	payload := fake.payloads()[0]
	env := envValues(t, payload.Env)
	if env["NEKO_ICELITE"] != "true" || env["NEKO_NAT1TO1"] != "203.0.113.12" || env["NEKO_PASSWORD"] != settings.UserPass || env["NEKO_PASSWORD_ADMIN"] != settings.AdminPass || len(payload.HostConfig.PortBindings) != 2 {
		t.Fatal("legacy V2 direct networking or passwords changed")
	}
	if _, hasVersion := payload.Labels["m1k1o.neko_rooms.api_version"]; hasVersion {
		t.Fatal("V2 label compatibility changed")
	}
	fake.mu.Lock()
	delete(fake.containers[id+strings.Repeat("a", 52)].Config.Labels, networkModeLabel)
	fake.mu.Unlock()
	// Only future creation changes. Looking up or inspecting an adopted legacy
	// room must not rewrite it or reinterpret its API, ranges or network mode.
	cfg.NetworkMode = config.NetworkModeRelay
	cfg.RelayICEServersFrontend = relayICE
	entry, err := manager.GetEntryByName(context.Background(), "legacy-v2")
	if err != nil || entry.ID != id || entry.NetworkMode != config.NetworkModeDirect || entry.MaxConnections != 2 {
		t.Fatal("legacy stable-name lookup changed after policy switch")
	}
	got, err := manager.GetSettings(context.Background(), id)
	if err != nil || got.ApiVersion != 2 || got.NetworkMode != config.NetworkModeDirect || got.UserPass != settings.UserPass || got.AdminPass != settings.AdminPass {
		t.Fatal("legacy V2 settings changed after policy switch")
	}
	if err := manager.ValidateSettings(context.Background(), *got); err == nil || !strings.Contains(err.Error(), "version 3") {
		t.Fatal("V2 recreation was not rejected under the new relay policy")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 1 || len(fake.containers[id+strings.Repeat("a", 52)].HostConfig.PortBindings) != 2 {
		t.Fatal("changing network policy mutated an existing container")
	}
}
