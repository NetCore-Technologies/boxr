package image

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/manit6752025/boxr/internal/rootfs"
)

// Diff compares the verified root filesystems of two local image references.
// Images are unpacked into private temporary directories and the comparison
// does not follow symlinks in either tree.
func Diff(beforeReference, afterReference string) ([]rootfs.Change, error) {
	if err := ValidateReference(beforeReference); err != nil {
		return nil, fmt.Errorf("validate first image reference: %w", err)
	}
	if err := ValidateReference(afterReference); err != nil {
		return nil, fmt.Errorf("validate second image reference: %w", err)
	}
	stage, err := os.MkdirTemp("", "boxr-image-diff-*")
	if err != nil {
		return nil, fmt.Errorf("create image diff staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	beforeRoot := filepath.Join(stage, "before")
	afterRoot := filepath.Join(stage, "after")
	if _, err := Unpack(beforeReference, beforeRoot); err != nil {
		return nil, fmt.Errorf("unpack first image %q: %w", beforeReference, err)
	}
	if _, err := Unpack(afterReference, afterRoot); err != nil {
		return nil, fmt.Errorf("unpack second image %q: %w", afterReference, err)
	}
	changes, err := rootfs.Diff(beforeRoot, afterRoot)
	if err != nil {
		return nil, fmt.Errorf("compare image root filesystems: %w", err)
	}
	return changes, nil
}
