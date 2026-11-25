package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/m1k1o/neko-rooms/internal/types"
	"github.com/m1k1o/neko-rooms/internal/utils"
)

const profileLabel = "m1k1o.neko_rooms.profile"
const profileStoragePath = "profiles"
const identityCheckpoint = ".neko-identity.json"

// Registry metadata contains ownership only, never browser identity or credentials.
// Docker labels and actual bind mounts reconcile leases, including legacy rooms
// and containers removed outside Manager. All mutations hold lifecycle.
type profileRecord struct {
	types.BrowserProfile
	Root   string            `json:"root"`
	Mounts []types.RoomMount `json:"mounts"`
}
type profileRegistry struct {
	Version  int              `json:"version"`
	Profiles []*profileRecord `json:"profiles"`
}

func (manager *RoomManagerCtx) registryPath() string {
	return filepath.Join(manager.config.StorageInternal, "profiles.json")
}

func atomicPrivateJSON(filename string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("cannot encode private profile metadata")
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".profile-write-")
	if err != nil {
		return errors.New("cannot create private profile metadata")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), filename)
	}
	if err != nil {
		return errors.New("cannot atomically save private profile metadata")
	}
	dir, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return errors.New("cannot open profile metadata directory")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("cannot sync profile metadata directory")
	}
	return nil
}

func (manager *RoomManagerCtx) loadProfiles() (*profileRegistry, error) {
	registry := &profileRegistry{Version: 1, Profiles: []*profileRecord{}}
	if !manager.config.StorageEnabled {
		return registry, nil
	}
	data, err := os.ReadFile(manager.registryPath())
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil || json.Unmarshal(data, registry) != nil || registry.Version != 1 {
		return nil, errors.New("persistent profile registry is unavailable or invalid")
	}
	ids, roots := map[string]bool{}, map[string]bool{}
	for _, profile := range registry.Profiles {
		if profile == nil || profile.ID == "" || ids[profile.ID] || roots[profile.Root] || !validProfileRoot(profile.Root) {
			return nil, errors.New("persistent profile registry has invalid ownership")
		}
		ids[profile.ID], roots[profile.Root] = true, true
	}
	return registry, nil
}
func validProfileRoot(root string) bool {
	parts := strings.Split(filepath.ToSlash(root), "/")
	return len(parts) == 2 && (parts[0] == "rooms" || parts[0] == profileStoragePath) && parts[1] != "" && parts[1] != "." && parts[1] != ".." && filepath.Clean(root) == root
}
func (manager *RoomManagerCtx) saveProfiles(registry *profileRegistry) error {
	if err := os.MkdirAll(manager.config.StorageInternal, 0755); err != nil {
		return errors.New("persistent profile storage is unavailable")
	}
	return atomicPrivateJSON(manager.registryPath(), registry)
}
func findProfile(registry *profileRegistry, id string) *profileRecord {
	for _, profile := range registry.Profiles {
		if profile.ID == id {
			return profile
		}
	}
	return nil
}
func profileForContainer(registry *profileRegistry, labels map[string]string, mounts []dockerContainer.MountPoint, external string) *profileRecord {
	if profile := findProfile(registry, labels[profileLabel]); profile != nil {
		return profile
	}
	for _, profile := range registry.Profiles {
		root := filepath.Join(external, profile.Root)
		for _, mount := range mounts {
			if _, ok := storageRelativePath(root, mount.Source); ok {
				return profile
			}
		}
	}
	return nil
}

