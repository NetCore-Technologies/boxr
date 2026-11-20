package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PrepareRootPath creates a mount target beneath root without following any
// pre-existing symlink component. This prevents bind/tmpfs targets from
// escaping the selected root filesystem through a malicious symlink.
func PrepareRootPath(root, target string, directory bool) (string, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) || hasDotDot(target) {
		return "", fmt.Errorf("root and target must be absolute and target must not contain '..'")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve rootfs: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", fmt.Errorf("rootfs %s is not a directory", root)
	}
	cleanTarget := filepath.Clean(target)
	candidate := filepath.Join(root, strings.TrimPrefix(cleanTarget, string(filepath.Separator)))
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target %q escapes rootfs", target)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	current := root
	for index, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		last := index == len(parts)-1
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("mount target component %s is a symlink", current)
			}
			if !last || directory {
				if !info.IsDir() {
					return "", fmt.Errorf("mount target component %s is not a directory", current)
				}
			} else if info.IsDir() {
				return "", fmt.Errorf("mount target %s is a directory, expected a file", current)
			}
			continue
		}
		if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect mount target %s: %w", current, statErr)
		}
		if !last || directory {
			if err := os.Mkdir(current, 0755); err != nil {
				return "", fmt.Errorf("create mount target directory %s: %w", current, err)
			}
		} else {
			file, err := os.OpenFile(current, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return "", fmt.Errorf("create mount target file %s: %w", current, err)
			}
			if err := file.Close(); err != nil {
				return "", fmt.Errorf("close mount target file %s: %w", current, err)
			}
		}
	}
	return candidate, nil
}

func ParseTmpfsSize(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, nil
	}
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"kib", 1 << 10}, {"k", 1 << 10},
		{"mib", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"g", 1 << 30},
		{"b", 1},
	}
	var multiplier int64 = 1
	number := value
	for _, unit := range units {
		if strings.HasSuffix(value, unit.suffix) {
			number = strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
			multiplier = unit.multiplier
			break
		}
	}
	amount, err := strconv.ParseInt(number, 10, 64)
	if err != nil || amount <= 0 || amount > int64(^uint64(0)>>1)/multiplier {
		return 0, fmt.Errorf("expected a positive size with optional B, K, M, or G suffix")
	}
	return amount * multiplier, nil
}
