package container

import (
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options configures a container process. Resource limits are opt-in. The
// default filesystem and network modes intentionally preserve the original
// boxr v0.1 behaviour unless a root filesystem or isolated network is asked for.
type Options struct {
	SecurityProfile     string             `json:"securityProfile,omitempty"`
	ResourceProfile     string             `json:"resourceProfile,omitempty"`
	Name                string             `json:"name,omitempty"`
	User                string             `json:"user,omitempty"`
	Labels              map[string]string  `json:"labels,omitempty"`
	AutoRemove          bool               `json:"autoRemove,omitempty"`
	RestartPolicy       string             `json:"restartPolicy,omitempty"`
	RestartMaxRetries   int                `json:"restartMaxRetries,omitempty"`
	RestartDelayMS      int                `json:"restartDelayMs,omitempty"`
	Hostname            string             `json:"hostname,omitempty"`
	MemoryLimitBytes    int64              `json:"memoryLimitBytes,omitempty"`
	MemoryHighBytes     int64              `json:"memoryHighBytes,omitempty"`
	MemoryLowBytes      int64              `json:"memoryLowBytes,omitempty"`
	MemoryMinBytes      int64              `json:"memoryMinBytes,omitempty"`
	MemorySwapMaxBytes  int64              `json:"memorySwapMaxBytes,omitempty"`
	MemorySwapMaxSet    bool               `json:"memorySwapMaxSet,omitempty"`
	MemorySwapDefaulted bool               `json:"memorySwapDefaulted,omitempty"`
	MemorySwapUnlimited bool               `json:"memorySwapUnlimited,omitempty"`
	CPUs                float64            `json:"cpus,omitempty"`
	CPUWeight           int                `json:"cpuWeight,omitempty"`
	IOWeight            int                `json:"ioWeight,omitempty"`
	PidsLimit           int64              `json:"pidsLimit,omitempty"`
	MemoryOOMGroup      bool               `json:"memoryOOMGroup,omitempty"`
	CgroupEnabled       bool               `json:"cgroupEnabled,omitempty"`
	Workdir             string             `json:"workdir,omitempty"`
	Env                 []string           `json:"env,omitempty"`
	ClearEnv            bool               `json:"-"`
	RootFS              string             `json:"rootfs,omitempty"`
	OverlayDir          string             `json:"overlayDir,omitempty"`
	ReadOnlyRootFS      bool               `json:"readOnlyRootfs,omitempty"`
	MountProc           bool               `json:"mountProc,omitempty"`
	BindMounts          []BindMount        `json:"bindMounts,omitempty"`
	TmpfsMounts         []TmpfsMount       `json:"tmpfsMounts,omitempty"`
	NetworkMode         string             `json:"networkMode,omitempty"`
	NetworkName         string             `json:"networkName,omitempty"`
	PublishedPorts      []PortMapping      `json:"publishedPorts,omitempty"`
	ExposedPorts        []string           `json:"exposedPorts,omitempty"`
	StopSignal          string             `json:"stopSignal,omitempty"`
	Healthcheck         *HealthcheckConfig `json:"healthcheck,omitempty"`
	UserNamespace       bool               `json:"userNamespace,omitempty"`
	NoNewPrivileges     bool               `json:"noNewPrivileges,omitempty"`
	SeccompProfile      string             `json:"seccompProfile,omitempty"`
	DropCapabilities    []string           `json:"dropCapabilities,omitempty"`
}

// HealthcheckConfig is Docker-compatible health probe metadata. Durations use
// nanoseconds in the Engine API representation, as required by Docker's schema.
type HealthcheckConfig struct {
	Test          []string      `json:"Test,omitempty"`
	Interval      time.Duration `json:"Interval,omitempty"`
	Timeout       time.Duration `json:"Timeout,omitempty"`
	StartPeriod   time.Duration `json:"StartPeriod,omitempty"`
	StartInterval time.Duration `json:"StartInterval,omitempty"`
	Retries       int           `json:"Retries,omitempty"`
}

const (
	DefaultHealthcheckInterval      = 30 * time.Second
	DefaultHealthcheckTimeout       = 30 * time.Second
	DefaultHealthcheckStartInterval = 5 * time.Second
	DefaultHealthcheckRetries       = 3
	MaxHealthcheckOutputBytes       = 4096
	MaxHealthcheckCommandBytes      = 8192
)

func NormalizeHealthcheck(input *HealthcheckConfig) (*HealthcheckConfig, error) {
	if input == nil {
		return nil, nil
	}
	result := *input
	result.Test = append([]string(nil), input.Test...)
	if len(result.Test) == 1 && strings.EqualFold(result.Test[0], "NONE") {
		result.Test[0] = "NONE"
		if result.Interval != 0 || result.Timeout != 0 || result.StartPeriod != 0 || result.StartInterval != 0 || result.Retries != 0 {
			return nil, fmt.Errorf("HEALTHCHECK NONE cannot specify timing or retry options")
		}
		return &result, nil
	}
	if len(result.Test) < 2 || len(result.Test) > 17 {
		return nil, fmt.Errorf("healthcheck Test must contain CMD/CMD-SHELL and a command")
	}
	switch strings.ToUpper(result.Test[0]) {
	case "CMD":
		result.Test[0] = "CMD"
	case "CMD-SHELL":
		result.Test[0] = "CMD-SHELL"
		if len(result.Test) != 2 || strings.TrimSpace(result.Test[1]) == "" {
			return nil, fmt.Errorf("healthcheck CMD-SHELL requires exactly one non-empty command string")
		}
	default:
		return nil, fmt.Errorf("healthcheck Test must start with CMD, CMD-SHELL, or NONE")
	}
	totalBytes := 0
	for index, argument := range result.Test[1:] {
		if argument == "" || strings.ContainsRune(argument, '\x00') || strings.ContainsAny(argument, "\r\n") {
			return nil, fmt.Errorf("healthcheck command argument %d is empty or contains a control character", index+1)
		}
		totalBytes += len(argument)
	}
	if totalBytes > MaxHealthcheckCommandBytes {
		return nil, fmt.Errorf("healthcheck command exceeds %d bytes", MaxHealthcheckCommandBytes)
	}
	if result.Interval == 0 {
		result.Interval = DefaultHealthcheckInterval
	}
	if result.Timeout == 0 {
		result.Timeout = DefaultHealthcheckTimeout
	}
	if result.StartInterval == 0 {
		result.StartInterval = DefaultHealthcheckStartInterval
	}
	for name, duration := range map[string]time.Duration{
		"interval": result.Interval, "timeout": result.Timeout,
		"start-period": result.StartPeriod, "start-interval": result.StartInterval,
	} {
		if duration < 0 || duration > 24*time.Hour {
			return nil, fmt.Errorf("healthcheck %s must be between 0 and 24 hours", name)
		}
		if (name == "interval" || name == "timeout" || name == "start-interval") && duration < time.Millisecond {
			return nil, fmt.Errorf("healthcheck %s must be at least 1ms", name)
		}
	}
	if result.Retries == 0 {
		result.Retries = DefaultHealthcheckRetries
	}
	if result.Retries < 1 || result.Retries > 1000 {
		return nil, fmt.Errorf("healthcheck retries must be between 1 and 1000")
	}
	return &result, nil
}

func HealthcheckEnabled(config *HealthcheckConfig) bool {
	return config != nil && len(config.Test) > 0 && !(len(config.Test) == 1 && strings.EqualFold(config.Test[0], "NONE"))
}

// PortMapping publishes a host IPv4 address and port to a container TCP or UDP port.
type PortMapping struct {
	HostIP        string `json:"hostIp,omitempty"`
	HostPort      uint16 `json:"hostPort"`
	ContainerPort uint16 `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// Normalize applies safe defaults and rejects unsupported port mapping values.
func (mapping PortMapping) Normalize() (PortMapping, error) {
	if mapping.HostPort == 0 || mapping.ContainerPort == 0 {
		return PortMapping{}, fmt.Errorf("published host and container ports must be between 1 and 65535")
	}
	if mapping.HostIP == "" {
		mapping.HostIP = "0.0.0.0"
	}
	address, err := netip.ParseAddr(mapping.HostIP)
	if err != nil || !address.Is4() || address.String() != mapping.HostIP {
		return PortMapping{}, fmt.Errorf("published host IP must be a canonical IPv4 address")
	}
	mapping.Protocol = strings.ToLower(mapping.Protocol)
	if mapping.Protocol == "" {
		mapping.Protocol = "tcp"
	}
	if mapping.Protocol != "tcp" && mapping.Protocol != "udp" {
		return PortMapping{}, fmt.Errorf("published port protocol must be tcp or udp")
	}
	return mapping, nil
}

// NormalizeStopSignal validates Docker's supported stop signals and returns a
// canonical SIG-prefixed name suitable for persistence and process signalling.
func NormalizeStopSignal(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("stop signal must not contain whitespace or control characters")
	}
	name := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(value), "SIG"))
	if number, err := strconv.Atoi(name); err == nil {
		signals := map[int]string{
			1: "HUP", 2: "INT", 3: "QUIT", 9: "KILL",
			10: "USR1", 12: "USR2", 15: "TERM", 18: "CONT", 19: "STOP",
		}
		var ok bool
		name, ok = signals[number]
		if !ok {
			return "", fmt.Errorf("stop signal number %d is not supported", number)
		}
	}
	switch name {
	case "HUP", "INT", "QUIT", "KILL", "USR1", "USR2", "TERM", "CONT", "STOP":
		return "SIG" + name, nil
	default:
		return "", fmt.Errorf("stop signal %q is not supported", value)
	}
}

// NormalizeExposedPorts expands Docker-style ranges, applies the default TCP
// protocol and returns a sorted, duplicate-free set. It never publishes ports.
func NormalizeExposedPorts(values []string) ([]string, error) {
	const maxExposedPorts = 256
	set := make(map[string]struct{})
	for index, value := range values {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("exposed port %d is empty or contains whitespace/control characters", index+1)
		}
		portRange, protocol, hasProtocol := strings.Cut(value, "/")
		if !hasProtocol {
			protocol = "tcp"
		}
		protocol = strings.ToLower(protocol)
		if strings.Contains(protocol, "/") || (protocol != "tcp" && protocol != "udp" && protocol != "sctp") {
			return nil, fmt.Errorf("exposed port %q has an unsupported protocol", value)
		}
		first, last := portRange, portRange
		if strings.Contains(portRange, "-") {
			var ok bool
			first, last, ok = strings.Cut(portRange, "-")
			if !ok || first == "" || last == "" || strings.Contains(last, "-") {
				return nil, fmt.Errorf("exposed port %q has an invalid range", value)
			}
		}
		start, startErr := strconv.ParseUint(first, 10, 16)
		end, endErr := strconv.ParseUint(last, 10, 16)
		if startErr != nil || endErr != nil || start == 0 || end == 0 || end < start || end-start+1 > maxExposedPorts {
			return nil, fmt.Errorf("exposed port %q must be between 1 and 65535 with a range of at most %d ports", value, maxExposedPorts)
		}
		for port := start; port <= end; port++ {
			set[strconv.FormatUint(port, 10)+"/"+protocol] = struct{}{}
			if len(set) > maxExposedPorts {
				return nil, fmt.Errorf("at most %d unique exposed ports are supported", maxExposedPorts)
			}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func portMappingsConflict(left, right PortMapping) bool {
	if left.HostPort != right.HostPort || left.Protocol != right.Protocol {
		return false
	}
	return left.HostIP == "0.0.0.0" || right.HostIP == "0.0.0.0" || left.HostIP == right.HostIP
}

// BindMount describes a host path mounted at an absolute path inside RootFS.
type BindMount struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
	VolumeName string `json:"volumeName,omitempty"`
	CopyUp     bool   `json:"copyUp,omitempty"`
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
	OnReady func(hostPID int)
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
	if err := validateUserSpec(o.User); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if o.UserNamespace && !userSpecIsRoot(o.User) {
		return fmt.Errorf("non-root container users are not supported with the current single-ID user namespace mapping")
	}
	policy := o.RestartPolicy
	if policy == "" {
		policy = "no"
	}
	switch policy {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return fmt.Errorf("restart policy must be no, always, unless-stopped, or on-failure")
	}
	if o.RestartMaxRetries < 0 || o.RestartMaxRetries > 1000 {
		return fmt.Errorf("restart max retries must be between 0 and 1000")
	}
	if o.RestartMaxRetries > 0 && policy != "on-failure" {
		return fmt.Errorf("restart max retries can only be set with on-failure")
	}
	if policy == "no" && (o.RestartMaxRetries != 0 || o.RestartDelayMS != 0) {
		return fmt.Errorf("restart limits require a non-no restart policy")
	}
	if o.RestartDelayMS < 0 || o.RestartDelayMS > 300000 || (o.RestartDelayMS > 0 && o.RestartDelayMS < 100) {
		return fmt.Errorf("restart delay must be 0 (default) or between 100 and 300000 milliseconds")
	}
	if o.ResourceProfile != "" {
		switch o.ResourceProfile {
		case "default", "micro", "small", "medium", "large":
		default:
			return fmt.Errorf("unknown resource profile %q (supported: default, micro, small, medium, large)", o.ResourceProfile)
		}
	}
	if o.SecurityProfile != "" {
		switch o.SecurityProfile {
		case "default", "baseline", "restricted", "untrusted":
		default:
			return fmt.Errorf("unknown security profile %q (supported: default, baseline, restricted, untrusted)", o.SecurityProfile)
		}
	}
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
	switch o.NetworkMode {
	case "host", "none":
		if o.NetworkName != "" {
			return fmt.Errorf("a named network can only be used with network mode bridge")
		}
	case "bridge":
		if len(o.NetworkName) == 0 || len(o.NetworkName) > 31 {
			return fmt.Errorf("network mode bridge requires --network-name")
		}
		for index, character := range o.NetworkName {
			valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
				index > 0 && (character == '.' || character == '_' || character == '-')
			if !valid {
				return fmt.Errorf("network name must start with a lowercase letter or digit and contain only lowercase letters, digits, '.', '_' or '-'")
			}
		}
	default:
		return fmt.Errorf("network mode must be host, none, or bridge")
	}
	if _, err := NormalizeExposedPorts(o.ExposedPorts); err != nil {
		return fmt.Errorf("exposed ports: %w", err)
	}
	if _, err := NormalizeStopSignal(o.StopSignal); err != nil {
		return fmt.Errorf("stop signal: %w", err)
	}
	if _, err := NormalizeHealthcheck(o.Healthcheck); err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	if len(o.PublishedPorts) > 128 {
		return fmt.Errorf("at most 128 published port mappings are supported")
	}
	if len(o.PublishedPorts) > 0 && o.NetworkMode != "bridge" {
		return fmt.Errorf("published ports require network mode bridge")
	}
	normalizedPorts := make([]PortMapping, 0, len(o.PublishedPorts))
	for index, mapping := range o.PublishedPorts {
		normalized, err := mapping.Normalize()
		if err != nil {
			return fmt.Errorf("published port %d: %w", index+1, err)
		}
		for _, previous := range normalizedPorts {
			if portMappingsConflict(previous, normalized) {
				return fmt.Errorf("published host port %s:%d/%s is configured more than once", normalized.HostIP, normalized.HostPort, normalized.Protocol)
			}
		}
		normalizedPorts = append(normalizedPorts, normalized)
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


func validateUserSpec(spec string) error {
	if spec == "" {
		return nil
	}
	if len(spec) > 256 || strings.TrimSpace(spec) != spec || strings.ContainsAny(spec, "\x00\r\n\t ") {
		return fmt.Errorf("must be at most 256 bytes and contain no whitespace or control characters")
	}
	user, group, hasGroup := strings.Cut(spec, ":")
	if strings.Contains(group, ":") {
		return fmt.Errorf("must use USER or USER:GROUP")
	}
	if user == "" && (!hasGroup || group == "") {
		return fmt.Errorf("must specify a user or group")
	}
	if strings.ContainsAny(user+group, "/\\\\") {
		return fmt.Errorf("user and group names cannot contain path separators")
	}
	return nil
}

func userSpecIsRoot(spec string) bool {
	if spec == "" {
		return true
	}
	user, group, _ := strings.Cut(spec, ":")
	isRoot := user == "" || user == "root" || user == "0"
	return isRoot && (group == "" || group == "root" || group == "0")
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

// ApplySecurityProfile makes a named security profile a set of mandatory
// restrictions. It is intentionally applied after config and CLI parsing so
// less restrictive individual flags cannot silently override the profile.
// The profile is a convenience baseline, not a claim that a workload is safe.
func (o *Options) ApplySecurityProfile() error {
	if o == nil {
		return fmt.Errorf("options cannot be nil")
	}
	switch strings.ToLower(strings.TrimSpace(o.SecurityProfile)) {
	case "", "default":
		o.SecurityProfile = "default"
		return nil
	case "baseline":
		o.NoNewPrivileges = true
		o.SeccompProfile = "baseline"
		o.DropCapabilities = appendUniqueCapabilities(o.DropCapabilities, "NET_RAW")
	case "restricted":
		o.NetworkMode = "none"
		o.UserNamespace = true
		o.NoNewPrivileges = true
		o.SeccompProfile = "baseline"
		o.DropCapabilities = appendUniqueCapabilities(o.DropCapabilities,
			"SYS_ADMIN", "SYS_MODULE", "SYS_RAWIO", "SYS_PTRACE", "NET_ADMIN", "NET_RAW", "SYS_BOOT", "SYSLOG", "MKNOD", "DAC_OVERRIDE", "DAC_READ_SEARCH")
	case "untrusted":
		if o.RootFS == "" {
			return fmt.Errorf("untrusted security profile requires --rootfs or --image")
		}
		o.NetworkMode = "none"
		o.UserNamespace = true
		o.NoNewPrivileges = true
		o.SeccompProfile = "baseline"
		o.ReadOnlyRootFS = true
		o.DropCapabilities = []string{"ALL"}
	default:
		return fmt.Errorf("unknown security profile %q (supported: default, baseline, restricted, untrusted)", o.SecurityProfile)
	}
	o.SecurityProfile = strings.ToLower(strings.TrimSpace(o.SecurityProfile))
	return nil
}

func appendUniqueCapabilities(existing []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	result := make([]string, 0, len(existing)+len(additions))
	for _, value := range append(append([]string(nil), existing...), additions...) {
		key := strings.ToUpper(strings.TrimPrefix(value, "CAP_"))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

// ApplyResourceProfile fills only unset resource limits. Explicit config or
// command-line values always win over profile defaults.
func (o *Options) ApplyResourceProfile() error {
	if o == nil {
		return fmt.Errorf("options cannot be nil")
	}
	profile := strings.ToLower(strings.TrimSpace(o.ResourceProfile))
	type preset struct {
		memory    int64
		cpus      float64
		pids      int64
		cpuWeight int
		ioWeight  int
	}
	presets := map[string]preset{
		"micro":  {memory: 64 << 20, cpus: 0.25, pids: 64, cpuWeight: 50, ioWeight: 50},
		"small":  {memory: 256 << 20, cpus: 0.5, pids: 128, cpuWeight: 100, ioWeight: 100},
		"medium": {memory: 512 << 20, cpus: 1, pids: 256, cpuWeight: 100, ioWeight: 100},
		"large":  {memory: 1 << 30, cpus: 2, pids: 512, cpuWeight: 200, ioWeight: 200},
	}
	if profile == "" || profile == "default" {
		o.ResourceProfile = "default"
		return nil
	}
	selected, ok := presets[profile]
	if !ok {
		return fmt.Errorf("unknown resource profile %q (supported: default, micro, small, medium, large)", profile)
	}
	if o.MemoryLimitBytes == 0 {
		o.MemoryLimitBytes = selected.memory
	}
	if o.CPUs == 0 {
		o.CPUs = selected.cpus
	}
	if o.PidsLimit == 0 {
		o.PidsLimit = selected.pids
	}
	if o.CPUWeight == 0 {
		o.CPUWeight = selected.cpuWeight
	}
	if o.IOWeight == 0 {
		o.IOWeight = selected.ioWeight
	}
	o.ResourceProfile = profile
	return nil
}
