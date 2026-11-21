package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/manit6752025/boxr/internal/container"
	volumeStore "github.com/manit6752025/boxr/internal/volume"
	"gopkg.in/yaml.v3"
)

type composeDocument struct {
	Name     string                    `yaml:"name"`
	Version  yaml.Node                 `yaml:"version"`
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks"`
	Volumes  map[string]composeVolume  `yaml:"volumes"`
}

type composeVolume struct {
	Name     string    `yaml:"name"`
	External yaml.Node `yaml:"external"`
}

type composeNetwork struct {
	Name       string    `yaml:"name"`
	Driver     string    `yaml:"driver"`
	External   yaml.Node `yaml:"external"`
	Internal   bool      `yaml:"internal"`
	EnableIPv6 bool      `yaml:"enable_ipv6"`
	Attachable bool      `yaml:"attachable"`
}

type composeService struct {
	Image         string    `yaml:"image"`
	Command       yaml.Node `yaml:"command"`
	Entrypoint    yaml.Node `yaml:"entrypoint"`
	Build         yaml.Node `yaml:"build"`
	Volumes       yaml.Node `yaml:"volumes"`
	Healthcheck   yaml.Node `yaml:"healthcheck"`
	DependsOn     yaml.Node `yaml:"depends_on"`
	Environment   yaml.Node `yaml:"environment"`
	EnvFile       yaml.Node `yaml:"env_file"`
	Ports         yaml.Node `yaml:"ports"`
	Networks      yaml.Node `yaml:"networks"`
	Labels        yaml.Node `yaml:"labels"`
	Profiles      []string  `yaml:"profiles"`
	WorkingDir    string    `yaml:"working_dir"`
	Hostname      string    `yaml:"hostname"`
	NetworkMode   string    `yaml:"network_mode"`
	Restart        string    `yaml:"restart"`
	StopSignal     string    `yaml:"stop_signal"`
	StopGracePeriod yaml.Node `yaml:"stop_grace_period"`
	ContainerName string    `yaml:"container_name"`
	TTY           bool      `yaml:"tty"`
	StdinOpen     bool      `yaml:"stdin_open"`
	Privileged    bool      `yaml:"privileged"`
	ReadOnly      bool      `yaml:"read_only"`
	CapDrop       yaml.Node `yaml:"cap_drop"`
}

// decodeCompose reads the deliberately supported Compose subset into Boxr's
// existing deployment model. Unsupported Docker Compose semantics are rejected
// instead of being silently ignored or approximated with unsafe host settings.
func decodeCompose(data []byte, path string) (File, error) {
	var result File
	if len(data) > MaxManifestBytes {
		return result, fmt.Errorf("deployment manifest exceeds %d bytes", MaxManifestBytes)
	}

	// Decode to a YAML syntax tree first so interpolation can be applied to
	// scalar values without ever rewriting mapping keys or single-quoted values.
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return result, fmt.Errorf("decode Compose YAML: %w", err)
	}
	var trailingDocument yaml.Node
	if err := decoder.Decode(&trailingDocument); err != io.EOF {
		if err == nil {
			return result, fmt.Errorf("Compose YAML must contain exactly one document")
		}
		return result, fmt.Errorf("decode trailing Compose YAML: %w", err)
	}
	environment, err := loadComposeInterpolationEnvironment(path)
	if err != nil {
		return result, err
	}
	if err := interpolateComposeNode(&root, environment); err != nil {
		return result, fmt.Errorf("interpolate Compose YAML: %w", err)
	}
	expanded, err := yaml.Marshal(&root)
	if err != nil {
		return result, fmt.Errorf("encode interpolated Compose YAML: %w", err)
	}
	decoder = yaml.NewDecoder(strings.NewReader(string(expanded)))
	decoder.KnownFields(true)
	var document composeDocument
	if err := decoder.Decode(&document); err != nil {
		return result, fmt.Errorf("decode Compose YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return result, fmt.Errorf("Compose YAML must contain exactly one document")
		}
		return result, fmt.Errorf("decode trailing Compose YAML: %w", err)
	}
	if len(document.Services) == 0 {
		return result, fmt.Errorf("Compose file must define at least one service")
	}
	volumeNames, err := parseComposeExternalVolumes(document.Volumes)
	if err != nil {
		return result, err
	}

	networkNames := make(map[string]string, len(document.Networks))
	for key, network := range document.Networks {
		name, err := composeExternalNetworkName(key, network)
		if err != nil {
			return result, fmt.Errorf("Compose network %q: %w", key, err)
		}
		networkNames[key] = name
	}

	projectName := strings.TrimSpace(document.Name)
	if projectName == "" {
		projectName = composeProjectName(path)
	}
	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return result, fmt.Errorf("resolve Compose file directory: %w", err)
	}

	result = File{
		Version:  1,
		Name:     projectName,
		Services: make(map[string]Service, len(document.Services)),
	}
	for name, source := range document.Services {
		service, err := convertComposeService(name, source, networkNames, document.Networks, volumeNames, baseDir)
		if err != nil {
			return File{}, fmt.Errorf("Compose service %q: %w", name, err)
		}
		result.Services[name] = service
	}
	return result, nil
}

