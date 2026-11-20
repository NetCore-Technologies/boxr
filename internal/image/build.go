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

	"github.com/manit6752025/boxr/internal/container"
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

	dockerfileSteps      []dockerfileStep
	dockerfileNetwork    string
	dockerfileHasWorkdir bool
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
		if err := ValidateReference(spec.Base); err != nil {
			return fmt.Errorf("build base must be scratch or a valid local image reference: %w", err)
		}
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
	if clean == string(filepath.Separator) {
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
	spec, err := LoadBuildSpec(specPath)
	if err != nil {
		return Image{}, err
	}
	return buildWithSpec(contextDir, spec, reference, platform, ".boxrignore")
}

func buildWithSpec(contextDir string, spec BuildSpec, reference, platform, ignoreFile string, additionalIgnorePaths ...string) (Image, error) {
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

	// Base images are resolved only from the verified local OCI store. Builds
	// never trigger an implicit network pull or execute anything from the base.
	var baseImage *Image
	if spec.Base != "scratch" {
		verified, err := Verify(spec.Base)
		if err != nil {
			return Image{}, fmt.Errorf("verify base image %q (base images must already exist locally; pull first with `boxr image pull`): %w", spec.Base, err)
		}
		if verified.OS != osName || verified.Arch != architecture {
			return Image{}, fmt.Errorf("base image %q platform %s/%s does not match build target %s/%s", spec.Base, verified.OS, verified.Arch, osName, architecture)
		}
		if variant != "" && verified.Variant != variant {
			return Image{}, fmt.Errorf("base image %q variant %q does not match build target variant %q", spec.Base, verified.Variant, variant)
		}
		if variant == "" && verified.Variant != "" {
			if _, _, _, err := parseBuildPlatform(osName + "/" + architecture + "/" + verified.Variant); err != nil {
				return Image{}, fmt.Errorf("base image %q has an unsupported platform variant: %w", spec.Base, err)
			}
			variant = verified.Variant
		}
		if err := ValidateBuildSpec(BuildSpec{Version: 1, Base: "scratch", Config: verified.RuntimeConfig}); err != nil {
			return Image{}, fmt.Errorf("base image %q has invalid runtime configuration: %w", spec.Base, err)
		}
		baseImage = &verified
	}

	ignorePatterns, err := loadIgnorePatternsNamed(contextAbs, ignoreFile)
	if err != nil {
		return Image{}, err
	}
	for _, ignored := range additionalIgnorePaths {
		if ignored != "" && ignored != "." {
			ignorePatterns = append(ignorePatterns, ignorePattern{value: filepath.ToSlash(ignored)})
		}
	}
	stage, err := os.MkdirTemp("", "boxr-image-build-*")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(stage)
	root := filepath.Join(stage, "rootfs")
	if baseImage != nil {
		if _, err := Unpack(spec.Base, root); err != nil {
			return Image{}, fmt.Errorf("extract verified base image %q: %w", spec.Base, err)
		}
	} else if err := os.Mkdir(root, 0755); err != nil {
		return Image{}, err
	}
	runtimeConfig := spec.Config
	var baseEnvironment []string
	workingDir := "/"
	if baseImage != nil {
		runtimeConfig = inheritBuildRuntimeConfig(baseImage.RuntimeConfig, spec.Config)
		baseEnvironment = baseImage.RuntimeConfig.Env
		if baseImage.RuntimeConfig.WorkingDir != "" {
			workingDir = baseImage.RuntimeConfig.WorkingDir
		}
	}

	if len(spec.dockerfileSteps) == 0 {
		for index, instruction := range spec.Copy {
			if err := applyCopy(contextAbs, root, instruction, ignorePatterns); err != nil {
				return Image{}, fmt.Errorf("COPY %d: %w", index+1, err)
			}
		}
	} else {
		runNumber := 0
		copyNumber := 0
		for _, step := range spec.dockerfileSteps {
			switch step.kind {
			case "copy":
				destination := step.destination
				if !path.IsAbs(destination) {
					destination = path.Join(workingDir, destination)
				}
				copyNumber++
				instruction := CopyInstruction{Source: step.source, Destination: path.Clean(destination)}
				if err := applyCopy(contextAbs, root, instruction, ignorePatterns); err != nil {
					return Image{}, fmt.Errorf("COPY %d: %w", copyNumber, err)
				}
			case "workdir":
				if path.IsAbs(step.value) {
					workingDir = path.Clean(step.value)
				} else {
					workingDir = path.Clean(path.Join(workingDir, step.value))
				}
				destination, err := rootfsDestination(root, workingDir)
				if err != nil {
					return Image{}, err
				}
				if _, err := ensureRootfsDirectory(root, destination); err != nil {
					return Image{}, fmt.Errorf("create Dockerfile WORKDIR %q: %w", workingDir, err)
				}
			case "run":
				runNumber++
				buildUser := step.user
				if buildUser == "" && baseImage != nil {
					buildUser = baseImage.RuntimeConfig.User
				}
				if buildUser != "" && buildUser != "root" && buildUser != "0" && buildUser != "0:0" {
					return Image{}, fmt.Errorf("RUN %d after USER %q is not supported yet; build commands currently execute as root inside a user namespace", runNumber, buildUser)
				}
				networkMode := spec.dockerfileNetwork
				if networkMode == "" {
					networkMode = "none"
				}
				runOptions := container.Options{
					RootFS: root, Workdir: workingDir, Env: mergeBuildEnvironment(baseEnvironment, step.env), ClearEnv: true,
					NetworkMode: networkMode, UserNamespace: true, MountProc: true,
					NoNewPrivileges: true, SeccompProfile: "baseline",
					DropCapabilities: []string{"ALL"},
				}
				if len(step.command) == 0 {
					return Image{}, fmt.Errorf("RUN %d: command is empty", runNumber)
				}
				if err := runDockerfileCommand(step.command[0], step.command[1:], runOptions); err != nil {
					return Image{}, fmt.Errorf("RUN %d: %w", runNumber, err)
				}
			}
		}
		if spec.dockerfileHasWorkdir {
			runtimeConfig.WorkingDir = workingDir
		}
	}
	if runtimeConfig.WorkingDir != "" {
		destination, err := rootfsDestination(root, runtimeConfig.WorkingDir)
		if err != nil {
			return Image{}, err
		}
		if _, err := ensureRootfsDirectory(root, destination); err != nil {
			return Image{}, fmt.Errorf("create image WorkingDir: %w", err)
		}
	}
	return ImportRootFSWithConfig(root, reference, osName, architecture, variant, runtimeConfig)
}

