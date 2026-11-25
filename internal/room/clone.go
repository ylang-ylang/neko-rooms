package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	dockerContainer "github.com/docker/docker/api/types/container"
	dockerMount "github.com/docker/docker/api/types/mount"
	dockerNames "github.com/docker/docker/daemon/names"

	"github.com/m1k1o/neko-rooms/internal/types"
	"github.com/m1k1o/neko-rooms/internal/utils"
)

const cloneOperationLabel = "m1k1o.neko_rooms.clone_operation"

// Clone is synchronous: the small managed profile is copied while its writers
// are frozen, then the source is resumed before any target startup/readiness
// wait. The lifecycle lock also covers regular create/delete/stop/recreate's
// mutations, preventing port, name and source-storage races inside Manager.
func (manager *RoomManagerCtx) Clone(ctx context.Context, id, name string) (entry *types.RoomEntry, resultErr error) {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" || !dockerNames.RestrictedNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid new room name, must match %s", types.ErrInvalidRoomSettings, dockerNames.RestrictedNameChars)
	}
	source, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	settings, err := manager.GetSettings(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if settings.NetworkMode != manager.config.EffectiveNetworkMode() {
		return nil, fmt.Errorf("%w: source networking differs from the current manager policy", types.ErrInvalidRoomSettings)
	}
	if _, err := manager.containerByName(ctx, name); err == nil {
		return nil, types.ErrRoomNameConflict
	} else if !errors.Is(err, types.ErrRoomNotFound) {
		return nil, err
	}
	if err := manager.validateCloneMounts(source, settings); err != nil {
		return nil, err
	}
	sourceRoot := filepath.Join(manager.config.StorageInternal, privateStoragePath, settings.Name)
	targetRoot := filepath.Join(manager.config.StorageInternal, privateStoragePath, name)
	if _, err := os.Lstat(targetRoot); err == nil {
		return nil, types.ErrRoomNameConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot inspect target storage")
	}
	// Preserve explicit hostnames; regenerate the default name-bound hostname.
	if settings.Hostname == manager.config.InstanceName+"-"+settings.Name {
		settings.Hostname = ""
	}
	settings.Name = name
	if err := manager.ValidateSettings(ctx, *settings); err != nil {
		return nil, err
	}
	// Refuse capacity failures before touching the source or filesystem.
	portsNeeded := settings.MaxConnections
	if manager.config.Mux {
		portsNeeded = 1
	}
	if _, err := manager.allocatePorts(ctx, portsNeeded); err != nil {
		return nil, err
	}
	operation, err := utils.NewUID(24)
	if err != nil {
		return nil, errors.New("cannot reserve clone transaction")
	}
	staging, err := os.MkdirTemp(filepath.Dir(targetRoot), ".clone-")
	if err != nil {
		return nil, errors.New("cannot reserve clone staging storage")
	}
	committed, success, createAttempted := false, false, false
	var targetID string
	// Cleanup has an independent context: a disconnected client must not leave
	// a paused source, orphan target container, or partial target storage.
	defer func() {
		if success {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		// Docker can accept Create but lose its response. Only recover a
		// container bearing this operation's unpredictable ownership label.
		if targetID == "" && createAttempted {
			candidate, lookupErr := manager.containerByName(cleanup, name)
			if lookupErr == nil && candidate.Labels[cloneOperationLabel] == operation {
				targetID = candidate.ID
			} else if lookupErr != nil && !errors.Is(lookupErr, types.ErrRoomNotFound) {
				resultErr = errors.Join(resultErr, errors.New("cannot verify failed clone container cleanup; target storage retained"))
				return // Its mount may still be active after an ambiguous Create.
			}
		}
		if targetID != "" {
			if err := manager.client.ContainerRemove(cleanup, targetID, dockerContainer.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
				resultErr = errors.Join(resultErr, errors.New("failed to remove incomplete clone container; target storage retained"))
				return // Do not remove data still mounted by the failed target.
			}
		}
		ownedPath := staging
		if committed {
			ownedPath = targetRoot
		}
		if err := os.RemoveAll(ownedPath); err != nil {
			resultErr = errors.Join(resultErr, errors.New("failed to remove incomplete clone storage"))
		}
	}()

	var cookies []map[string]json.RawMessage
	var localStorage []*cloneFrame
	if source.State.Running {
		cookies, localStorage, err = manager.snapshotLiveIdentity(ctx, source)
		if err != nil {
			return nil, err
		}
	}
	if err := manager.snapshotSource(ctx, source, sourceRoot, staging); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := commitSnapshot(staging, targetRoot); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, types.ErrRoomNameConflict
		}
		return nil, errors.New("cannot atomically commit clone storage")
	}
	committed = true

	// Once Docker Create has begun, finish collecting its result despite HTTP
	// cancellation, then roll back immediately if the request was canceled.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	createAttempted = true
	targetID, err = manager.create(createCtx, *settings, operation)
	cancel()
	if err != nil {
		if errdefs.IsConflict(err) {
			return nil, types.ErrRoomNameConflict
		}
		return nil, errors.New("failed to create clone container")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := manager.start(ctx, targetID); err != nil {
		return nil, errors.New("failed to start clone container")
	}
	if cookies != nil {
		if err := manager.restoreCookies(ctx, targetID+":9223", cookies); err != nil {
			return nil, fmt.Errorf("restore cloned browser identity: %w", err)
		}
		if err := manager.restoreLocalStorage(ctx, targetID+":9223", localStorage); err != nil {
			return nil, fmt.Errorf("restore active browser storage: %w", err)
		}
	}
	entry, err = manager.GetEntry(ctx, targetID)
	if err != nil {
		return nil, errors.New("cannot read started clone room")
	}
	success = true
	return entry, nil
}