func composeProjectName(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "boxr"
	}
	value := strings.ToLower(filepath.Base(filepath.Dir(absolute)))
	var slug strings.Builder
	lastSeparator := false
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if valid {
			slug.WriteRune(r)
			lastSeparator = false
			continue
		}
		if !lastSeparator {
			slug.WriteByte('-')
			lastSeparator = true
		}
	}
	name := strings.Trim(slug.String(), "-_")
	if name == "" {
		name = "boxr"
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		name = "boxr-" + name
	}
	if len(name) > 32 {
		name = strings.TrimRight(name[:32], "-_")
	}
	return name
}

func composeExternalNetworkName(key string, network composeNetwork) (string, error) {
	if network.Internal || network.EnableIPv6 || network.Attachable {
		return "", fmt.Errorf("internal, IPv6, and attachable Compose networks are not supported")
	}
	if network.Driver != "" && network.Driver != "bridge" {
		return "", fmt.Errorf("only external bridge networks managed by Boxr are supported")
	}
	external := false
	externalName := ""
	switch network.External.Kind {
	case 0:
	case yaml.ScalarNode:
		value, err := strconv.ParseBool(strings.TrimSpace(network.External.Value))
		if err != nil {
			return "", fmt.Errorf("external must be true")
		}
		external = value
	case yaml.MappingNode:
		for index := 0; index+1 < len(network.External.Content); index += 2 {
			field := network.External.Content[index].Value
			value := network.External.Content[index+1]
			if field != "name" || value.Kind != yaml.ScalarNode || value.Value == "" {
				return "", fmt.Errorf("external mapping supports only a non-empty name")
			}
			externalName = value.Value
			external = true
		}
	default:
		return "", fmt.Errorf("external must be true or an external name mapping")
	}
	if !external {
		return "", fmt.Errorf("Boxr can attach only to a pre-created external network; create it with boxr network create")
	}
	name := network.Name
	if externalName != "" {
		if name != "" && name != externalName {
			return "", fmt.Errorf("network name conflicts with external.name")
		}
		name = externalName
	}
	if name == "" {
		name = key
	}
	options := container.Options{NetworkMode: "bridge", NetworkName: name}
	if err := options.Validate(); err != nil {
		return "", fmt.Errorf("invalid Boxr network name: %w", err)
	}
	return name, nil
}

func convertComposeService(name string, source composeService, networkNames map[string]string, networks map[string]composeNetwork, volumeNames map[string]string, baseDir string) (Service, error) {
	var result Service
	if source.Image == "" {
		if source.Build.Kind != 0 {
			return result, fmt.Errorf("build is not supported yet; build the image with boxr build and set image")
		}
		return result, fmt.Errorf("image is required")
	}
	if source.Build.Kind != 0 {
		return result, fmt.Errorf("build is not supported yet; build the image with boxr build and set image")
	}
	if source.Entrypoint.Kind != 0 {
		return result, fmt.Errorf("entrypoint overrides are not supported yet")
	}
	healthcheck, err := parseComposeHealthcheck(&source.Healthcheck)
	if err != nil {
		return result, err
	}
	if source.TTY || source.StdinOpen {
		return result, fmt.Errorf("interactive tty/stdin services are not supported yet")
	}
	if source.Privileged {
		return result, fmt.Errorf("privileged services are not supported")
	}
	if source.ContainerName != "" {
		return result, fmt.Errorf("container_name is managed by the Boxr deployment stack and cannot be overridden")
	}
	if source.Restart != "" && source.Restart != "no" {
		return result, fmt.Errorf("restart policy %q is not supported for attached deployments; use restart: 'no'", source.Restart)
	}

	stopSignal, err := parseComposeStopSignal(source.StopSignal)
	if err != nil {
		return result, err
	}
	stopGracePeriodMS, err := parseComposeStopGracePeriod(&source.StopGracePeriod)
	if err != nil {
		return result, err
	}

	command, err := parseComposeCommand(&source.Command)
	if err != nil {
		return result, err
	}
	dependencies, healthyDependencies, err := parseComposeDependencies(&source.DependsOn)
	if err != nil {
		return result, err
	}
	environment, err := parseComposeEnvironment(&source.Environment)
	if err != nil {
		return result, err
	}
	envFiles, err := parseComposeEnvFiles(&source.EnvFile, baseDir)
	if err != nil {
		return result, err
	}
	ports, err := parseComposePorts(&source.Ports)
	if err != nil {
		return result, err
	}
	labels, err := parseComposeLabels(&source.Labels)
	if err != nil {
		return result, err
	}
	capabilityDrops, err := parseComposeCapabilityDrops(&source.CapDrop)
	if err != nil {
		return result, err
	}
	namedVolumes, bindMounts, tmpfsMounts, err := parseComposeVolumeMounts(&source.Volumes, volumeNames, baseDir)
	if err != nil {
		return result, err
	}
	networkMode, networkName, err := resolveComposeServiceNetwork(source, networkNames, networks)
	if err != nil {
		return result, err
	}
	options := container.Options{
		NetworkMode:      networkMode,
		NetworkName:      networkName,
		PublishedPorts:   ports,
		Workdir:          source.WorkingDir,
		Hostname:         source.Hostname,
		Env:              environment,
		Labels:           labels,
		BindMounts:       bindMounts,
		TmpfsMounts:      tmpfsMounts,
		ReadOnlyRootFS:   source.ReadOnly,
		DropCapabilities: capabilityDrops,
		Healthcheck:      healthcheck,
		StopSignal:       stopSignal,
	}
	return Service{
		Image:     source.Image,
		Command:   command,
		Options:   options,
		DependsOn:        dependencies,
		DependsOnHealthy: healthyDependencies,
		Profiles:  append([]string(nil), source.Profiles...),
		EnvFiles:  envFiles,
		Volumes:       namedVolumes,
		StopTimeoutMS: stopGracePeriodMS,
	}, nil
}

