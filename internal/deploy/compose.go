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
	"gopkg.in/yaml.v3"
)

type composeDocument struct {
	Name     string                    `yaml:"name"`
	Version  yaml.Node                 `yaml:"version"`
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks"`
	Volumes  yaml.Node                 `yaml:"volumes"`
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
	Restart       string    `yaml:"restart"`
	ContainerName string    `yaml:"container_name"`
	TTY           bool      `yaml:"tty"`
	StdinOpen     bool      `yaml:"stdin_open"`
	Privileged    bool      `yaml:"privileged"`
}

// decodeCompose reads the deliberately supported Compose subset into Boxr's
// existing deployment model. Unsupported Docker Compose semantics are rejected
// instead of being silently ignored or approximated with unsafe host settings.
func decodeCompose(data []byte, path string) (File, error) {
	var result File
	if len(data) > MaxManifestBytes {
		return result, fmt.Errorf("deployment manifest exceeds %d bytes", MaxManifestBytes)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
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
	if document.Volumes.Kind != 0 && !(document.Volumes.Kind == yaml.SequenceNode && len(document.Volumes.Content) == 0) &&
		!(document.Volumes.Kind == yaml.MappingNode && len(document.Volumes.Content) == 0) {
		return result, fmt.Errorf("top-level Compose volumes are not supported yet; use Boxr-managed volumes in a JSON deployment manifest")
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
		service, err := convertComposeService(name, source, networkNames, document.Networks, baseDir)
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

func convertComposeService(name string, source composeService, networkNames map[string]string, networks map[string]composeNetwork, baseDir string) (Service, error) {
	var result Service
	if source.Image == "" {
		if source.Build.Kind != 0 {
			return result, fmt.Errorf("build is not supported yet; build the image with boxr build and set image")
		}
		return result, fmt.Errorf("image is required")
	}
	if strings.Contains(source.Image, "$") {
		return result, fmt.Errorf("Compose variable interpolation is not supported yet; provide a literal image reference")
	}
	if source.Build.Kind != 0 {
		return result, fmt.Errorf("build is not supported yet; build the image with boxr build and set image")
	}
	if source.Entrypoint.Kind != 0 {
		return result, fmt.Errorf("entrypoint overrides are not supported yet")
	}
	if source.Volumes.Kind != 0 && len(source.Volumes.Content) > 0 {
		return result, fmt.Errorf("service volumes are not supported in Compose YAML yet; use a JSON deployment manifest with an explicit rootfs")
	}
	if source.Healthcheck.Kind != 0 {
		return result, fmt.Errorf("Compose healthcheck commands are not supported; Boxr deployment healthchecks currently require explicit loopback TCP/HTTP probes in JSON")
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

	command, err := parseComposeCommand(&source.Command)
	if err != nil {
		return result, err
	}
	dependencies, err := parseComposeDependencies(&source.DependsOn)
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
	networkMode, networkName, err := resolveComposeServiceNetwork(source, networkNames, networks)
	if err != nil {
		return result, err
	}
	options := container.Options{
		NetworkMode:    networkMode,
		NetworkName:    networkName,
		PublishedPorts: ports,
		Workdir:        source.WorkingDir,
		Hostname:       source.Hostname,
		Env:            environment,
		Labels:         labels,
	}
	return Service{
		Image:     source.Image,
		Command:   command,
		Options:   options,
		DependsOn: dependencies,
		Profiles:  append([]string(nil), source.Profiles...),
		EnvFiles:  envFiles,
	}, nil
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

func parseComposeDependencies(node *yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	var result []string
	switch node.Kind {
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Value == "" {
				return nil, fmt.Errorf("depends_on must contain service names")
			}
			result = append(result, item.Value)
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			name, config := node.Content[index], node.Content[index+1]
			if name.Kind != yaml.ScalarNode || name.Value == "" || config.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("depends_on mapping values must use condition objects")
			}
			for field := 0; field+1 < len(config.Content); field += 2 {
				key, value := config.Content[field].Value, config.Content[field+1]
				switch key {
				case "condition":
					if value.Kind != yaml.ScalarNode || (value.Value != "service_started" && value.Value != "") {
						return nil, fmt.Errorf("depends_on condition %q is unsupported; only service_started is supported", value.Value)
					}
				case "required":
					if value.Kind != yaml.ScalarNode || value.Value != "true" {
						return nil, fmt.Errorf("optional dependencies are not supported")
					}
				case "restart":
					if value.Kind != yaml.ScalarNode || value.Value != "false" {
						return nil, fmt.Errorf("dependency restart propagation is not supported")
					}
				default:
					return nil, fmt.Errorf("depends_on option %q is not supported", key)
				}
			}
			result = append(result, name.Value)
		}
	default:
		return nil, fmt.Errorf("depends_on must be a service list or mapping")
	}
	return result, nil
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
