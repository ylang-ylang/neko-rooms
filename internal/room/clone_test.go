package room

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type cloneBrowser struct {
	mu      sync.Mutex
	methods []string
	applied []map[string]json.RawMessage
	failSet bool
	failGet bool
}

func fakeCloneBrowser(t *testing.T, manager *RoomManagerCtx) *cloneBrowser {
	t.Helper()
	browser := &cloneBrowser{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://untrusted.example/devtools/browser/default"})
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if conn.ReadJSON(&request) != nil {
				return
			}
			browser.mu.Lock()
			browser.methods = append(browser.methods, request.Method)
			result := any(map[string]any{})
			failure := (request.Method == "Storage.setCookies" && browser.failSet) || (request.Method == "Storage.getCookies" && browser.failGet)
			switch request.Method {
			case "Storage.getCookies":
				result = map[string]any{"cookies": []any{map[string]any{"name": "session", "value": "private-marker", "domain": "site.test", "path": "/", "secure": true, "httpOnly": true, "sameSite": "Lax", "priority": "High", "sourceScheme": "Secure", "sourcePort": 443, "session": true, "expires": -1, "size": 20, "partitionKey": map[string]any{"topLevelSite": "https://site.test", "hasCrossSiteAncestor": false}}, map[string]any{"name": "persistent", "value": "opaque-marker", "domain": "site.test", "path": "/", "session": false, "expires": 1900000000}}}
			case "Storage.setCookies":
				var params struct {
					Cookies []map[string]json.RawMessage `json:"cookies"`
				}
				json.Unmarshal(request.Params, &params)
				browser.applied = params.Cookies
			}
			browser.mu.Unlock()
			response := map[string]any{"id": request.ID, "result": result}
			if failure {
				response = map[string]any{"id": request.ID, "error": map[string]any{"message": "must-not-expose-cookie-value"}}
			}
			if conn.WriteJSON(response) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	manager.cdpDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return browser
}

func cloneFixture(t *testing.T, mode string) (*RoomManagerCtx, *fakeDocker, string, string) {
	t.Helper()
	cfg := networkConfig(mode, false)
	cfg.StorageEnabled, cfg.StorageInternal, cfg.StorageExternal = true, t.TempDir(), "/host/managed"
	manager, fake := newNetworkManager(t, cfg)
	settings := networkSettings("source")
	settings.Mounts = []types.RoomMount{{Type: types.MountPrivate, HostPath: "/profile", ContainerPath: "/home/neko/profile"}, {Type: types.MountPrivate, HostPath: "/Downloads", ContainerPath: "/home/neko/Downloads"}}
	settings.VideoMaxFPS, settings.VideoPipeline, settings.AudioPipeline, settings.BroadcastPipeline = 37, "custom-video", "custom-audio", "custom-broadcast"
	settings.Resources = types.RoomResources{CPUShares: 512, NanoCPUs: 2500000000, ShmSize: 536870912, Memory: 1073741824}
	settings.Hostname, settings.DNS = "explicit-hostname", []string{"192.0.2.53"}
	profile, err := manager.CreateProfile(context.Background(), settings.Name)
	if err != nil {
		t.Fatal(err)
	}
	settings.ProfileID = profile.ID
	root := filepath.Join(cfg.StorageInternal, profileStoragePath, profile.ID)
	writeSnapshotFixture(t, root, "profile/Default/Cookies", "persistent identity", 0640)
	writeSnapshotFixture(t, root, "profile/Local State", "opaque profile identity", 0600)
	writeSnapshotFixture(t, root, "Downloads/download", "download", 0600)
	id, err := manager.Create(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return manager, fake, id, root
}

func TestClonePreservesConfigurationAndIsolatedManagedStorage(t *testing.T) {
	for _, mode := range []string{config.NetworkModeDirect, config.NetworkModeRelay} {
		t.Run(mode, func(t *testing.T) {
			manager, fake, source, sourceRoot := cloneFixture(t, mode)
			browser := fakeCloneBrowser(t, manager)
			original, err := manager.GetSettings(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			entry, err := manager.Clone(context.Background(), source, "target")
			if err != nil {
				t.Fatal(err)
			}
			if entry.ID == source || entry.Name != "target" || !entry.Running {
				t.Fatal("clone was not a separate auto-started room")
			}
			cloned, err := manager.GetSettings(context.Background(), entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			original.Name = "target"
			original.ProfileID, original.ProfileName = cloned.ProfileID, cloned.ProfileName
			if !reflect.DeepEqual(cloned, original) {
				t.Fatal("canonical source configuration did not survive cloning")
			}
			payloads := fake.payloads()
			if payloads[1].Labels["m1k1o.neko_rooms.epr.min"] != "59002" {
				t.Fatal("clone copied the source's allocated ports")
			}
			if mode == config.NetworkModeRelay && len(payloads[1].HostConfig.PortBindings) != 0 {
				t.Fatal("relay clone published additional host ports")
			}
			if mode == config.NetworkModeDirect && payloads[1].HostConfig.PortBindings["59002/udp"][0].HostPort != "59002" {
				t.Fatal("direct clone did not allocate unique published ports")
			}
			for _, mount := range payloads[1].HostConfig.Mounts {
				if !strings.Contains(mount.Source, "/profiles/"+entry.ProfileID+"/") || strings.Contains(mount.Source, sourceRoot+"/") {
					t.Fatal("clone reused source writable mount")
				}
			}
			targetRoot := filepath.Join(manager.config.StorageInternal, profileStoragePath, entry.ProfileID)
			writeSnapshotFixture(t, targetRoot, "profile/Default/Cookies", "target changed", 0640)
			data, _ := os.ReadFile(filepath.Join(sourceRoot, "profile/Default/Cookies"))
			if string(data) != "persistent identity" {
				t.Fatal("clone writes mutated source data")
			}
			browser.mu.Lock()
			if !reflect.DeepEqual(browser.methods, []string{"Target.getBrowserContexts", "Target.getTargets", "Storage.getCookies", "Storage.clearCookies", "Storage.setCookies"}) || len(browser.applied) != 2 {
				t.Fatal("clone did not supplement identity in existing default contexts")
			}
			session, persistent := browser.applied[0], browser.applied[1]
			if session["expires"] != nil || session["size"] != nil || session["session"] != nil || session["partitionKey"] == nil || string(session["httpOnly"]) != "true" || string(session["sameSite"]) != `"Lax"` || persistent["expires"] == nil {
				t.Fatal("CookieParam lost semantics or included read-only fields")
			}
			browser.mu.Unlock()
			fake.mu.Lock()
			requests := strings.Join(fake.requests, "\n")
			fake.mu.Unlock()
			pause, resume, start := strings.Index(requests, "/pause"), strings.Index(requests, "/unpause"), strings.LastIndex(requests, "/start")
			if pause < 0 || resume < pause || start < resume {
				t.Fatal("source was not resumed immediately after snapshot before target start")
			}
		})
	}
}

func TestCloneRetainsStoppedAndAlreadyPausedSourceState(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "paused"}[paused], func(t *testing.T) {
			manager, fake, source, _ := cloneFixture(t, config.NetworkModeRelay)
			fakeCloneBrowser(t, manager)
			fake.mu.Lock()
			for id, c := range fake.containers {
				c.State.Running, c.State.Paused = paused, paused
				c.State.Status = map[bool]string{false: "exited", true: "paused"}[paused]
				fake.containers[id] = c
			}
			fake.mu.Unlock()
			if _, err := manager.Clone(context.Background(), source, "target"); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			pauses, resumes := 0, 0
			for _, request := range fake.requests {
				if strings.HasSuffix(request, "/pause") {
					pauses++
				}
				if strings.HasSuffix(request, "/unpause") {
					resumes++
				}
				if strings.HasSuffix(request, "/stop") {
					t.Fatal("source browser was stopped")
				}
			}
			if (paused && (pauses != 1 || resumes != 1)) || (!paused && (pauses != 0 || resumes != 0)) {
				t.Fatal("paused identity capture did not resume/re-pause exactly once, or stopped source was changed")
			}
			for id, c := range fake.containers {
				if strings.HasPrefix(id, source) && (c.State.Paused != paused || c.State.Running != paused) {
					t.Fatal("source original lifecycle state was not restored")
				}
			}
		})
	}
}

func TestCloneFailuresResumeAndRollbackOnlyOwnedTarget(t *testing.T) {
	for _, failure := range []string{"copy", "cancel", "start", "cookies", "unpause"} {
		t.Run(failure, func(t *testing.T) {
			manager, fake, source, sourceRoot := cloneFixture(t, config.NetworkModeRelay)
			browser := fakeCloneBrowser(t, manager)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "start" || failure == "unpause" {
				fake.failAction = failure
			}
			if failure == "cookies" {
				browser.failSet = true
			}
			fake.onAction = func(action string) {
				if action == "pause" {
					if failure == "cancel" {
						cancel()
					}
					if failure == "copy" {
						// Force a traversal failure after Docker pause, without
						// changing or deleting any persistent source files.
						os.Chmod(filepath.Join(sourceRoot, "profile/Default/Cookies"), 0000)
					}
				}
			}
			if failure == "copy" && os.Geteuid() == 0 {
				t.Skip("unreadable-file failure requires nonroot test process")
			}
			_, err := manager.Clone(ctx, source, "target")
			if err == nil || strings.Contains(err.Error(), "must-not-expose") {
				t.Fatal("clone failure was hidden or included a secret CDP error")
			}
			if failure == "unpause" && !strings.Contains(err.Error(), "resume source") {
				t.Fatal("resume failure was not surfaced")
			}
			if _, err := os.Lstat(filepath.Join(manager.config.StorageInternal, privateStoragePath, "target")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed clone storage was left behind")
			}
			stages, _ := filepath.Glob(filepath.Join(manager.config.StorageInternal, privateStoragePath, ".clone-*"))
			if len(stages) != 0 {
				t.Fatal("failed clone left private staging data")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.containers) != 1 {
				t.Fatal("rollback removed source or retained incomplete target")
			}
			for _, c := range fake.containers {
				if c.Config.Labels["m1k1o.neko_rooms.name"] != "source" || (failure != "unpause" && c.State.Paused) {
					t.Fatal("failed clone did not preserve and resume the source")
				}
			}
		})
	}
}

func TestCloneKeepsOrphanStorageAndRejectsExternalWritableMounts(t *testing.T) {
	manager, fake, source, _ := cloneFixture(t, config.NetworkModeRelay)
	for _, name := range []string{"", "x", "../escape", "new room"} {
		if _, err := manager.Clone(context.Background(), source, name); !errors.Is(err, types.ErrInvalidRoomSettings) {
			t.Fatal("invalid clone name was accepted")
		}
	}
	if _, err := manager.Clone(context.Background(), source, "source"); !errors.Is(err, types.ErrRoomNameConflict) {
		t.Fatal("existing room name did not return conflict")
	}
	target := filepath.Join(manager.config.StorageInternal, privateStoragePath, "target")
	writeSnapshotFixture(t, target, "keep", "existing target", 0600)
	fakeCloneBrowser(t, manager)
	if entry, err := manager.Clone(context.Background(), source, "target"); err != nil || entry.ProfileID == "" {
		t.Fatal("orphaned profile incorrectly prevented an independent new profile")
	}
	data, _ := os.ReadFile(filepath.Join(target, "keep"))
	if string(data) != "existing target" {
		t.Fatal("existing storage changed")
	}
	fake.mu.Lock()
	for id, c := range fake.containers {
		c.Mounts[0].Source = "/host/managed/rooms/source-other/profile"
		fake.containers[id] = c
	}
	fake.mu.Unlock()
	if _, err := manager.Clone(context.Background(), source, "another"); !errors.Is(err, types.ErrInvalidRoomSettings) || !strings.Contains(err.Error(), "unmanaged writable") {
		t.Fatal("external writable prefix sibling was silently shared")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 2 {
		t.Fatal("invalid clone reached Docker create")
	}
}

func TestCloneSerializesSourceDeletionAndTargetNameReservation(t *testing.T) {
	manager, fake, source, _ := cloneFixture(t, config.NetworkModeRelay)
	fakeCloneBrowser(t, manager)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paused, release := make(chan struct{}), make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fake.onAction = func(action string) {
		if action == "pause" {
			pauseOnce.Do(func() {
				close(paused)
				<-release
			})
		}
	}
	cloned := make(chan error, 1)
	go func() {
		_, err := manager.Clone(ctx, source, "target")
		cloned <- err
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("clone did not reach its snapshot")
	}
	if manager.lifecycle.TryLock() {
		manager.lifecycle.Unlock()
		t.Fatal("clone snapshot does not own the shared lifecycle lock")
	}
	removed := make(chan error, 1)
	go func() { removed <- manager.Remove(ctx, source) }()
	releaseOnce.Do(func() { close(release) })
	if err := <-cloned; err != nil {
		t.Fatalf("concurrent source deletion interrupted clone: %v", err)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	entry, err := manager.GetEntryByName(ctx, "target")
	if err != nil || !entry.Running {
		t.Fatal("serialized source removal destroyed the clone")
	}
	// Two simultaneous clones of one remaining source cannot claim the same
	// name, even though both requests began before that target existed.
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := manager.Clone(ctx, entry.ID, "same-name")
			results <- err
		}()
	}
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, types.ErrRoomNameConflict)) || (second == nil && errors.Is(first, types.ErrRoomNameConflict))) {
		t.Fatalf("concurrent target name reservation failed: %v / %v", first, second)
	}
}

func TestPausedCloneFailureRestoresOriginalPause(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "CDP failure", true: "client cancellation"}[canceled], func(t *testing.T) {
			manager, fake, source, _ := cloneFixture(t, config.NetworkModeRelay)
			browser := fakeCloneBrowser(t, manager)
			browser.failGet = !canceled
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake.mu.Lock()
			for id, c := range fake.containers {
				c.State.Paused, c.State.Status = true, "paused"
				fake.containers[id] = c
			}
			fake.onAction = func(action string) {
				if action == "unpause" && canceled {
					cancel()
				}
			}
			fake.mu.Unlock()
			if _, err := manager.Clone(ctx, source, "target"); err == nil {
				t.Fatal("paused identity failure was hidden")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.containers) != 1 {
				t.Fatal("paused-source failure created an orphan clone")
			}
			for _, c := range fake.containers {
				if !c.State.Paused || !c.State.Running {
					t.Fatal("paused source was left running/unpaused after failure")
				}
			}
		})
	}
}