func parseComposeExternalVolumes(definitions map[string]composeVolume) (map[string]string, error) {
	result := make(map[string]string, len(definitions))
	for key, definition := range definitions {
		if key == "" {
			return nil, fmt.Errorf("top-level Compose volume names cannot be empty")
		}
		if definition.External.Kind == 0 {
			return nil, fmt.Errorf("Compose volume %q must set external: true; Boxr does not create or delete Compose volumes automatically", key)
		}
		externalName := ""
		switch definition.External.Kind {
		case yaml.ScalarNode:
			value, err := strconv.ParseBool(strings.TrimSpace(definition.External.Value))
			if err != nil || !value {
				return nil, fmt.Errorf("Compose volume %q must set external: true", key)
			}
		case yaml.MappingNode:
			if len(definition.External.Content) == 0 {
				return nil, fmt.Errorf("Compose volume %q external mapping must provide a non-empty name", key)
			}
			for index := 0; index+1 < len(definition.External.Content); index += 2 {
				field := definition.External.Content[index].Value
				value := definition.External.Content[index+1]
				if field != "name" || value.Kind != yaml.ScalarNode || strings.TrimSpace(value.Value) == "" {
					return nil, fmt.Errorf("Compose volume %q external mapping supports only a non-empty name", key)
				}
				externalName = value.Value
			}
		default:
			return nil, fmt.Errorf("Compose volume %q external must be true or an external name mapping", key)
		}
		name := definition.Name
		if externalName != "" {
			if name != "" && name != externalName {
				return nil, fmt.Errorf("Compose volume %q name conflicts with external.name", key)
			}
			name = externalName
		}
		if name == "" {
			name = key
		}
		if err := volumeStore.ValidateName(name); err != nil {
			return nil, fmt.Errorf("Compose volume %q does not name a valid pre-created Boxr volume: %w", key, err)
		}
		result[key] = name
	}
	return result, nil
}

const (
	maxComposeMountCount            = 64
	maxComposeTmpfsBytes      int64 = 1 << 30
	maxComposeTmpfsTotalBytes int64 = 2 << 30
)

func parseComposeVolumeMounts(node *yaml.Node, volumeNames map[string]string, baseDir string) ([]VolumeMount, []container.BindMount, []container.TmpfsMount, error) {
	if node.Kind == 0 {
		return nil, nil, nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, nil, nil, fmt.Errorf("service volumes must be a sequence of short-form strings or long-form mount objects")
	}
	if len(node.Content) > maxComposeMountCount {
		return nil, nil, nil, fmt.Errorf("service volumes supports at most %d mounts", maxComposeMountCount)
	}
	var named []VolumeMount
	var binds []container.BindMount
	var tmpfs []container.TmpfsMount
	var totalTmpfs int64
	for index, item := range node.Content {
		if item.Kind == yaml.ScalarNode {
			mount, bind, err := parseComposeShortMount(item.Value, volumeNames, baseDir, index+1)
			if err != nil {
				return nil, nil, nil, err
			}
			if mount != nil {
				named = append(named, *mount)
			}
			if bind != nil {
				binds = append(binds, *bind)
			}
			continue
		}
		if item.Kind != yaml.MappingNode {
			return nil, nil, nil, fmt.Errorf("volume mount %d must be a short-form string or long-form object", index+1)
		}
		mount, bind, temporary, err := parseComposeLongMount(item, volumeNames, baseDir, index+1)
		if err != nil {
			return nil, nil, nil, err
		}
		if mount != nil {
			named = append(named, *mount)
		}
		if bind != nil {
			binds = append(binds, *bind)
		}
		if temporary != nil {
			if temporary.SizeBytes > maxComposeTmpfsTotalBytes-totalTmpfs {
				return nil, nil, nil, fmt.Errorf("combined Compose tmpfs mounts exceed %d bytes", maxComposeTmpfsTotalBytes)
			}
			totalTmpfs += temporary.SizeBytes
			tmpfs = append(tmpfs, *temporary)
		}
	}
	if err := validateComposeMountTargets(named, binds, tmpfs); err != nil {
		return nil, nil, nil, err
	}
	return named, binds, tmpfs, nil
}

