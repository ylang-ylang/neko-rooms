package room

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// storageRelativePath is shared by readback and clone validation. A sibling
// such as rooms/alpha-other is not storage belonging to rooms/alpha.
func storageRelativePath(root, source string) (string, bool) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) {
		return "", false
	}
	rel, err := filepath.Rel(root, source)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "/", true
	}
	return "/" + filepath.ToSlash(rel), true
}

func transientBrowserFile(name string) bool {
	switch name {
	case "SingletonLock", "SingletonCookie", "SingletonSocket", "DevToolsActivePort":
		return true
	}
	return false
}

// snapshotTree never follows symlinks, never hardlinks writable files, and
// retains SQLite databases, WALs and journals together while writers are frozen.
// The caller owns destination (a newly created private staging directory).
func snapshotTree(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("managed storage root is not a directory")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()

	var directories []struct {
		path string
		info fs.FileInfo
	}
	err = filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		if rel != "." && transientBrowserFile(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		switch {
		case info.IsDir():
			if rel != "." {
				if err := os.Mkdir(target, 0700); err != nil {
					return err
				}
			}
			directories = append(directories, struct {
				path string
				info fs.FileInfo
			}{target, info})
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			return snapshotMetadata(target, info, true)
		case info.Mode().IsRegular():
			input, err := root.Open(rel)
			if err != nil {
				return err
			}
			defer input.Close()
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			copyErr := copySnapshotFile(ctx, output, input)
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			return snapshotMetadata(target, info, false)
		case info.Mode()&os.ModeSocket != 0:
			// Unix sockets represent source processes, not persistent data.
			return nil
		default:
			return fmt.Errorf("managed storage contains unsupported special file")
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Apply directory modes last (a source directory may be non-writable), and
	// restore its timestamp only after all children have been copied.
	for i := len(directories) - 1; i >= 0; i-- {
		if err := snapshotMetadata(directories[i].path, directories[i].info, false); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func copySnapshotFile(ctx context.Context, output, input *os.File) error {
	if err := cloneFile(output, input); err == nil {
		return ctx.Err()
	} else if !reflinkUnsupported(err) {
		return err
	}
	// A bounded buffer keeps cancellation responsive even on large downloads.
	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := input.Read(buffer)
		if n > 0 {
			if _, err := output.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func snapshotMetadata(target string, info fs.FileInfo, symlink bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("managed storage ownership is unavailable")
	}
	if err := os.Lchown(target, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	if symlink {
		return nil
	}
	if err := os.Chmod(target, info.Mode()); err != nil {
		return err
	}
	return os.Chtimes(target, info.ModTime(), info.ModTime())
}
