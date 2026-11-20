package container

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Options configures a container process. Resource limits are opt-in. The
// default filesystem and network modes intentionally preserve the original
// boxr v0.1 behaviour unless a root filesystem or isolated network is asked for.
type Options struct {
	Name               string            `json:"name,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	AutoRemove         bool              `json:"autoRemove,omitempty"`
	Hostname           string            `json:"hostname,omitempty"`
	MemoryLimitBytes   int64             `json:"memoryLimitBytes,omitempty"`
	MemoryHighBytes    int64             `json:"memoryHighBytes,omitempty"`
	MemoryLowBytes     int64             `json:"memoryLowBytes,omitempty"`
	MemoryMinBytes     int64             `json:"memoryMinBytes,omitempty"`
	MemorySwapMaxBytes int64             `json:"memorySwapMaxBytes,omitempty"`
	CPUs               float64           `json:"cpus,omitempty"`
	CPUWeight          int               `json:"cpuWeight,omitempty"`
	IOWeight           int               `json:"ioWeight,omitempty"`
	PidsLimit          int64             `json:"pidsLimit,omitempty"`
	MemoryOOMGroup     bool              `json:"memoryOOMGroup,omitempty"`
	Workdir            string            `json:"workdir,omitempty"`
	Env                []string          `json:"env,omitempty"`
	RootFS             string            `json:"rootfs,omitempty"`
	OverlayDir         string            `json:"overlayDir,omitempty"`
	ReadOnlyRootFS     bool              `json:"readOnlyRootfs,omitempty"`
	MountProc          bool              `json:"mountProc,omitempty"`
	BindMounts         []BindMount       `json:"bindMounts,omitempty"`
	TmpfsMounts        []TmpfsMount      `json:"tmpfsMounts,omitempty"`
	NetworkMode        string            `json:"networkMode,omitempty"`
	UserNamespace      bool              `json:"userNamespace,omitempty"`
	NoNewPrivileges    bool              `json:"noNewPrivileges,omitempty"`
	SeccompProfile     string            `json:"seccompProfile,omitempty"`
	DropCapabilities   []string          `json:"dropCapabilities,omitempty"`
}

// BindMount describes a host path mounted at an absolute path inside RootFS.
type BindMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// TmpfsMount describes a bounded tmpfs mount inside RootFS.
type TmpfsMount struct {
	Target    string `json:"target"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
}

// Hooks expose the host-side lifecycle without requiring a long-running daemon.
// The callbacks run in the host-side Boxr process.
type Hooks struct {
	OnStart func(hostPID int) error
	OnExit  func(hostPID int, runErr error) error
	Stdout  io.Writer
	Stderr  io.Writer
}

// ExitError reports the exit status of the command run inside the container.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return "container command exited with status " + strconv.Itoa(e.Code)
}