// runDockerfileCommand is replaceable by tests so build steps can be
// verified without requiring privileged namespace creation in the test process.
var runDockerfileCommand = container.Run

// inheritBuildRuntimeConfig carries forward useful image defaults while
// allowing a child recipe to override only the fields it explicitly supplies.
func inheritBuildRuntimeConfig(base, overrides ImageRuntimeConfig) ImageRuntimeConfig {
	result := ImageRuntimeConfig{
		Env:        mergeBuildEnvironment(base.Env, overrides.Env),
		Entrypoint: cloneBuildStrings(base.Entrypoint),
		Cmd:        cloneBuildStrings(base.Cmd),
		WorkingDir: base.WorkingDir,
		User:       base.User,
		Labels:     make(map[string]string, len(base.Labels)+len(overrides.Labels)),
	}
	if overrides.Entrypoint != nil {
		result.Entrypoint = cloneBuildStrings(overrides.Entrypoint)
	}
	if overrides.Cmd != nil {
		result.Cmd = cloneBuildStrings(overrides.Cmd)
	}
	if overrides.WorkingDir != "" {
		result.WorkingDir = overrides.WorkingDir
	}
	if overrides.User != "" {
		result.User = overrides.User
	}
	for key, value := range base.Labels {
		result.Labels[key] = value
	}
	for key, value := range overrides.Labels {
		result.Labels[key] = value
	}
	if len(result.Labels) == 0 {
		result.Labels = nil
	}
	return result
}

// mergeBuildEnvironment preserves base-variable ordering and replaces values
// by key when a child recipe supplies the same variable.
func mergeBuildEnvironment(base, overrides []string) []string {
	if base == nil && overrides == nil {
		return nil
	}
	result := make([]string, 0, len(base)+len(overrides))
	positions := make(map[string]int, len(base)+len(overrides))
	for _, group := range [][]string{base, overrides} {
		for _, entry := range group {
			key, _, _ := strings.Cut(entry, "=")
			if index, exists := positions[key]; exists {
				result[index] = entry
				continue
			}
			positions[key] = len(result)
			result = append(result, entry)
		}
	}
	return result
}

func cloneBuildStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
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

// normalizeBuildSymlink rewrites container-absolute link targets to paths
// relative to the staged rootfs, never the build host's filesystem root.
func normalizeBuildSymlink(root, destination, link string) (string, error) {
	if !filepath.IsAbs(link) {
		return link, nil
	}
	target, err := rootfsDestination(root, link)
	if err != nil {
		return "", fmt.Errorf("invalid absolute symlink target: %w", err)
	}
	relative, err := filepath.Rel(filepath.Dir(destination), target)
	if err != nil {
		return "", fmt.Errorf("resolve absolute symlink target: %w", err)
	}
	return relative, nil
}