// Adoption registers directories in place. It never pauses browsers, changes a
// live mount, or rewrites the legacy directory's contents/ownership.
func (manager *RoomManagerCtx) reconcileProfiles(ctx context.Context) (*profileRegistry, error) {
	registry, err := manager.loadProfiles()
	if err != nil || !manager.config.StorageEnabled {
		return registry, err
	}
	before, _ := json.Marshal(registry)
	containers, err := manager.listContainers(ctx, nil)
	if err != nil {
		return nil, err
	}
	knownRoots := map[string]*profileRecord{}
	for _, profile := range registry.Profiles {
		profile.RoomID, profile.RoomName = "", ""
		knownRoots[profile.Root] = profile
	}
	legacy, err := os.ReadDir(filepath.Join(manager.config.StorageInternal, privateStoragePath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot inspect legacy profile storage")
	}
	for _, directory := range legacy {
		if !directory.IsDir() || strings.HasPrefix(directory.Name(), ".") {
			continue
		}
		root := filepath.Join("rooms", directory.Name())
		if knownRoots[root] != nil {
			continue
		}
		id, err := utils.NewUID(24)
		if err != nil {
			return nil, errors.New("cannot assign persistent profile identity")
		}
		profile := &profileRecord{BrowserProfile: types.BrowserProfile{ID: id, Name: directory.Name(), Created: time.Now().UTC()}, Root: root}
		registry.Profiles = append(registry.Profiles, profile)
		knownRoots[root] = profile
	}
	for _, container := range containers {
		profile := profileForContainer(registry, container.Labels, container.Mounts, manager.config.StorageExternal)
		if profile == nil {
			if container.Labels[profileLabel] != "" {
				return nil, errors.New("container references an unknown persistent profile")
			}
			continue
		}
		if profile.RoomID != "" && profile.RoomID != container.ID[:12] {
			return nil, fmt.Errorf("%w: multiple deployments reference the same profile", types.ErrProfileInUse)
		}
		profile.RoomID, profile.RoomName = container.ID[:12], container.Labels["m1k1o.neko_rooms.name"]
		// Authoritative mounts also recover a Create accepted just before a crash.
		private := []types.RoomMount{}
		for _, mount := range container.Mounts {
			if relative, ok := storageRelativePath(filepath.Join(manager.config.StorageExternal, profile.Root), mount.Source); ok {
				private = append(private, types.RoomMount{Type: types.MountPrivate, HostPath: relative, ContainerPath: mount.Destination})
			}
		}
		if len(private) > 0 {
			profile.Mounts = private
		}
	}
	after, _ := json.Marshal(registry)
	if !reflect.DeepEqual(before, after) {
		if err := manager.saveProfiles(registry); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (manager *RoomManagerCtx) ListProfiles(ctx context.Context) ([]types.BrowserProfile, error) {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	registry, err := manager.reconcileProfiles(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]types.BrowserProfile, 0, len(registry.Profiles))
	for _, profile := range registry.Profiles {
		result = append(result, profile.BrowserProfile)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Created.Before(result[j].Created) })
	return result, nil
}
func validateProfileName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > 120 || strings.ContainsAny(name, "\r\n\x00") {
		return fmt.Errorf("%w: profile name must contain 1 to 120 characters", types.ErrInvalidRoomSettings)
	}
	return nil
}
func (manager *RoomManagerCtx) newProfile(registry *profileRegistry, name string) (*profileRecord, error) {
	if !manager.config.StorageEnabled {
		return nil, fmt.Errorf("%w: persistent profiles require managed storage", types.ErrInvalidRoomSettings)
	}
	if err := validateProfileName(name); err != nil {
		return nil, err
	}
	id, err := utils.NewUID(24)
	if err != nil {
		return nil, errors.New("cannot assign persistent profile identity")
	}
	profile := &profileRecord{BrowserProfile: types.BrowserProfile{ID: id, Name: strings.TrimSpace(name), Created: time.Now().UTC()}, Root: filepath.Join(profileStoragePath, id)}
	if err := os.MkdirAll(filepath.Join(manager.config.StorageInternal, profileStoragePath), 0755); err != nil {
		return nil, errors.New("cannot create persistent profile storage")
	}
	if err := os.Mkdir(filepath.Join(manager.config.StorageInternal, profile.Root), 0750); err != nil {
		return nil, errors.New("cannot reserve persistent profile storage")
	}
	registry.Profiles = append(registry.Profiles, profile)
	if err := manager.saveProfiles(registry); err != nil {
		return nil, err
	}
	return profile, nil
}
func (manager *RoomManagerCtx) CreateProfile(ctx context.Context, name string) (*types.BrowserProfile, error) {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	registry, err := manager.reconcileProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profile, err := manager.newProfile(registry, name)
	if err != nil {
		return nil, err
	}
	return &profile.BrowserProfile, nil
}
func (manager *RoomManagerCtx) DeleteProfile(ctx context.Context, id string) error {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	registry, err := manager.reconcileProfiles(ctx)
	if err != nil {
		return err
	}
	profile := findProfile(registry, id)
	if profile == nil {
		return types.ErrProfileNotFound
	}
	if profile.RoomID != "" {
		return types.ErrProfileInUse
	}
	// Keep the registry entry if deletion fails, so partial data remains discoverable.
	if err := os.RemoveAll(filepath.Join(manager.config.StorageInternal, profile.Root)); err != nil {
		return errors.New("cannot delete persistent profile data")
	}
	for i, candidate := range registry.Profiles {
		if candidate.ID == id {
			registry.Profiles = append(registry.Profiles[:i], registry.Profiles[i+1:]...)
			break
		}
	}
	return manager.saveProfiles(registry)
}
func (manager *RoomManagerCtx) containerProfile(container *dockerContainer.InspectResponse) (*profileRecord, error) {
	registry, err := manager.loadProfiles()
	if err != nil {
		return nil, err
	}
	return profileForContainer(registry, container.Config.Labels, container.Mounts, manager.config.StorageExternal), nil
}