func parseComposeShortMount(value string, volumeNames map[string]string, baseDir string, index int) (*VolumeMount, *container.BindMount, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil, fmt.Errorf("volume mount %d must not be empty", index)
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return nil, nil, fmt.Errorf("volume mount %d must use SOURCE:TARGET[:ro|rw]", index)
	}
	source, target := parts[0], parts[1]
	readOnly := false
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			readOnly = true
		case "rw":
		default:
			return nil, nil, fmt.Errorf("volume mount %d mode must be ro or rw", index)
		}
	}
	if err := validateComposeMountTarget(target); err != nil {
		return nil, nil, fmt.Errorf("volume mount %d target: %w", index, err)
	}
	if filepath.IsAbs(source) || source == "." || source == ".." || strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		resolved, err := resolveComposeBindSource(source, baseDir)
		if err != nil {
			return nil, nil, fmt.Errorf("volume mount %d bind source: %w", index, err)
		}
		return nil, &container.BindMount{Source: resolved, Target: filepath.Clean(target), ReadOnly: readOnly}, nil
	}
	name, exists := volumeNames[source]
	if !exists {
		return nil, nil, fmt.Errorf("volume mount %d references %q, which is not declared as a pre-created external Compose volume", index, source)
	}
	return &VolumeMount{Name: name, Target: filepath.Clean(target), ReadOnly: readOnly}, nil, nil
}

func composeMountMap(node *yaml.Node, context string, allowed ...string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return nil, fmt.Errorf("%s must be a mapping", context)
	}
	permitted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		permitted[key] = true
	}
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || strings.TrimSpace(key.Value) == "" {
			return nil, fmt.Errorf("%s keys must be non-empty strings", context)
		}
		if !permitted[key.Value] {
			return nil, fmt.Errorf("%s field %q is unsupported", context, key.Value)
		}
		if _, exists := result[key.Value]; exists {
			return nil, fmt.Errorf("%s field %q is duplicated", context, key.Value)
		}
		result[key.Value] = node.Content[index+1]
	}
	return result, nil
}

func composeMountValue(fields map[string]*yaml.Node, key, context string, required bool) (string, error) {
	node, exists := fields[key]
	if !exists {
		if required {
			return "", fmt.Errorf("%s.%s is required", context, key)
		}
		return "", nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return "", fmt.Errorf("%s.%s must be a scalar", context, key)
	}
	value := strings.TrimSpace(node.Value)
	if required && value == "" {
		return "", fmt.Errorf("%s.%s must not be empty", context, key)
	}
	return value, nil
}

func composeMountBoolean(fields map[string]*yaml.Node, key, context string, fallback bool) (bool, error) {
	node, exists := fields[key]
	if !exists {
		return fallback, nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return false, fmt.Errorf("%s.%s must be a boolean", context, key)
	}
	value, err := strconv.ParseBool(node.Value)
	if err != nil {
		return false, fmt.Errorf("%s.%s must be a boolean", context, key)
	}
	return value, nil
}

func validateComposeMountTarget(target string) error {
	if target == "" || !filepath.IsAbs(target) || filepath.Clean(target) == string(filepath.Separator) || strings.ContainsAny(target, "\x00\r\n") {
		return fmt.Errorf("target must be an absolute non-root path without backslashes or control characters")
	}
	for _, segment := range strings.Split(filepath.ToSlash(target), "/") {
		if segment == ".." {
			return fmt.Errorf("target cannot contain '..'")
		}
	}
	return nil
}

func parseComposeTmpfsSize(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, fmt.Errorf("tmpfs.size is required")
	}
	multiplier := int64(1)
	for _, suffix := range []struct {
		name   string
		factor int64
	}{
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"gi", 1 << 30}, {"g", 1 << 30},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"mi", 1 << 20}, {"m", 1 << 20},
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"ki", 1 << 10}, {"k", 1 << 10},
	} {
		if strings.HasSuffix(value, suffix.name) {
			multiplier = suffix.factor
			value = strings.TrimSpace(strings.TrimSuffix(value, suffix.name))
			break
		}
	}
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, ".+e") {
		return 0, fmt.Errorf("tmpfs.size must be a positive whole-byte value")
	}
	amount, err := strconv.ParseUint(value, 10, 64)
	maxInt64 := uint64(^uint64(0) >> 1)
	if err != nil || amount == 0 || amount > maxInt64/uint64(multiplier) {
		return 0, fmt.Errorf("tmpfs.size must be a positive whole-byte value")
	}
	return int64(amount * uint64(multiplier)), nil
}