// ensureRootfsDirectory creates a directory path without following symlinks
// outside the image root. In-root links are resolved so paths such as
// /bin -> /usr/bin continue to behave as expected during an image build.
func ensureRootfsDirectory(root, directory string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	directoryAbs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, directoryAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("directory escapes the image rootfs")
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil {
		return "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", fmt.Errorf("image rootfs must remain a real directory")
	}
	if rel == "." {
		return rootAbs, nil
	}
	current := rootAbs
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next := filepath.Join(current, component)
		info, statErr := os.Lstat(next)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(next, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("create rootfs directory %s: %w", component, err)
			}
			info, statErr = os.Lstat(next)
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", fmt.Errorf("resolve rootfs symlink %s: %w", component, err)
			}
			inside, err := filepath.Rel(rootAbs, resolved)
			if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("rootfs symlink %s escapes the image rootfs", component)
			}
			resolvedInfo, err := os.Stat(resolved)
			if err != nil || !resolvedInfo.IsDir() {
				return "", fmt.Errorf("rootfs symlink %s does not resolve to a directory", component)
			}
			current = resolved
			continue
		}
		if !info.IsDir() {
			return "", fmt.Errorf("rootfs path component %s is not a directory", component)
		}
		current = next
	}
	return current, nil
}

// resolveRootfsTarget resolves and creates the parent path before the caller
// opens a file, preventing a copied symlink from redirecting writes onto host paths.
func resolveRootfsTarget(root, target string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target escapes the image rootfs")
	}
	if rel == "." {
		return rootAbs, nil
	}
	parent, err := ensureRootfsDirectory(rootAbs, filepath.Dir(targetAbs))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(targetAbs))
	inside, err := filepath.Rel(rootAbs, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resolved target escapes the image rootfs")
	}
	return resolved, nil
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
		destination, err = ensureRootfsDirectory(root, destination)
		if err != nil {
			return fmt.Errorf("prepare COPY destination: %w", err)
		}
		return copyContextDirectory(source, destination, root, instruction.Source, ignorePatterns)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("COPY source must be a regular file or directory")
	}
	destination, err = resolveRootfsTarget(root, destination)
	if err != nil {
		return fmt.Errorf("prepare COPY destination: %w", err)
	}
	return copyContextFile(source, destination, info.Mode())
}

func copyContextDirectory(source, destination, root, sourceRelative string, ignorePatterns []ignorePattern) error {
	var err error
	destination, err = ensureRootfsDirectory(root, destination)
	if err != nil {
		return err
	}
	var directories []struct {
		path string
		mode os.FileMode
	}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
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
			target, err = ensureRootfsDirectory(root, target)
			if err != nil {
				return err
			}
			directories = append(directories, struct {
				path string
				mode os.FileMode
			}{target, info.Mode()})
		case info.Mode().IsRegular():
			target, err = resolveRootfsTarget(root, target)
			if err != nil {
				return err
			}
			return copyContextFile(path, target, info.Mode())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			link, err = normalizeBuildSymlink(root, target, link)
			if err != nil {
				return err
			}
			if err := validateCopiedSymlink(root, target, link); err != nil {
				return err
			}
			parent, err := ensureRootfsDirectory(root, filepath.Dir(target))
			if err != nil {
				return err
			}
			if err := os.Symlink(link, filepath.Join(parent, filepath.Base(target))); err != nil {
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
func loadIgnorePatternsNamed(contextRoot, filename string) ([]ignorePattern, error) {
	if filename == "" || filepath.Base(filename) != filename {
		return nil, fmt.Errorf("invalid build ignore filename")
	}
	ignorePath := filepath.Join(contextRoot, filename)
	info, err := os.Lstat(ignorePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", filename, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxBuildIgnoreBytes {
		return nil, fmt.Errorf("%s must be a regular non-symlink file no larger than %d bytes", filename, maxBuildIgnoreBytes)
	}
	file, err := os.Open(ignorePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filename, err)
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
			return nil, fmt.Errorf("%s line %d: negation rules are not supported", filename, lineNumber)
		}
		value = strings.ReplaceAll(value, "\\", "/")
		if strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
			return nil, fmt.Errorf("%s line %d: pattern must be relative and contain no NUL", filename, lineNumber)
		}
		directoryOnly := strings.HasSuffix(value, "/")
		value = strings.Trim(value, "/")
		if value == "" || hasParentTraversal(value) {
			return nil, fmt.Errorf("%s line %d: invalid pattern", filename, lineNumber)
		}
		if _, err := path.Match(value, "probe"); err != nil {
			return nil, fmt.Errorf("%s line %d: invalid glob: %w", filename, lineNumber, err)
		}
		patterns = append(patterns, ignorePattern{value: value, directoryOnly: directoryOnly})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filename, err)
	}
	return patterns, nil
}

func loadIgnorePatterns(contextRoot string) ([]ignorePattern, error) {
	return loadIgnorePatternsNamed(contextRoot, ".boxrignore")
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