// Validate rejects configurations that cannot be represented safely by the
// runtime. It does not check privileges or mutate the host.
func (o Options) Validate() error {
	if o.Name != "" {
		if len(o.Name) > 64 {
			return fmt.Errorf("container name must be at most 64 characters")
		}
		for index, char := range o.Name {
			valid := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || index > 0 && (char == '-' || char == '_' || char == '.')
			if !valid {
				return fmt.Errorf("container name %q contains an invalid character", o.Name)
			}
		}
	}
	if len(o.Labels) > 64 {
		return fmt.Errorf("at most 64 container labels are supported")
	}
	for key, value := range o.Labels {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 1024 || strings.ContainsAny(key+value, "\x00\r\n") || strings.Contains(key, "=") {
			return fmt.Errorf("invalid container label %q", key)
		}
	}
	if o.MemoryLimitBytes < 0 || o.MemoryHighBytes < 0 || o.MemoryLowBytes < 0 || o.MemoryMinBytes < 0 || o.MemorySwapMaxBytes < 0 {
		return fmt.Errorf("memory limits cannot be negative")
	}
	if o.PidsLimit < 0 {
		return fmt.Errorf("PID limit cannot be negative")
	}
	if o.CPUWeight != 0 && (o.CPUWeight < 1 || o.CPUWeight > 10000) {
		return fmt.Errorf("CPU weight must be between 1 and 10000")
	}
	if o.IOWeight != 0 && (o.IOWeight < 1 || o.IOWeight > 10000) {
		return fmt.Errorf("I/O weight must be between 1 and 10000")
	}
	if o.NetworkMode == "" {
		o.NetworkMode = "host"
	}
	if o.NetworkMode != "host" && o.NetworkMode != "none" {
		return fmt.Errorf("network mode must be host or none")
	}
	needsRootFS := o.ReadOnlyRootFS || o.MountProc || len(o.BindMounts) > 0 || len(o.TmpfsMounts) > 0 || o.OverlayDir != ""
	if needsRootFS && o.RootFS == "" {
		return fmt.Errorf("--rootfs is required with rootfs, bind, tmpfs, or proc mount options")
	}
	if o.RootFS != "" {
		if !filepath.IsAbs(o.RootFS) || filepath.Clean(o.RootFS) == "/" {
			return fmt.Errorf("rootfs must be an absolute directory other than the host root /")
		}
	}
	if o.OverlayDir != "" {
		if !filepath.IsAbs(o.OverlayDir) || filepath.Clean(o.OverlayDir) == "/" {
			return fmt.Errorf("overlay directory must be an absolute directory other than /")
		}
		if o.RootFS == "" {
			return fmt.Errorf("--rootfs is required with --overlay-dir")
		}
		root := filepath.Clean(o.RootFS)
		overlay := filepath.Clean(o.OverlayDir)
		if pathWithin(root, overlay) || pathWithin(overlay, root) {
			return fmt.Errorf("overlay directory and rootfs must be separate directory trees")
		}
		for _, value := range []string{root, overlay} {
			if strings.ContainsAny(value, ",:\\") {
				return fmt.Errorf("overlay paths cannot contain commas, colons, or backslashes")
			}
		}
	}
	if o.Workdir != "" && o.RootFS != "" && !filepath.IsAbs(o.Workdir) {
		return fmt.Errorf("workdir must be absolute when --rootfs is set")
	}
	for i, mount := range o.BindMounts {
		if !filepath.IsAbs(mount.Source) || !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) == "/" {
			return fmt.Errorf("bind mount %d source and target must be absolute paths", i+1)
		}
		if hasDotDot(mount.Target) {
			return fmt.Errorf("bind mount %d target cannot contain '..'", i+1)
		}
	}
	for i, mount := range o.TmpfsMounts {
		if !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) == "/" || hasDotDot(mount.Target) {
			return fmt.Errorf("tmpfs mount %d target must be an absolute path without '..'", i+1)
		}
		if mount.SizeBytes < 0 || mount.Mode > 07777 {
			return fmt.Errorf("tmpfs mount %d has an invalid size or mode", i+1)
		}
	}
	if !seccompProfileSupported(o.SeccompProfile) {
		return fmt.Errorf("seccomp profile %q is unsupported on %s/%s (supported Linux profile: baseline)", o.SeccompProfile, runtime.GOOS, runtime.GOARCH)
	}
	for _, capability := range o.DropCapabilities {
		if _, ok := capabilityNumber(capability); !ok && !strings.EqualFold(capability, "ALL") {
			return fmt.Errorf("unknown Linux capability %q", capability)
		}
	}
	return nil
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func hasDotDot(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// mergeEnvironment applies KEY=VALUE overrides without leaving duplicate keys
// in the environment passed to execve. Duplicate keys have platform-dependent
// lookup behaviour, so boxr makes the caller's explicit override unambiguous.
func mergeEnvironment(base, overrides []string) []string {
	merged := make([]string, 0, len(base)+len(overrides))
	positions := make(map[string]int, len(base)+len(overrides))
	set := func(entry string) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return
		}
		if index, exists := positions[key]; exists {
			merged[index] = entry
			return
		}
		positions[key] = len(merged)
		merged = append(merged, entry)
	}
	for _, entry := range base {
		set(entry)
	}
	for _, entry := range overrides {
		set(entry)
	}
	return merged
}

// MergeEnvironment combines KEY=VALUE entries while preserving first-seen key
// order and using the last value supplied for each key.
func MergeEnvironment(groups ...[]string) []string {
	merged := make([]string, 0)
	for _, group := range groups {
		merged = mergeEnvironment(merged, group)
	}
	return merged
}
