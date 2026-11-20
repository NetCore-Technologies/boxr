package support

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Diagnostic struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Metadata struct {
	Version     string       `json:"version"`
	Commit      string       `json:"commit"`
	BuildDate   string       `json:"buildDate"`
	Diagnostics []Diagnostic `json:"-"`
}

type Report struct {
	SchemaVersion int         `json:"schemaVersion"`
	GeneratedAt   time.Time   `json:"generatedAt"`
	Boxr          buildInfo   `json:"boxr"`
	Runtime       runtimeInfo `json:"runtime"`
	Diagnostics   diagSummary `json:"diagnostics"`
	Privacy       privacyInfo `json:"privacy"`
}

type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

type runtimeInfo struct {
	OS        string `json:"os"`
	Arch      string `json:"architecture"`
	GoVersion string `json:"goVersion"`
}

type diagSummary struct {
	Checks []Diagnostic `json:"checks"`
	OK     int          `json:"ok"`
	Warn   int          `json:"warn"`
	Error  int          `json:"error"`
}

type privacyInfo struct {
	IncludesEnvironment     bool `json:"includesEnvironment"`
	IncludesCommandArgs     bool `json:"includesCommandArgs"`
	IncludesContainerLogs   bool `json:"includesContainerLogs"`
	IncludesContainerState  bool `json:"includesContainerState"`
	IncludesFilesystemPaths bool `json:"includesFilesystemPaths"`
	IncludesConfiguration   bool `json:"includesConfiguration"`
}

type Result struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Create writes a small support archive containing only build/runtime metadata
// and doctor check names/statuses. It deliberately does not collect secrets,
// command arguments, labels, logs, host paths, configuration, or state files.
func Create(path string, force bool, metadata Metadata) (Result, error) {
	var result Result
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		return result, errors.New("output path must be non-empty and contain no NUL characters")
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return result, fmt.Errorf("resolve output path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	parent := filepath.Dir(absolute)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return result, fmt.Errorf("inspect output directory: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return result, errors.New("output parent must be a real directory, not a symlink")
	}

	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return result, errors.New("output destination must be a regular file, not a symlink or special file")
		}
		if !force {
			return result, fmt.Errorf("output already exists: %s (use --force to replace it)", absolute)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("inspect output destination: %w", statErr)
	}

	checks := append([]Diagnostic(nil), metadata.Diagnostics...)
	for i := range checks {
		if !validName(checks[i].Name) {
			return result, fmt.Errorf("invalid diagnostic name at index %d", i)
		}
		switch checks[i].Status {
		case "ok", "warn", "error":
		default:
			return result, fmt.Errorf("invalid diagnostic status %q", checks[i].Status)
		}
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	for i := 1; i < len(checks); i++ {
		if checks[i-1].Name == checks[i].Name {
			return result, fmt.Errorf("duplicate diagnostic name %q", checks[i].Name)
		}
	}

	generated := time.Now().UTC().Truncate(time.Second)
	report := Report{
		SchemaVersion: 1,
		GeneratedAt:   generated,
		Boxr: buildInfo{
			Version:   cleanMetadata(metadata.Version),
			Commit:    cleanMetadata(metadata.Commit),
			BuildDate: cleanMetadata(metadata.BuildDate),
		},
		Runtime: runtimeInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version()},
		Diagnostics: diagSummary{Checks: checks},
		Privacy: privacyInfo{},
	}
	for _, check := range checks {
		switch check.Status {
		case "ok":
			report.Diagnostics.OK++
		case "warn":
			report.Diagnostics.Warn++
		case "error":
			report.Diagnostics.Error++
		}
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return result, fmt.Errorf("encode support report: %w", err)
	}

	temporary, err := os.CreateTemp(parent, ".boxr-support-*.tmp")
	if err != nil {
		return result, fmt.Errorf("create temporary archive: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return result, fmt.Errorf("secure temporary archive: %w", err)
	}

	archive := zip.NewWriter(temporary)
	if err := addFile(archive, "report.json", payload, generated); err != nil {
		_ = archive.Close()
		_ = temporary.Close()
		return result, err
	}
	readme := []byte("Boxr support bundle\n\nThis archive contains only Boxr build metadata, Go runtime metadata, and host diagnostic check names/statuses.\nIt does not include environment variables or their values, command arguments, container labels, filesystem paths, container logs, configuration files, or container state.\n\nReview report.json before sharing this archive.\n")
	if err := addFile(archive, "README.txt", readme, generated); err != nil {
		_ = archive.Close()
		_ = temporary.Close()
		return result, err
	}
	if err := archive.Close(); err != nil {
		_ = temporary.Close()
		return result, fmt.Errorf("finish support archive: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return result, fmt.Errorf("sync support archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return result, fmt.Errorf("close support archive: %w", err)
	}

	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return result, errors.New("output destination changed to a symlink or special file")
		}
		if !force {
			return result, fmt.Errorf("output already exists: %s (use --force to replace it)", absolute)
		}
		if err := os.Remove(absolute); err != nil {
			return result, fmt.Errorf("replace output file: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("recheck output destination: %w", statErr)
	}

	if err := os.Rename(temporaryPath, absolute); err != nil {
		return result, fmt.Errorf("publish support archive: %w", err)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	bytesWritten, err := io.Copy(hash, file)
	if err != nil {
		return result, fmt.Errorf("hash support archive: %w", err)
	}
	return Result{Path: absolute, Bytes: bytesWritten, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func addFile(archive *zip.Writer, name string, payload []byte, modified time.Time) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0600)
	header.SetModTime(modified)
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("add %s to support archive: %w", name, err)
	}
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("write %s to support archive: %w", name, err)
	}
	return nil
}

func validName(value string) bool {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ' ') {
			return false
		}
	}
	return true
}

func cleanMetadata(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}