func (manager *RoomManagerCtx) snapshotSource(ctx context.Context, source *dockerContainer.InspectResponse, sourceRoot, staging string) (resultErr error) {
	if source.State.Running && !source.State.Paused {
		// A cancellation between daemon acceptance and HTTP response is
		// ambiguous. Complete the short pause call independently, then always
		// unpause on our own cleanup context before returning to Clone.
		pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		pauseErr := manager.client.ContainerPause(pauseCtx, source.ID)
		cancel()
		defer func() {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := manager.client.ContainerUnpause(resumeCtx, source.ID); err != nil {
				resultErr = errors.Join(resultErr, errors.New("failed to resume source after clone snapshot"))
			}
		}()
		if pauseErr != nil {
			return errors.New("failed to pause source for clone snapshot")
		}
	}
	if err := snapshotTree(ctx, sourceRoot, staging); err != nil {
		// Filesystem errors include private filenames; keep details off API
		// responses/logs without hiding the fact that the snapshot failed.
		return errors.New("failed to snapshot managed room storage")
	}
	return nil
}

func (manager *RoomManagerCtx) validateCloneMounts(source *dockerContainer.InspectResponse, settings *types.RoomSettings) error {
	if !manager.config.StorageEnabled {
		return fmt.Errorf("%w: cloning identity requires managed persistent storage", types.ErrInvalidRoomSettings)
	}
	root := filepath.Join(manager.config.StorageInternal, privateStoragePath, settings.Name)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: source managed storage is unavailable", types.ErrInvalidRoomSettings)
	}
	privateMounts := 0
	for i, mount := range source.Mounts {
		if mount.Type != dockerMount.TypeBind || i >= len(settings.Mounts) {
			return fmt.Errorf("%w: non-bind mounts cannot be cloned independently", types.ErrInvalidRoomSettings)
		}
		canonical := settings.Mounts[i]
		if canonical.Type != types.MountPrivate {
			if mount.RW {
				return fmt.Errorf("%w: unmanaged writable mount cannot be cloned independently", types.ErrInvalidRoomSettings)
			}
			continue // Read-only external/templates may retain their config.
		}
		if !mount.RW {
			return fmt.Errorf("%w: read-only private mount is not representable by room settings", types.ErrInvalidRoomSettings)
		}
		// Reject a symlink in the mount's own path. Copying that symlink
		// verbatim could make Docker bind a source/external directory again.
		current := root
		for _, component := range strings.Split(strings.TrimPrefix(canonical.HostPath, "/"), "/") {
			if component == "" || component == "." {
				continue
			}
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: managed mount path is unavailable or symbolic", types.ErrInvalidRoomSettings)
			}
		}
		privateMounts++
	}
	if privateMounts == 0 {
		return fmt.Errorf("%w: source has no managed persistent mounts", types.ErrInvalidRoomSettings)
	}
	return nil
}

// An already-paused Chrome can still have unflushed cookies/localStorage.
// Briefly resume only for read-only CDP, then restore paused state before the
// filesystem snapshot. The original paused state is recovered on every error.
func (manager *RoomManagerCtx) snapshotLiveIdentity(ctx context.Context, source *dockerContainer.InspectResponse) (cookies []map[string]json.RawMessage, storage []*cloneFrame, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if source.State.Paused {
		resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		resumeErr := manager.client.ContainerUnpause(resumeCtx, source.ID)
		cancel()
		defer func() {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := manager.client.ContainerPause(pauseCtx, source.ID); err != nil {
				resultErr = errors.Join(resultErr, errors.New("failed to restore original paused source state"))
			}
		}()
		if resumeErr != nil {
			return nil, nil, errors.New("cannot briefly resume paused source to capture browser identity")
		}
	}
	address := source.ID[:12] + ":9223"
	storage, err := manager.snapshotLocalStorage(ctx, address)
	if err != nil {
		return nil, nil, fmt.Errorf("capture active browser storage: %w", err)
	}
	cookies, err = manager.snapshotCookies(ctx, address)
	if err != nil {
		return nil, nil, fmt.Errorf("capture live browser identity: %w", err)
	}
	return cookies, storage, nil
}