func (manager *RoomManagerCtx) prepareProfile(ctx context.Context, settings *types.RoomSettings) (*profileRecord, error) {
	if !manager.config.StorageEnabled {
		if settings.ProfileID != "" || settings.ProfileName != "" {
			return nil, fmt.Errorf("%w: persistent profiles require managed storage", types.ErrInvalidRoomSettings)
		}
		return nil, nil
	}
	registry, err := manager.reconcileProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profile := findProfile(registry, settings.ProfileID)
	if settings.ProfileID != "" && profile == nil {
		return nil, types.ErrProfileNotFound
	}
	if profile != nil && profile.RoomID != "" {
		return nil, types.ErrProfileInUse
	}
	if profile == nil {
		name := settings.ProfileName
		if name == "" {
			name = settings.Name
		}
		profile, err = manager.newProfile(registry, name)
		if err != nil {
			return nil, err
		}
	}
	mounts := []types.RoomMount{}
	private := []types.RoomMount{}
	for _, mount := range settings.Mounts {
		if mount.Type == types.MountPrivate {
			private = append(private, mount)
		} else {
			mounts = append(mounts, mount)
		}
	}
	if len(profile.Mounts) == 0 {
		if len(private) == 0 {
			private = []types.RoomMount{{Type: types.MountPrivate, HostPath: "/profile", ContainerPath: "/home/neko/profile"}}
		}
		hasDownloads := false
		for _, mount := range private {
			hasDownloads = hasDownloads || mount.ContainerPath == "/home/neko/Downloads"
		}
		if !hasDownloads {
			private = append(private, types.RoomMount{Type: types.MountPrivate, HostPath: "/downloads", ContainerPath: "/home/neko/Downloads"})
		}
		profile.Mounts = private
		if err := manager.saveProfiles(registry); err != nil {
			return nil, err
		}
	}
	settings.Mounts = append(mounts, profile.Mounts...)
	settings.ProfileID = profile.ID
	return profile, nil
}

type profileIdentity struct {
	Cookies []map[string]json.RawMessage `json:"cookies"`
	Storage []*cloneFrame                `json:"storage"`
}

func usesChromiumCheckpoint(container *dockerContainer.InspectResponse) bool {
	image := strings.ToLower(container.Config.Image)
	return strings.Contains(image, "chromium") || strings.Contains(image, "chrome") || strings.Contains(image, "cdp")
}
func (manager *RoomManagerCtx) checkpointProfile(ctx context.Context, container *dockerContainer.InspectResponse, profile *profileRecord) error {
	if profile == nil || !container.State.Running || !usesChromiumCheckpoint(container) {
		return nil
	}
	cookies, storage, err := manager.snapshotLiveIdentity(ctx, container)
	if err != nil {
		return err
	}
	return atomicPrivateJSON(filepath.Join(manager.config.StorageInternal, profile.Root, identityCheckpoint), profileIdentity{cookies, storage})
}
func (manager *RoomManagerCtx) restoreProfile(ctx context.Context, id string, profile *profileRecord) error {
	if profile == nil {
		return nil
	}
	filename := filepath.Join(manager.config.StorageInternal, profile.Root, identityCheckpoint)
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var identity profileIdentity
	if err != nil || json.Unmarshal(data, &identity) != nil {
		return errors.New("private profile identity checkpoint is unavailable or invalid")
	}
	if err := manager.restoreCookies(ctx, id[:12]+":9223", identity.Cookies); err != nil {
		return err
	}
	if err := manager.restoreLocalStorage(ctx, id[:12]+":9223", identity.Storage); err != nil {
		return err
	}
	// A checkpoint is consumed once; a subsequent live stop captures newer state.
	if err := os.Remove(filename); err != nil {
		return errors.New("cannot consume restored private identity checkpoint")
	}
	return nil
}

// Rollback may delete only the new profile whose Docker ownership was checked.
func (manager *RoomManagerCtx) discardUnboundProfile(ctx context.Context, id string) error {
	registry, err := manager.reconcileProfiles(ctx)
	if err != nil {
		return err
	}
	profile := findProfile(registry, id)
	if profile == nil {
		return nil
	}
	if profile.RoomID != "" {
		return types.ErrProfileInUse
	}
	if err := os.RemoveAll(filepath.Join(manager.config.StorageInternal, profile.Root)); err != nil {
		return errors.New("cannot clean incomplete profile storage")
	}
	for i, p := range registry.Profiles {
		if p.ID == id {
			registry.Profiles = append(registry.Profiles[:i], registry.Profiles[i+1:]...)
			break
		}
	}
	return manager.saveProfiles(registry)
}