func parseComposeLongMount(node *yaml.Node, volumeNames map[string]string, baseDir string, index int) (*VolumeMount, *container.BindMount, *container.TmpfsMount, error) {
	context := fmt.Sprintf("volume mount %d", index)
	fields, err := composeMountMap(node, context, "type", "source", "target", "read_only", "consistency", "bind", "volume", "tmpfs")
	if err != nil {
		return nil, nil, nil, err
	}
	kind, err := composeMountValue(fields, "type", context, true)
	if err != nil {
		return nil, nil, nil, err
	}
	target, err := composeMountValue(fields, "target", context, true)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateComposeMountTarget(target); err != nil {
		return nil, nil, nil, fmt.Errorf("%s.target: %w", context, err)
	}
	source, err := composeMountValue(fields, "source", context, kind != "tmpfs")
	if err != nil {
		return nil, nil, nil, err
	}
	readOnly, err := composeMountBoolean(fields, "read_only", context, false)
	if err != nil {
		return nil, nil, nil, err
	}
	consistency, err := composeMountValue(fields, "consistency", context, false)
	if err != nil {
		return nil, nil, nil, err
	}
	if consistency != "" && consistency != "default" && consistency != "consistent" {
		return nil, nil, nil, fmt.Errorf("%s.consistency %q is unsupported", context, consistency)
	}
	for _, key := range []string{"bind", "volume", "tmpfs"} {
		if fields[key] != nil && key != kind {
			return nil, nil, nil, fmt.Errorf("%s.%s options cannot be combined with type %q", context, key, kind)
		}
	}
	switch kind {
	case "bind":
		resolved, err := resolveComposeBindSource(source, baseDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s.bind source: %w", context, err)
		}
		if node := fields["bind"]; node != nil {
			bind, err := composeMountMap(node, context+".bind", "propagation", "create_host_path", "selinux")
			if err != nil {
				return nil, nil, nil, err
			}
			propagation, err := composeMountValue(bind, "propagation", context+".bind", false)
			if err != nil {
				return nil, nil, nil, err
			}
			if propagation != "" && propagation != "rprivate" {
				return nil, nil, nil, fmt.Errorf("%s.bind.propagation supports only rprivate", context)
			}
			createHostPath, err := composeMountBoolean(bind, "create_host_path", context+".bind", false)
			if err != nil {
				return nil, nil, nil, err
			}
			if createHostPath {
				return nil, nil, nil, fmt.Errorf("%s.bind.create_host_path must be false", context)
			}
			selinux, err := composeMountValue(bind, "selinux", context+".bind", false)
			if err != nil {
				return nil, nil, nil, err
			}
			if selinux != "" {
				return nil, nil, nil, fmt.Errorf("%s.bind.selinux relabeling is unsupported", context)
			}
		}
		return nil, &container.BindMount{Source: resolved, Target: filepath.Clean(target), ReadOnly: readOnly}, nil, nil
	case "volume":
		name, exists := volumeNames[source]
		if !exists {
			return nil, nil, nil, fmt.Errorf("%s references %q, which is not a declared external Boxr volume", context, source)
		}
		node := fields["volume"]
		if node == nil {
			return nil, nil, nil, fmt.Errorf("%s.volume.nocopy must be true because volume copy-up is not implemented", context)
		}
		volumeOptions, err := composeMountMap(node, context+".volume", "nocopy", "subpath")
		if err != nil {
			return nil, nil, nil, err
		}
		noCopy, err := composeMountBoolean(volumeOptions, "nocopy", context+".volume", false)
		if err != nil {
			return nil, nil, nil, err
		}
		if !noCopy {
			return nil, nil, nil, fmt.Errorf("%s.volume.nocopy must be true because volume copy-up is not implemented", context)
		}
		subpath, err := composeMountValue(volumeOptions, "subpath", context+".volume", false)
		if err != nil {
			return nil, nil, nil, err
		}
		if subpath != "" {
			return nil, nil, nil, fmt.Errorf("%s.volume.subpath is unsupported", context)
		}
		return &VolumeMount{Name: name, Target: filepath.Clean(target), ReadOnly: readOnly}, nil, nil, nil
	case "tmpfs":
		if source != "" {
			return nil, nil, nil, fmt.Errorf("%s.source must be empty for tmpfs", context)
		}
		if readOnly {
			return nil, nil, nil, fmt.Errorf("%s cannot be read-only tmpfs", context)
		}
		node := fields["tmpfs"]
		if node == nil {
			return nil, nil, nil, fmt.Errorf("%s.tmpfs must specify a bounded size", context)
		}
		tmpfsOptions, err := composeMountMap(node, context+".tmpfs", "size", "mode")
		if err != nil {
			return nil, nil, nil, err
		}
		size, err := composeMountValue(tmpfsOptions, "size", context+".tmpfs", true)
		if err != nil {
			return nil, nil, nil, err
		}
		sizeBytes, err := parseComposeTmpfsSize(size)
		if err != nil || sizeBytes > maxComposeTmpfsBytes {
			return nil, nil, nil, fmt.Errorf("%s.tmpfs.size must be between 1 byte and %d bytes", context, maxComposeTmpfsBytes)
		}
		modeValue, err := composeMountValue(tmpfsOptions, "mode", context+".tmpfs", false)
		if err != nil {
			return nil, nil, nil, err
		}
		mode := uint64(01777)
		if modeValue != "" {
			mode, err = strconv.ParseUint(modeValue, 10, 32)
			if err != nil || mode > 07777 {
				return nil, nil, nil, fmt.Errorf("%s.tmpfs.mode must be between 0 and 07777", context)
			}
		}
		return nil, nil, &container.TmpfsMount{Target: filepath.Clean(target), SizeBytes: sizeBytes, Mode: uint32(mode)}, nil
	default:
		return nil, nil, nil, fmt.Errorf("%s.type %q is unsupported (supported: bind, volume, tmpfs)", context, kind)
	}
}

