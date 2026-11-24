package room

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestRelayStartupNativeConfigPermissionsArgvAndPID(t *testing.T) {
	manager, fake := newNetworkManager(t, networkConfig(config.NetworkModeRelay, false))
	fake.imageEntrypoint = []string{"/bin/sh", "-c"}
	fake.imageCmd = []string{`printf '%s\n' "$$" "$NEKO_CONFIG" "$0" "$@"`, "original-argv0", "argument with spaces", "literal-$VALUE", "-leading-dash"}
	if _, err := manager.Create(context.Background(), networkSettings("adapter")); err != nil {
		t.Fatal(err)
	}
	payload := fake.payloads()[0]
	if !reflect.DeepEqual([]string(payload.Cmd[2:]), append(append([]string{}, fake.imageEntrypoint...), fake.imageCmd...)) {
		t.Fatal("the original image argv were modified")
	}
	var previousPath string
	for range 2 {
		args := append([]string{}, payload.Entrypoint[1:]...)
		args = append(args, payload.Cmd...)
		// A root-created 0600 mktemp file is not readable by the Neko service.
		// Force a restrictive inherited umask to exercise the explicit chmod.
		args[1] = "umask 077\n" + args[1]
		command := exec.Command(payload.Entrypoint[0], args...)
		command.Dir = t.TempDir()
		output, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
		if len(lines) != 2+len(fake.imageCmd)-1 || !reflect.DeepEqual(lines[2:], fake.imageCmd[1:]) {
			t.Fatalf("startup did not preserve argv literally: %q", output)
		}
		pid, err := strconv.Atoi(lines[0])
		if err != nil || pid != command.Process.Pid {
			t.Fatal("startup did not exec the original process as the same PID")
		}
		file := lines[1]
		if !strings.HasPrefix(file, "/tmp/neko-rooms-relay.") || !strings.HasSuffix(file, ".yaml") || file == previousPath {
			t.Fatal("native config path is not unique in the container temporary layer")
		}
		previousPath = file
		t.Cleanup(func() { os.Remove(file) })
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0444 {
			t.Fatal("native config is not readable after supervisord drops UID")
		}
		contents, err := os.ReadFile(file)
		if err != nil || string(contents) != "webrtc:\n  ip_retrieval_url: \"\"\n  nat1to1: []\n" || strings.Contains(string(contents), "secret") {
			t.Fatal("native config is not limited to the nonsecret network policy")
		}

		// Reproduce the important pinned-Viper behavior: empty environment is
		// ignored, but an empty native config value overrides a flag default.
		t.Setenv("NEKO_WEBRTC_IP_RETRIEVAL_URL", "")
		t.Setenv("NEKO_WEBRTC_NAT1TO1", "")
		v := viper.New()
		v.SetEnvPrefix("NEKO")
		v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
		v.AutomaticEnv()
		v.SetDefault("webrtc.ip_retrieval_url", "https://checkip.amazonaws.com")
		v.SetDefault("webrtc.nat1to1", []string{})
		v.SetConfigFile(file)
		if err := v.ReadInConfig(); err != nil {
			t.Fatal(err)
		}
		if v.GetString("webrtc.ip_retrieval_url") != "" || len(v.GetStringSlice("webrtc.nat1to1")) != 0 {
			t.Fatal("native config did not disable public-IP discovery and NAT rewriting")
		}
	}
}

func TestRelayStartupPreservesTerminationSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", relayStartupScript, "adapter", "/bin/sh", "-c", `printf '%s\n' "$NEKO_CONFIG"; exec sleep 60`)
	command.Dir = t.TempDir()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	file, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		command.Wait()
		t.Fatal("original process did not start")
	}
	t.Cleanup(func() { os.Remove(strings.TrimSpace(file)) })
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("original startup did not receive SIGTERM: %v", err)
	}
}

func TestRelayStartupRefusesExistingNativeConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "neko.yaml"), []byte("user-config-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", relayStartupScript, "adapter", "/bin/sh", "-c", `printf original-startup-ran`)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "refuses an existing native Neko config") || strings.Contains(string(output), "user-config-secret") || strings.Contains(string(output), "original-startup-ran") {
		t.Fatal("an inherited native config was silently hidden or startup continued")
	}
}

