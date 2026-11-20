package image

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxBuildSpecBytes   = 1 << 20
	maxBuildIgnoreBytes = 64 << 10
	maxBuildIgnoreLine  = 4096
)

// BuildSpec is a deliberately small, non-executing Boxr build recipe. Only a
// scratch base and explicit context copies are supported; no shell RUN step is
// accepted, so building an image never executes arbitrary build instructions.
type BuildSpec struct {
	Version int                `json:"version"`
	Base    string             `json:"base"`
	Copy    []CopyInstruction  `json:"copy,omitempty"`
	Config  ImageRuntimeConfig `json:"config,omitempty"`
}

type CopyInstruction struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// ImageRuntimeConfig stores the standard runtime defaults represented by the
// image config. Environment entries are not printed by the default inspect UI.
type ImageRuntimeConfig struct {
	Env        []string          `json:"Env,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// LoadBuildSpec reads a bounded strict JSON recipe. Unknown fields and trailing
// JSON values are rejected rather than silently ignored.
func LoadBuildSpec(path string) (BuildSpec, error) {
	var spec BuildSpec
	info, err := os.Lstat(path)
	if err != nil {
		return spec, fmt.Errorf("inspect build spec: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxBuildSpecBytes {
		return spec, fmt.Errorf("build spec must be a regular non-symlink file no larger than %d bytes", maxBuildSpecBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return spec, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, fmt.Errorf("decode build spec: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return spec, fmt.Errorf("build spec contains trailing JSON data")
	}
	if err := ValidateBuildSpec(spec); err != nil {
		return spec, err
	}
	return spec, nil
}

func ValidateBuildSpec(spec BuildSpec) error {
	if spec.Version != 1 {
		return fmt.Errorf("unsupported build spec version %d (expected 1)", spec.Version)
	}
	if spec.Base != "scratch" {
		return fmt.Errorf("unsupported build base %q (only scratch is currently supported)", spec.Base)
	}
	if len(spec.Copy) > 256 {
		return fmt.Errorf("build spec supports at most 256 COPY instructions")
	}
	for index, instruction := range spec.Copy {
		if err := validateContextPath(instruction.Source); err != nil {
			return fmt.Errorf("COPY %d source: %w", index+1, err)
		}
		if !filepath.IsAbs(instruction.Destination) || strings.ContainsRune(instruction.Destination, '\x00') || hasParentTraversal(instruction.Destination) {
			return fmt.Errorf("COPY %d destination must be an absolute path without '..'", index+1)
		}
	}
	for _, entry := range spec.Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsAny(key+value, "\x00\r\n") || strings.Contains(key, "=") {
			return fmt.Errorf("invalid image environment entry")
		}
	}
	for _, values := range [][]string{spec.Config.Entrypoint, spec.Config.Cmd} {
		for _, value := range values {
			if value == "" || strings.ContainsAny(value, "\x00\r\n") {
				return fmt.Errorf("image command values must be non-empty and contain no NUL or newline")
			}
		}
	}
	if spec.Config.WorkingDir != "" && (!filepath.IsAbs(spec.Config.WorkingDir) || hasParentTraversal(spec.Config.WorkingDir) || strings.ContainsRune(spec.Config.WorkingDir, '\x00')) {
		return fmt.Errorf("image WorkingDir must be absolute and cannot contain '..'")
	}
	if strings.ContainsAny(spec.Config.User, "\x00\r\n") || len(spec.Config.User) > 256 {
		return fmt.Errorf("invalid image User value")
	}
	if len(spec.Config.Labels) > 128 {
		return fmt.Errorf("image config supports at most 128 labels")
	}
	for key, value := range spec.Config.Labels {
		if strings.TrimSpace(key) == "" || len(key) > 256 || len(value) > 4096 || strings.ContainsAny(key+value, "\x00\r\n") {
			return fmt.Errorf("invalid image label %q", key)
		}
	}
	return nil
}

func validateContextPath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') || hasParentTraversal(value) {
		return fmt.Errorf("must be a non-empty relative path inside the build context")
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("must name a path inside the build context")
	}
	return nil
}

func hasParentTraversal(value string) bool {
	for _, component := range strings.FieldsFunc(filepath.ToSlash(value), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

// Build creates a local Boxr image from an explicit context copy spec. It does
// not invoke a shell, package manager, or arbitrary command from the recipe.
func Build(contextDir, specPath, reference string) (Image, error) {
	return BuildForPlatform(contextDir, specPath, reference, "")
}

// BuildForPlatform builds a Linux rootfs image for an explicitly selected
// target platform. It does not execute target binaries; callers are responsible
// for supplying files that match the declared platform.
func BuildForPlatform(contextDir, specPath, reference, platform string) (Image, error) {
	if err := ValidateReference(reference); err != nil {
		return Image{}, err
	}
	osName, architecture, variant, err := parseBuildPlatform(platform)
	if err != nil {
		return Image{}, err
	}
	contextAbs, err := filepath.Abs(contextDir)
	if err != nil {
		return Image{}, err
	}
	contextInfo, err := os.Lstat(contextAbs)
	if err != nil {
		return Image{}, fmt.Errorf("inspect build context: %w", err)
	}
	if contextInfo.Mode()&os.ModeSymlink != 0 || !contextInfo.IsDir() {
		return Image{}, fmt.Errorf("build context must be a real directory, not a symlink")
	}
	spec, err := LoadBuildSpec(specPath)
	if err != nil {
		return Image{}, err
	}
	ignorePatterns, err := loadIgnorePatterns(contextAbs)
	if err != nil {
		return Image{}, err
	}
	stage, err := os.MkdirTemp("", "boxr-image-build-*")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(stage)
	root := filepath.Join(stage, "rootfs")
	if err := os.Mkdir(root, 0755); err != nil {
		return Image{}, err
	}
	for index, instruction := range spec.Copy {
		if err := applyCopy(contextAbs, root, instruction, ignorePatterns); err != nil {
			return Image{}, fmt.Errorf("COPY %d: %w", index+1, err)
		}
	}
	if spec.Config.WorkingDir != "" {
		destination, err := rootfsDestination(root, spec.Config.WorkingDir)
		if err != nil {
			return Image{}, err
		}
		if err := os.MkdirAll(destination, 0755); err != nil {
			return Image{}, fmt.Errorf("create image WorkingDir: %w", err)
		}
	}
	return ImportRootFSWithConfig(root, reference, osName, architecture, variant, spec.Config)
}

func parseBuildPlatform(value string) (osName, architecture, variant string, err error) {
	if value == "" {
		return "linux", runtimeArchitecture(), "", nil
	}
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" || parts[1] == "" {
		return "", "", "", fmt.Errorf("platform must be linux/ARCH[/VARIANT]")
	}
	osName, architecture = parts[0], parts[1]
	if len(parts) == 3 {
		variant = parts[2]
	}
	allowed := map[string]map[string]bool{
		"386":     {"": true},
		"amd64":   {"": true},
		"arm":     {"v5": true, "v6": true, "v7": true},
		"arm64":   {"": true, "v8": true},
		"ppc64le": {"": true},
		"riscv64": {"": true},
		"s390x":   {"": true},
	}
	variants, ok := allowed[architecture]
	if !ok || !variants[variant] {
		return "", "", "", fmt.Errorf("unsupported Linux build platform %q", value)
	}
	return osName, architecture, variant, nil
}

func rootfsDestination(root, target string) (string, error) {
	if !filepath.IsAbs(target) || hasParentTraversal(target) {
		return "", fmt.Errorf("rootfs destination must be absolute and cannot contain '..'")
	}
	clean := filepath.Clean(target)
	return filepath.Join(root, strings.TrimLeft(clean, string(filepath.Separator))), nil
}

func applyCopy(contextRoot, root string, instruction CopyInstruction, ignorePatterns []ignorePattern) error {
	if err := validateContextPath(instruction.Source); err != nil {
		return err
	}
	source := filepath.Join(contextRoot, filepath.Clean(instruction.Source))
	relative, err := filepath.Rel(contextRoot, source)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("source escapes the build context")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("COPY source may not be a symlink")
	}
	if ignoredPath(instruction.Source, info.IsDir(), ignorePatterns) {
		return fmt.Errorf("source %q is excluded by .boxrignore", instruction.Source)
	}
	destination, err := rootfsDestination(root, instruction.Destination)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyContextDirectory(source, destination, root, instruction.Source, ignorePatterns)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("COPY source must be a regular file or directory")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return fmt.Errorf("create COPY destination parent: %w", err)
	}
	return copyContextFile(source, destination, info.Mode())
}

func copyContextDirectory(source, destination, root, sourceRelative string, ignorePatterns []ignorePattern) error {
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	var directories []struct {
		path string
		mode os.FileMode
	}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		contextRelative := filepath.Join(sourceRelative, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if ignoredPath(contextRelative, info.IsDir(), ignorePatterns) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			directories = append(directories, struct {
				path string
				mode os.FileMode
			}{target, info.Mode()})
		case info.Mode().IsRegular():
			return copyContextFile(path, target, info.Mode())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := validateCopiedSymlink(root, target, link); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("build context contains unsupported special file %s", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Chmod(directories[index].path, directories[index].mode.Perm()); err != nil {
			return err
		}
	}
	return nil
}

type ignorePattern struct {
	value         string
	directoryOnly bool
}

// loadIgnorePatterns reads a small, deliberately non-negating ignore file.
// Negation rules are not supported so a parent directory exclusion cannot be
// accidentally bypassed by a later child rule.
func loadIgnorePatterns(contextRoot string) ([]ignorePattern, error) {
	ignorePath := filepath.Join(contextRoot, ".boxrignore")
	info, err := os.Lstat(ignorePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect .boxrignore: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxBuildIgnoreBytes {
		return nil, fmt.Errorf(".boxrignore must be a regular non-symlink file no larger than %d bytes", maxBuildIgnoreBytes)
	}
	file, err := os.Open(ignorePath)
	if err != nil {
		return nil, fmt.Errorf("open .boxrignore: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), maxBuildIgnoreLine)
	patterns := make([]ignorePattern, 0)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		value := strings.TrimSpace(scanner.Text())
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		if strings.HasPrefix(value, "!") {
			return nil, fmt.Errorf(".boxrignore line %d: negation rules are not supported", lineNumber)
		}
		value = strings.ReplaceAll(value, "\\", "/")
		if strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
			return nil, fmt.Errorf(".boxrignore line %d: pattern must be relative and contain no NUL", lineNumber)
		}
		directoryOnly := strings.HasSuffix(value, "/")
		value = strings.Trim(value, "/")
		if value == "" || hasParentTraversal(value) {
			return nil, fmt.Errorf(".boxrignore line %d: invalid pattern", lineNumber)
		}
		if _, err := path.Match(value, "probe"); err != nil {
			return nil, fmt.Errorf(".boxrignore line %d: invalid glob: %w", lineNumber, err)
		}
		patterns = append(patterns, ignorePattern{value: value, directoryOnly: directoryOnly})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read .boxrignore: %w", err)
	}
	return patterns, nil
}

func ignoredPath(relative string, isDir bool, patterns []ignorePattern) bool {
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean == "." || clean == "" {
		return false
	}
	components := strings.Split(clean, "/")
	for index := range components {
		prefix := strings.Join(components[:index+1], "/")
		isAncestor := index < len(components)-1
		componentIsDir := isAncestor || isDir
		for _, pattern := range patterns {
			if pattern.directoryOnly && !componentIsDir {
				continue
			}
			candidate := prefix
			if !strings.Contains(pattern.value, "/") {
				candidate = components[index]
			}
			matched, err := path.Match(pattern.value, candidate)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

func validateCopiedSymlink(root, target, link string) error {
	if strings.ContainsRune(link, '\x00') {
		return fmt.Errorf("symlink target contains NUL")
	}
	if filepath.IsAbs(link) {
		return nil // absolute symlinks are resolved from the container root after chroot
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), link))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink %s escapes the image rootfs", target)
	}
	return nil
}

func copyContextFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return fmt.Errorf("COPY source changed or is not a regular file")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	return os.Chmod(destination, mode.Perm())
}