func validateComposeMountTargets(named []VolumeMount, binds []container.BindMount, tmpfs []container.TmpfsMount) error {
	seen := make([]string, 0, len(named)+len(binds)+len(tmpfs))
	check := func(target string) error {
		for _, previous := range seen {
			if target == previous {
				return fmt.Errorf("multiple Compose mounts target %q", target)
			}
			relative, err := filepath.Rel(previous, target)
			if err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("nested Compose mount targets %q and %q are not supported", previous, target)
			}
			reverse, err := filepath.Rel(target, previous)
			if err == nil && reverse != "." && reverse != ".." && !filepath.IsAbs(reverse) && !strings.HasPrefix(reverse, ".."+string(filepath.Separator)) {
				return fmt.Errorf("nested Compose mount targets %q and %q are not supported", target, previous)
			}
		}
		seen = append(seen, target)
		return nil
	}
	for _, mount := range named {
		if err := check(filepath.Clean(mount.Target)); err != nil {
			return err
		}
	}
	for _, mount := range binds {
		if err := check(filepath.Clean(mount.Target)); err != nil {
			return err
		}
	}
	for _, mount := range tmpfs {
		if err := check(filepath.Clean(mount.Target)); err != nil {
			return err
		}
	}
	return nil
}

func resolveComposeBindSource(source, baseDir string) (string, error) {
	path := source
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve host path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) {
		return "", fmt.Errorf("host bind source must not be the filesystem root")
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("host bind source %q contains a symlink component", source)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("host bind source parent %q is not a directory", current)
		}
		if index == len(parts)-1 && !info.IsDir() && !info.Mode().IsRegular() {
			return "", fmt.Errorf("host bind source must be a regular file or directory")
		}
	}
	return absolute, nil
}

func resolveComposeServiceNetwork(source composeService, networkNames map[string]string, networks map[string]composeNetwork) (string, string, error) {
	if source.NetworkMode != "" {
		if source.Networks.Kind != 0 {
			return "", "", fmt.Errorf("network_mode and networks cannot both be set")
		}
		if source.NetworkMode != "host" && source.NetworkMode != "none" {
			return "", "", fmt.Errorf("network_mode %q is unsupported; use host, none, or one external Boxr network", source.NetworkMode)
		}
		return source.NetworkMode, "", nil
	}

	names, err := parseComposeNetworkReferences(&source.Networks)
	if err != nil {
		return "", "", err
	}
	if len(names) == 0 {
		if _, exists := networks["default"]; exists {
			names = []string{"default"}
		} else {
			return "", "", fmt.Errorf("Compose's implicit default network is not available; select network_mode: none|host or reference one pre-created external Boxr bridge network")
		}
	}
	if len(names) != 1 {
		return "", "", fmt.Errorf("a service can join one external Boxr bridge network; multiple Compose networks are not supported yet")
	}
	name, exists := networkNames[names[0]]
	if !exists {
		return "", "", fmt.Errorf("network %q is undefined or not an external Boxr network", names[0])
	}
	return "bridge", name, nil
}

func parseComposeNetworkReferences(node *yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	var result []string
	switch node.Kind {
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Value == "" {
				return nil, fmt.Errorf("networks must contain non-empty network names")
			}
			result = append(result, item.Value)
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind != yaml.ScalarNode || key.Value == "" {
				return nil, fmt.Errorf("networks must use non-empty network names")
			}
			if value.Kind != yaml.MappingNode || len(value.Content) != 0 {
				return nil, fmt.Errorf("per-service network options such as aliases, static addresses, and priorities are not supported")
			}
			result = append(result, key.Value)
		}
	default:
		return nil, fmt.Errorf("networks must be a sequence or mapping")
	}
	return result, nil
}

