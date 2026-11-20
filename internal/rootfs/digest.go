package rootfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added, removed, or modified
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// Digest computes a deterministic SHA-256 over rootfs paths, object types,
// permission bits, symlink targets, and regular-file contents. Modification
// times, owners, and inode numbers are intentionally excluded for portability.
func Digest(root string) (string, error) {
	entries, err := treeDigests(root)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(entries))
	for name := range entries {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, name := range paths {
		writeDigestField(h, name)
		writeDigestField(h, entries[name])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Diff compares two rootfs trees without following symlinks.
func Diff(beforeRoot, afterRoot string) ([]Change, error) {
	before, err := treeDigests(beforeRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect first rootfs: %w", err)
	}
	after, err := treeDigests(afterRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect second rootfs: %w", err)
	}
	all := make(map[string]struct{}, len(before)+len(after))
	for name := range before {
		all[name] = struct{}{}
	}
	for name := range after {
		all[name] = struct{}{}
	}
	paths := make([]string, 0, len(all))
	for name := range all {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	changes := make([]Change, 0)
	for _, name := range paths {
		oldHash, hadOld := before[name]
		newHash, hasNew := after[name]
		switch {
		case !hadOld:
			changes = append(changes, Change{Path: name, Status: "added", After: newHash})
		case !hasNew:
			changes = append(changes, Change{Path: name, Status: "removed", Before: oldHash})
		case oldHash != newHash:
			changes = append(changes, Change{Path: name, Status: "modified", Before: oldHash, After: newHash})
		}
	}
	return changes, nil
}

func treeDigests(root string) (map[string]string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect rootfs: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("rootfs must be a real directory, not a symlink")
	}
	entries := make(map[string]string)
	err = filepath.Walk(absolute, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == absolute {
			return nil
		}
		relative, err := filepath.Rel(absolute, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		h := sha256.New()
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			writeDigestField(h, "symlink")
			writeDigestField(h, fmt.Sprintf("%o", mode.Perm()))
			writeDigestField(h, link)
		case info.IsDir():
			writeDigestField(h, "directory")
			writeDigestField(h, fmt.Sprintf("%o", mode.Perm()))
		case mode.IsRegular():
			writeDigestField(h, "file")
			writeDigestField(h, fmt.Sprintf("%o", mode.Perm()))
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(h, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported special file %q", name)
		}
		entries[name] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func writeDigestField(h hash.Hash, value string) {
	_, _ = io.WriteString(h, fmt.Sprintf("%d:", len(value)))
	_, _ = io.WriteString(h, value)
	_, _ = io.WriteString(h, "\n")
}

// NormalizeDigest accepts the common optional sha256: prefix and rejects
// malformed values before any comparison is attempted.
func NormalizeDigest(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return "", fmt.Errorf("SHA-256 digest must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("invalid SHA-256 digest: %w", err)
	}
	return value, nil
}