func TestRelayRejectsCustomNativeConfigBeforeCreatingContainer(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings func(*types.RoomSettings)
		image    func(*fakeDocker)
	}{
		{"custom config env", func(s *types.RoomSettings) { s.Envs["NEKO_CONFIG"] = "/custom-secret.yaml" }, nil},
		{"malformed custom config env", func(s *types.RoomSettings) { s.Envs["NEKO_CONFIG=/custom-secret.yaml"] = "" }, nil},
		{"config file mount", func(s *types.RoomSettings) {
			s.Mounts = []types.RoomMount{{Type: types.MountPublic, HostPath: "/safe/config.yaml", ContainerPath: "/etc/neko/neko.yaml"}}
		}, nil},
		{"config parent mount", func(s *types.RoomSettings) {
			s.Mounts = []types.RoomMount{{Type: types.MountPublic, HostPath: "/safe/etc", ContainerPath: "/etc"}}
		}, nil},
		{"cwd config mount", func(s *types.RoomSettings) {
			s.Mounts = []types.RoomMount{{Type: types.MountPublic, HostPath: "/safe/config.yaml", ContainerPath: "/home/neko/neko.yaml"}}
		}, func(f *fakeDocker) { f.imageWorkingDir = "/home/neko" }},
		{"image custom config", nil, func(f *fakeDocker) { f.imageEnv = []string{"NEKO_CONFIG=/custom-secret.yaml"} }},
		{"missing startup argv", nil, func(f *fakeDocker) { f.imageCmd = nil }},
		{"V2 image despite V3 request", nil, func(f *fakeDocker) { f.imageAPI = "2" }},
		{"native config CLI", nil, func(f *fakeDocker) {
			f.imageCmd = []string{"/usr/bin/neko", "serve", "--config", "/custom-secret.yaml"}
		}},
		{"native NAT CLI", nil, func(f *fakeDocker) { f.imageCmd = []string{"/usr/bin/neko", "serve", "--webrtc.nat1to1=203.0.113.99"} }},
		{"native IP retrieval CLI", nil, func(f *fakeDocker) {
			f.imageCmd = []string{"/usr/bin/neko", "serve", "--webrtc.ip_retrieval_url=https://custom-secret.example.test"}
		}},
		{"native frontend ICE CLI", nil, func(f *fakeDocker) {
			f.imageCmd = []string{"/usr/bin/neko", "serve", "--webrtc.iceservers.frontend=[]"}
		}},
		{"legacy network CLI", nil, func(f *fakeDocker) { f.imageCmd = []string{"/usr/bin/neko", "serve", "--icelite"} }},
		{"quoted shell-form network CLI", nil, func(f *fakeDocker) {
			f.imageCmd = []string{"/bin/sh", "-c", "exec /usr/bin/neko serve '--webrtc.icelite=true'"}
		}},
		{"shell-form network CLI and separator", nil, func(f *fakeDocker) {
			f.imageCmd = []string{"/bin/sh", "-c", "exec /usr/bin/neko serve --webrtc.icelite; printf bad"}
		}},
		{"entrypoint network CLI", nil, func(f *fakeDocker) { f.imageEntrypoint = []string{"/usr/bin/neko", "serve", "--webrtc.udpmux=59999"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := networkConfig(config.NetworkModeRelay, false)
			cfg.MountsWhitelist = []string{"/safe"}
			manager, fake := newNetworkManager(t, cfg)
			settings := networkSettings("custom")
			if test.settings != nil {
				test.settings(&settings)
			}
			if test.image != nil {
				test.image(fake)
			}
			if _, err := manager.Create(context.Background(), settings); err == nil || !strings.Contains(err.Error(), "relay networking") || strings.Contains(err.Error(), "custom-secret") {
				t.Fatal("unsupported config/startup was silently ignored or error disclosed input")
			}
			fake.mu.Lock()
			for _, request := range fake.requests {
				if strings.Contains(request, "/containers/") {
					fake.mu.Unlock()
					t.Fatal("unsupported native configuration reached allocation or creation")
				}
			}
			fake.mu.Unlock()
			if test.name == "missing startup argv" {
				return // a direct image without a command is the daemon's concern
			}
			cfg.NetworkMode = config.NetworkModeDirect
			if _, err := manager.Create(context.Background(), settings); err != nil {
				t.Fatalf("direct config compatibility changed for %s: %v", test.name, err)
			}
		})
	}
}

func TestComposeLiteralStringsProtectsInterpolation(t *testing.T) {
	values := []string{"$relay_config", `exec "$@"`, "NEKO_WEBRTC_ICESERVERS_FRONTEND=credential-$value"}
	got := composeLiteralStrings(values)
	if fmt.Sprint(values) == fmt.Sprint(got) {
		t.Fatal("Compose interpolation was not escaped")
	}
	for i, value := range got {
		if strings.ReplaceAll(value, "$$", "$") != values[i] {
			t.Fatal("Compose literal escaping changed an argument or credential")
		}
	}
}