func parseComposeCommand(node *yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind == yaml.SequenceNode {
		result := make([]string, 0, len(node.Content))
		for index, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Value == "" {
				return nil, fmt.Errorf("command argument %d must be a non-empty scalar", index+1)
			}
			result = append(result, item.Value)
		}
		return result, nil
	}
	if node.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("command must be a string or string array")
	}
	return splitComposeCommand(node.Value)
}

// splitComposeCommand tokenizes a command string without executing a shell or
// expanding variables. Quotes and backslash escapes group arguments only.
func splitComposeCommand(value string) ([]string, error) {
	var result []string
	var token strings.Builder
	var quote rune
	escaped := false
	started := false
	for _, character := range value {
		if escaped {
			token.WriteRune(character)
			escaped = false
			started = true
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			} else {
				token.WriteRune(character)
			}
			started = true
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				token.WriteRune(character)
			}
			started = true
			continue
		}
		switch {
		case character == '\\':
			escaped = true
			started = true
		case character == '\'' || character == '"':
			quote = character
			started = true
		case unicode.IsSpace(character):
			if started {
				if token.Len() == 0 {
					return nil, fmt.Errorf("command contains an empty argument")
				}
				result = append(result, token.String())
				token.Reset()
				started = false
			}
		default:
			token.WriteRune(character)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("command contains an unterminated quote or escape")
	}
	if started {
		if token.Len() == 0 {
			return nil, fmt.Errorf("command contains an empty argument")
		}
		result = append(result, token.String())
	}
	return result, nil
}

func parseComposeDependencies(node *yaml.Node) ([]string, []string, error) {
	if node.Kind == 0 {
		return nil, nil, nil
	}
	var result []string
	var healthy []string
	switch node.Kind {
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Value == "" {
				return nil, nil, fmt.Errorf("depends_on must contain service names")
			}
			result = append(result, item.Value)
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			name, config := node.Content[index], node.Content[index+1]
			if name.Kind != yaml.ScalarNode || name.Value == "" || config.Kind != yaml.MappingNode {
				return nil, nil, fmt.Errorf("depends_on mapping values must use condition objects")
			}
			condition := "service_started"
			for field := 0; field+1 < len(config.Content); field += 2 {
				key, value := config.Content[field].Value, config.Content[field+1]
				switch key {
				case "condition":
					if value.Kind != yaml.ScalarNode || (value.Value != "service_started" && value.Value != "service_healthy" && value.Value != "") {
						return nil, nil, fmt.Errorf("depends_on condition %q is unsupported; supported conditions are service_started and service_healthy", value.Value)
					}
					if value.Value != "" {
						condition = value.Value
					}
				case "required":
					if value.Kind != yaml.ScalarNode || value.Value != "true" {
						return nil, nil, fmt.Errorf("optional dependencies are not supported")
					}
				case "restart":
					if value.Kind != yaml.ScalarNode || value.Value != "false" {
						return nil, nil, fmt.Errorf("dependency restart propagation is not supported")
					}
				default:
					return nil, nil, fmt.Errorf("depends_on option %q is not supported", key)
				}
			}
			result = append(result, name.Value)
			if condition == "service_healthy" {
				healthy = append(healthy, name.Value)
			}
		}
	default:
		return nil, nil, fmt.Errorf("depends_on must be a service list or mapping")
	}
	return result, healthy, nil
}

func parseComposeEnvironment(node *yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	var result []string
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind != yaml.ScalarNode || key.Value == "" || strings.ContainsAny(key.Value, "=\x00\r\n") {
				return nil, fmt.Errorf("environment keys must be non-empty strings without '=' or line breaks")
			}
			if value.Tag == "!!null" {
				if inherited, exists := os.LookupEnv(key.Value); exists {
					result = append(result, key.Value+"="+inherited)
				}
				continue
			}
			if value.Kind != yaml.ScalarNode || strings.ContainsRune(value.Value, '\x00') {
				return nil, fmt.Errorf("environment values must be scalar strings")
			}
			result = append(result, key.Value+"="+value.Value)
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Value == "" {
				return nil, fmt.Errorf("environment entries must be KEY or KEY=VALUE strings")
			}
			key, _, hasValue := strings.Cut(item.Value, "=")
			if key == "" || strings.ContainsAny(key, "\x00\r\n") {
				return nil, fmt.Errorf("environment entries must use valid non-empty keys")
			}
			if hasValue {
				result = append(result, item.Value)
			} else if inherited, exists := os.LookupEnv(key); exists {
				result = append(result, key+"="+inherited)
			}
		}
	default:
		return nil, fmt.Errorf("environment must be a mapping or sequence")
	}
	return container.MergeEnvironment(result), nil
}

