package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const DefaultSBOMMaxEntries int64 = 250000

type SBOM struct {
	SchemaVersion  int         `json:"schemaVersion"`
	Kind           string      `json:"kind"`
	Reference      string      `json:"reference"`
	Manifest       string      `json:"manifestDigest"`
	OS             string      `json:"os"`
	Architecture   string      `json:"architecture"`
	Variant        string      `json:"variant,omitempty"`
	FileCount      int         `json:"fileCount"`
	DirectoryCount int         `json:"directoryCount"`
	SymlinkCount   int         `json:"symlinkCount"`
	Entries        []SBOMEntry `json:"entries"`
}

type SBOMEntry struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Mode       string `json:"mode"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	LinkTarget string `json:"linkTarget,omitempty"`
}

// GenerateSBOM creates a deterministic filesystem inventory for a verified
// local image. It records regular-file hashes and symlink targets but does not
// follow links or claim to discover package-manager dependencies.
func GenerateSBOM(reference string, maxEntries int64) (SBOM, error) {
	var report SBOM
	if maxEntries <= 0 {
		return report, fmt.Errorf("maximum SBOM entries must be positive")
	}
	image, err := Verify(reference)
	if err != nil {
		return report, fmt.Errorf("verify image before generating SBOM: %w", err)
	}
	stage, err := os.MkdirTemp("", "boxr-image-sbom-*")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(stage)
	root := filepath.Join(stage, "rootfs")
	if _, err := Unpack(reference, root); err != nil {
		return report, fmt.Errorf("unpack image for SBOM: %w", err)
	}
	report = SBOM{
		SchemaVersion: 1,
		Kind:          "boxr.filesystem-inventory",
		Reference:     image.Reference,
		Manifest:      image.Digest,
		OS:            image.OS,
		Architecture:  image.Arch,
		Variant:       image.Variant,
		Entries:       make([]SBOMEntry, 0),
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if int64(len(report.Entries)) >= maxEntries {
			return fmt.Errorf("SBOM entry limit of %d exceeded", maxEntries)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := SBOMEntry{Path: filepath.ToSlash(relative), Mode: fmt.Sprintf("%04o", info.Mode().Perm())}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			item.Type = "symlink"
			item.LinkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
			report.SymlinkCount++
		case info.IsDir():
			item.Type = "directory"
			report.DirectoryCount++
		case info.Mode().IsRegular():
			item.Type = "file"
			item.Size = info.Size()
			hash, err := hashRegularFile(path, info)
			if err != nil {
				return fmt.Errorf("hash %s: %w", item.Path, err)
			}
			item.SHA256 = hash
			report.FileCount++
		default:
			item.Type = "special"
		}
		report.Entries = append(report.Entries, item)
		return nil
	})
	if err != nil {
		return SBOM{}, fmt.Errorf("walk image rootfs: %w", err)
	}
	sort.Slice(report.Entries, func(i, j int) bool { return report.Entries[i].Path < report.Entries[j].Path })
	return report, nil
}

func hashRegularFile(path string, expected os.FileInfo) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return "", fmt.Errorf("file changed type while being inspected")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	if after.Size() != opened.Size() || after.ModTime() != opened.ModTime() {
		return "", fmt.Errorf("file changed while being hashed")
	}
	return strings.ToLower(hex.EncodeToString(hash.Sum(nil))), nil
}
