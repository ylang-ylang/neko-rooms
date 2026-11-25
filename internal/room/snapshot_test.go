package room

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeSnapshotFixture(t *testing.T, root, name, value string, mode os.FileMode) {
	t.Helper()
	file := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotPreservesProfileMetadataAndIndependentFiles(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	files := []string{"profile/Local State", "profile/Default/Cookies", "profile/Default/Cookies-wal", "profile/Default/Cookies-shm", "profile/Default/Cookies-journal", "profile/Default/Local Storage/leveldb/000003.log", "profile/Default/IndexedDB/site/000002.ldb", "profile/Default/Extensions/extension/state", "Downloads/report"}
	for _, name := range files {
		writeSnapshotFixture(t, source, name, "original", 0640)
	}
	stamp := time.Unix(1700000000, 0)
	cookieFile := filepath.Join(source, files[1])
	if err := os.Chtimes(cookieFile, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeSnapshotFixture(t, outside, "never-read", "outside", 0600)
	if err := os.Symlink(outside, filepath.Join(source, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("Default/Cookies", filepath.Join(source, "profile/cookie-link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SingletonCookie", "SingletonLock", "SingletonSocket", "DevToolsActivePort"} {
		writeSnapshotFixture(t, source, "profile/"+name, "transient", 0600)
	}
	socketPath := filepath.Join(source, "profile/extension.sock")
	socket, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := snapshotTree(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		copied, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(copied) != "original" {
			t.Fatalf("persistent file %s was not copied", name)
		}
		before, _ := os.Stat(filepath.Join(source, name))
		after, _ := os.Stat(filepath.Join(target, name))
		if os.SameFile(before, after) || before.Mode() != after.Mode() || before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid || before.Sys().(*syscall.Stat_t).Gid != after.Sys().(*syscall.Stat_t).Gid {
			t.Fatal("snapshot shared an inode or changed ownership/modes")
		}
		if err := os.WriteFile(filepath.Join(target, name), []byte("target changed"), 0600); err != nil {
			t.Fatal(err)
		}
		original, _ := os.ReadFile(filepath.Join(source, name))
		if string(original) != "original" {
			t.Fatal("target writes changed the source")
		}
	}
	// The copied link is still a link, not a recursive copy of outside data.
	if link, err := os.Readlink(filepath.Join(target, "outside-link")); err != nil || link != outside {
		t.Fatal("external symlink was dereferenced or changed")
	}
	if link, _ := os.Readlink(filepath.Join(target, "profile/cookie-link")); link != "Default/Cookies" {
		t.Fatal("relative symlink changed")
	}
	for _, name := range []string{"SingletonCookie", "SingletonLock", "SingletonSocket", "DevToolsActivePort", "extension.sock"} {
		if _, err := os.Lstat(filepath.Join(target, "profile", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source process artifact %s was copied", name)
		}
		if _, err := os.Lstat(filepath.Join(source, "profile", name)); err != nil {
			t.Fatal("source artifact was removed")
		}
	}
}

func TestSnapshotCancellationAndAtomicNoReplace(t *testing.T) {
	source, stage := t.TempDir(), t.TempDir()
	writeSnapshotFixture(t, source, "profile/Cookies", "original", 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := snapshotTree(ctx, source, stage); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled snapshot did not stop")
	}
	target := t.TempDir()
	writeSnapshotFixture(t, target, "existing", "never replace", 0600)
	if err := commitSnapshot(stage, target); err == nil {
		t.Fatal("atomic commit replaced an existing directory")
	}
	data, _ := os.ReadFile(filepath.Join(target, "existing"))
	if string(data) != "never replace" {
		t.Fatal("preexisting target changed")
	}
	if _, err := storageRelativePath("/data/rooms/source", "/data/rooms/source-other/profile"); err {
		t.Fatal("sibling was classified as managed source storage")
	}
	if rel, ok := storageRelativePath("/data/rooms/source", "/data/rooms/source/profile"); !ok || rel != "/profile" {
		t.Fatal("managed child did not retain its mount-relative path")
	}
}