func parseComposeEnvFiles(node *yaml.Node, baseDir string) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	values, err := composeStringList(node, "env_file")
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for index, value := range values {
		if value == "" {
			return nil, fmt.Errorf("env_file entry %d is empty", index+1)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(baseDir, value)
		}
		value = filepath.Clean(value)
		if !filepath.IsAbs(value) {
			return nil, fmt.Errorf("env_file entry %d could not be resolved to an absolute path", index+1)
		}
		result = append(result, value)
	}
	return result, nil
}

func parseComposePorts(node *yaml.Node) ([]container.PortMapping, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("ports must use short-form string entries")
	}
	result := make([]container.PortMapping, 0, len(node.Content))
	for index, item := range node.Content {
		if item.Kind != yaml.ScalarNode || item.Value == "" {
			return nil, fmt.Errorf("port mapping %d must be a short-form string", index+1)
		}
		mapping, err := parseComposePort(item.Value)
		if err != nil {
			return nil, fmt.Errorf("port mapping %d: %w", index+1, err)
		}
		result = append(result, mapping)
	}
	return result, nil
}

func parseComposePort(value string) (container.PortMapping, error) {
	var result container.PortMapping
	parts := strings.Split(value, "/")
	if len(parts) > 2 {
		return result, fmt.Errorf("invalid protocol suffix")
	}
	protocol := "tcp"
	if len(parts) == 2 {
		protocol = parts[1]
	}
	fields := strings.Split(parts[0], ":")
	switch len(fields) {
	case 2:
		host, err := parseComposePortNumber(fields[0])
		if err != nil {
			return result, fmt.Errorf("host port: %w", err)
		}
		target, err := parseComposePortNumber(fields[1])
		if err != nil {
			return result, fmt.Errorf("container port: %w", err)
		}
		result.HostPort, result.ContainerPort = host, target
	case 3:
		host, err := parseComposePortNumber(fields[1])
		if err != nil {
			return result, fmt.Errorf("host port: %w", err)
		}
		target, err := parseComposePortNumber(fields[2])
		if err != nil {
			return result, fmt.Errorf("container port: %w", err)
		}
		result.HostIP = fields[0]
		result.HostPort, result.ContainerPort = host, target
	default:
		return result, fmt.Errorf("expected HOST:CONTAINER or IPv4:HOST:CONTAINER; ephemeral, ranged, and IPv6 mappings are not supported")
	}
	result.Protocol = protocol
	normalized, err := result.Normalize()
	if err != nil {
		return result, err
	}
	return normalized, nil
}

func parseComposePortNumber(value string) (uint16, error) {
	if value == "" || strings.Contains(value, "-") {
		return 0, fmt.Errorf("port must be a single integer from 1 to 65535")
	}
	number, err := strconv.ParseUint(value, 10, 16)
	if err != nil || number == 0 {
		return 0, fmt.Errorf("port must be a single integer from 1 to 65535")
	}
	return uint16(number), nil
}

func parseComposeCapabilityDrops(node *yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("cap_drop must be a list of capability names")
	}
	if len(node.Content) > 64 {
		return nil, fmt.Errorf("cap_drop supports at most 64 capability names")
	}
	result := make([]string, 0, len(node.Content))
	seen := make(map[string]struct{}, len(node.Content))
	for index, item := range node.Content {
		if item.Kind != yaml.ScalarNode || strings.TrimSpace(item.Value) == "" {
			return nil, fmt.Errorf("cap_drop entry %d must be a non-empty capability name", index+1)
		}
		name := strings.ToUpper(strings.TrimSpace(item.Value))
		name = strings.TrimPrefix(name, "CAP_")
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	// Reuse the runtime's canonical Linux capability validation so Compose
	// cannot accept a value that the container launcher would later ignore.
	if err := (container.Options{DropCapabilities: result}).Validate(); err != nil {
		return nil, fmt.Errorf("cap_drop: %w", err)
	}
	return result, nil
}

func parseComposeLabels(node *yaml.Node) (map[string]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	result := make(map[string]string)
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind != yaml.ScalarNode || key.Value == "" {
				return nil, fmt.Errorf("label keys must be non-empty strings")
			}
			if value.Tag == "!!null" {
				result[key.Value] = ""
			} else if value.Kind == yaml.ScalarNode {
				result[key.Value] = value.Value
			} else {
				return nil, fmt.Errorf("label values must be scalar strings")
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Value == "" {
				return nil, fmt.Errorf("labels must use KEY or KEY=VALUE strings")
			}
			key, value, hasValue := strings.Cut(item.Value, "=")
			if key == "" {
				return nil, fmt.Errorf("label keys must be non-empty")
			}
			if !hasValue {
				value = ""
			}
			result[key] = value
		}
	default:
		return nil, fmt.Errorf("labels must be a mapping or sequence")
	}
	return result, nil
}

func composeStringList(node *yaml.Node, field string) ([]string, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		result := make([]string, 0, len(node.Content))
		for index, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%s entry %d must be a string", field, index+1)
			}
			result = append(result, item.Value)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("%s must be a string or string list", field)
	}
}
