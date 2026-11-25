package room

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestProfilesSurviveRemovalRestartAndExternalDisappearance(t *testing.T) {
	manager, fake, id, root := cloneFixture(t, config.NetworkModeDirect)
	ctx := context.Background()
	entry, err := manager.GetEntry(ctx, id)
	if err != nil || entry.ProfileID == "" {
		t.Fatal("deployment has no durable profile")
	}
	settings, err := manager.GetSettings(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, *settings); !errors.Is(err, types.ErrRoomNameConflict) {
		t.Fatal("duplicate deployment name accepted")
	}
	settings.Name = "replacement"
	if _, err := manager.Create(ctx, *settings); !errors.Is(err, types.ErrProfileInUse) {
		t.Fatal("a second writable binding was accepted")
	}
	if err := manager.DeleteProfile(ctx, entry.ProfileID); !errors.Is(err, types.ErrProfileInUse) {
		t.Fatal("bound profile deletion accepted")
	}
	if err := manager.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "profile/Local State")); string(data) != "opaque profile identity" {
		t.Fatal("deployment removal deleted profile data")
	}
	// No RAM registry is reused: a fresh manager loads metadata from disk.
	restarted := &RoomManagerCtx{config: manager.config, client: manager.client, events: manager.events}
	profiles, err := restarted.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].ID != entry.ProfileID || profiles[0].RoomID != "" {
		t.Fatal("profile identity/binding did not survive manager restart")
	}
	replacement, err := restarted.Create(ctx, *settings)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.GetSettings(ctx, replacement)
	if err != nil || got.ProfileID != entry.ProfileID {
		t.Fatal("replacement did not reuse selected profile")
	}
	// Simulate an external docker rm, not a product delete callback.
	fake.mu.Lock()
	for fullID := range fake.containers {
		if strings.HasPrefix(fullID, replacement) {
			delete(fake.containers, fullID)
		}
	}
	fake.mu.Unlock()
	profiles, err = restarted.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].RoomID != "" {
		t.Fatal("externally removed deployment left a permanent busy lease")
	}
	if err := restarted.DeleteProfile(ctx, entry.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit profile deletion did not delete owned storage")
	}
}

func TestProfilesAdoptLegacyInPlaceAndDoNotCollideWithNames(t *testing.T) {
	manager, fake, id, sourceRoot := cloneFixture(t, config.NetworkModeDirect)
	ctx := context.Background()
	// Model a pre-feature deployment with no profile label or registry. Moving
	// fixture data here happens before adoption, never in the implementation.
	legacyRoot := filepath.Join(manager.config.StorageInternal, "rooms", "source")
	if err := os.MkdirAll(filepath.Dir(legacyRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sourceRoot, legacyRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manager.registryPath()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	for fullID, container := range fake.containers {
		delete(container.Config.Labels, profileLabel)
		for i := range container.Mounts {
			container.Mounts[i].Source = filepath.Join(manager.config.StorageExternal, "rooms", "source", filepath.Base(container.Mounts[i].Source))
		}
		fake.containers[fullID] = container
	}
	before := len(fake.requests)
	fake.mu.Unlock()
	profiles, err := manager.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].RoomID != id {
		t.Fatal("legacy profile was not adopted")
	}
	fake.mu.Lock()
	for _, request := range fake.requests[before:] {
		if strings.HasPrefix(request, "POST") || strings.HasPrefix(request, "DELETE") {
			t.Fatal("adoption mutated a live container")
		}
	}
	fake.mu.Unlock()
	if data, _ := os.ReadFile(filepath.Join(legacyRoot, "profile/Local State")); string(data) != "opaque profile identity" {
		t.Fatal("adoption changed legacy data")
	}
	if err := manager.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	settings := networkSettings("source")
	// A new explicit profile with the same deployment name must have a new ID;
	// the old profile's legacy storage remains untouched and discoverable.
	fresh, err := manager.CreateProfile(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	settings.ProfileID = fresh.ID
	for _, dir := range []string{"profile", "downloads"} {
		if err := os.MkdirAll(filepath.Join(manager.config.StorageInternal, "profiles", fresh.ID, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Create(ctx, settings); err != nil {
		t.Fatal(err)
	}
	profiles, err = manager.ListProfiles(ctx)
	if err != nil || len(profiles) != 2 || profiles[0].ID == profiles[1].ID {
		t.Fatal("same deployment name collided with retained profile")
	}
}

func TestProfileCheckpointIsPrivateAndRegistryContainsOnlyOwnership(t *testing.T) {
	manager, _, id, root := cloneFixture(t, config.NetworkModeDirect)
	fakeCloneBrowser(t, manager)
	container, err := manager.inspectContainer(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	container.Config.Image = "owned-chromium-cdp:test"
	profile, err := manager.containerProfile(container)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.checkpointProfile(context.Background(), container, profile); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(root, identityCheckpoint)
	info, err := os.Stat(checkpoint)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("identity checkpoint is not private")
	}
	var identity profileIdentity
	data, _ := os.ReadFile(checkpoint)
	if json.Unmarshal(data, &identity) != nil || len(identity.Cookies) != 2 {
		t.Fatal("private checkpoint omitted browser identity")
	}
	data, _ = os.ReadFile(manager.registryPath())
	if strings.Contains(string(data), "cookies") || strings.Contains(string(data), "session-secret") || strings.Contains(string(data), "admin-secret") {
		t.Fatal("registry leaked private identity")
	}
	if err := manager.restoreProfile(context.Background(), id, profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(checkpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restored checkpoint was not consumed")
	}
}
