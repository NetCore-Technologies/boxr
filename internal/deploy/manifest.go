// Package deploy validates declarative, multi-service Boxr deployment manifests.
// It builds a deterministic dependency order without starting processes or
// changing host state. Execution is intentionally a separate operation.
package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/manit6752025/boxr/internal/container"
)

const (
	MaxManifestBytes = 1 << 20
	MaxServices      = 64
	MaxCommandArgs   = 128
	MaxCommandBytes  = 32 << 10
	StackLabel       = "io.boxr.deployment"
	ServiceLabel     = "io.boxr.service"
	OrderLabel       = "io.boxr.deployment-order"
)

var (
	stackNamePattern      = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,31}$")
	serviceNamePattern    = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,62}$")
	imageReferencePattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$")
)

// File is a versioned deployment manifest. Each service maps to one Boxr
// container configuration and may depend on other services in the same file.
type File struct {
	Version  int                `json:"version"`
	Name     string             `json:"name,omitempty"`
	Services map[string]Service `json:"services"`
}

// Service describes one process-isolated workload.
type Service struct {
	Image     string            `json:"image,omitempty"`
	Command   []string          `json:"command,omitempty"`
	Options   container.Options `json:"options"`
	DependsOn []string          `json:"dependsOn,omitempty"`
}

// ServicePlan contains only safe-to-display metadata. It deliberately excludes
// command contents, environment values, labels and host paths.
type ServicePlan struct {
	Name                  string   `json:"name"`
	ContainerName         string   `json:"containerName,omitempty"`
	Image                 string   `json:"image,omitempty"`
	DependsOn             []string `json:"dependsOn,omitempty"`
	NetworkMode           string   `json:"networkMode"`
	SecurityProfile       string   `json:"securityProfile,omitempty"`
	ResourceProfile       string   `json:"resourceProfile,omitempty"`
	RootFSConfigured      bool     `json:"rootfsConfigured"`
	ReadOnlyRootFS        bool     `json:"readOnlyRootfs"`
	MemoryLimitBytes      int64    `json:"memoryLimitBytes,omitempty"`
	CPUs                  float64  `json:"cpus,omitempty"`
	PidsLimit             int64    `json:"pidsLimit,omitempty"`
	CommandArgumentCount  int      `json:"commandArgumentCount"`
	EnvironmentEntryCount int      `json:"environmentEntryCount"`
	LabelCount            int      `json:"labelCount"`
}

// Plan is deterministic for the same deployment file. Services are listed in
// dependency-safe execution order; no process is started by building a plan.
type Plan struct {
	Version        int           `json:"version"`
	Name           string        `json:"name,omitempty"`
	ExecutionOrder []string      `json:"executionOrder"`
	StartGroups    [][]string    `json:"startGroups"`
	Services       []ServicePlan `json:"services"`
}

// Load reads and strictly decodes one bounded JSON deployment manifest.
func Load(path string) (File, error) {
	var result File
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("open deployment manifest: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return result, fmt.Errorf("read deployment manifest: %w", err)
	}
	if len(data) > MaxManifestBytes {
		return result, fmt.Errorf("deployment manifest exceeds %d bytes", MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode deployment manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, fmt.Errorf("deployment manifest must contain exactly one JSON value")
	}
	return result, nil
}

// BuildPlan validates every service and computes a stable topological ordering.
// The returned plan is safe to print: secrets and command arguments are omitted.
func BuildPlan(file File) (Plan, error) {
	plan := Plan{Version: file.Version, Name: file.Name}
	if file.Version != 1 {
		return plan, fmt.Errorf("unsupported deployment version %d (supported: 1)", file.Version)
	}
	if file.Name != "" && !stackNamePattern.MatchString(file.Name) {
		return plan, fmt.Errorf("deployment name must start with a lowercase letter or digit and contain only lowercase letters, digits, '-' or '_' (maximum 32 characters)")
	}
	if len(file.Services) == 0 {
		return plan, fmt.Errorf("deployment must define at least one service")
	}
	if len(file.Services) > MaxServices {
		return plan, fmt.Errorf("deployment has %d services; maximum is %d", len(file.Services), MaxServices)
	}

	names := make([]string, 0, len(file.Services))
	for name := range file.Services {
		if !serviceNamePattern.MatchString(name) {
			return plan, fmt.Errorf("invalid service name %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	dependencies := make(map[string][]string, len(names))
	dependents := make(map[string][]string, len(names))
	indegree := make(map[string]int, len(names))
	configuredNames := make(map[string]string)
	servicePlans := make(map[string]ServicePlan, len(names))

	for _, name := range names {
		service := file.Services[name]
		if service.Image == "" {
			if len(service.Command) == 0 {
				return plan, fmt.Errorf("service %q must define an image or a command", name)
			}
		} else {
			if !imageReferencePattern.MatchString(service.Image) {
				return plan, fmt.Errorf("service %q has an invalid image reference", name)
			}
			if service.Options.RootFS != "" {
				return plan, fmt.Errorf("service %q cannot combine an image with an explicit rootfs", name)
			}
		}
		if len(service.Command) > MaxCommandArgs {
			return plan, fmt.Errorf("service %q command has more than %d arguments", name, MaxCommandArgs)
		}
		commandBytes := 0
		for index, arg := range service.Command {
			if arg == "" || strings.ContainsRune(arg, '\x00') {
				return plan, fmt.Errorf("service %q command argument %d is empty or contains NUL", name, index)
			}
			commandBytes += len(arg)
			if commandBytes > MaxCommandBytes {
				return plan, fmt.Errorf("service %q command exceeds %d bytes", name, MaxCommandBytes)
			}
		}
		for key := range service.Options.Labels {
			if key == StackLabel || key == ServiceLabel || key == OrderLabel {
				return plan, fmt.Errorf("service %q uses reserved Boxr deployment label %q", name, key)
			}
		}
		if err := service.Options.Validate(); err != nil {
			return plan, fmt.Errorf("service %q has invalid container options: %w", name, err)
		}
		if configuredName := service.Options.Name; configuredName != "" {
			if previous, exists := configuredNames[configuredName]; exists {
				return plan, fmt.Errorf("services %q and %q use the same container name %q", previous, name, configuredName)
			}
			configuredNames[configuredName] = name
		}

		deps := append([]string(nil), service.DependsOn...)
		if len(deps) > MaxServices {
			return plan, fmt.Errorf("service %q has more than %d dependencies", name, MaxServices)
		}
		sort.Strings(deps)
		for index, dependency := range deps {
			if !serviceNamePattern.MatchString(dependency) {
				return plan, fmt.Errorf("service %q has invalid dependency name %q", name, dependency)
			}
			if dependency == name {
				return plan, fmt.Errorf("service %q cannot depend on itself", name)
			}
			if index > 0 && deps[index-1] == dependency {
				return plan, fmt.Errorf("service %q lists dependency %q more than once", name, dependency)
			}
		}
		dependencies[name] = deps
		indegree[name] = len(deps)

		networkMode := service.Options.NetworkMode
		if networkMode == "" {
			networkMode = "host"
		}
		containerName := ""
		if file.Name != "" {
			containerName = ContainerName(file.Name, name)
		}
		servicePlans[name] = ServicePlan{
			Name:                  name,
			ContainerName:         containerName,
			Image:                 service.Image,
			DependsOn:             deps,
			NetworkMode:           networkMode,
			SecurityProfile:       service.Options.SecurityProfile,
			ResourceProfile:       service.Options.ResourceProfile,
			RootFSConfigured:      service.Options.RootFS != "",
			ReadOnlyRootFS:        service.Options.ReadOnlyRootFS,
			MemoryLimitBytes:      service.Options.MemoryLimitBytes,
			CPUs:                  service.Options.CPUs,
			PidsLimit:             service.Options.PidsLimit,
			CommandArgumentCount:  len(service.Command),
			EnvironmentEntryCount: len(service.Options.Env),
			LabelCount:            len(service.Options.Labels),
		}
	}

	for _, name := range names {
		for _, dependency := range dependencies[name] {
			if _, exists := file.Services[dependency]; !exists {
				return plan, fmt.Errorf("service %q depends on unknown service %q", name, dependency)
			}
			dependents[dependency] = append(dependents[dependency], name)
		}
	}
	for name := range dependents {
		sort.Strings(dependents[name])
	}

	ready := make([]string, 0, len(names))
	for _, name := range names {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	order := make([]string, 0, len(names))
	startGroups := make([][]string, 0, len(names))
	for len(ready) > 0 {
		current := append([]string(nil), ready...)
		startGroups = append(startGroups, current)
		ready = ready[:0]
		for _, name := range current {
			order = append(order, name)
			for _, dependent := range dependents[name] {
				indegree[dependent]--
				if indegree[dependent] == 0 {
					ready = append(ready, dependent)
				}
			}
		}
		sort.Strings(ready)
	}
	if len(order) != len(names) {
		blocked := make([]string, 0, len(names)-len(order))
		for _, name := range names {
			if indegree[name] > 0 {
				blocked = append(blocked, name)
			}
		}
		return plan, fmt.Errorf("deployment dependency cycle detected among services: %s", strings.Join(blocked, ", "))
	}

	plan.ExecutionOrder = order
	plan.StartGroups = startGroups
	plan.Services = make([]ServicePlan, 0, len(order))
	for _, name := range order {
		plan.Services = append(plan.Services, servicePlans[name])
	}
	return plan, nil
}

// ContainerName returns the stable, stack-scoped Boxr container name for a
// service. Long combinations receive a deterministic hash suffix and remain
// within the runtime's 64-character container-name limit.
func ContainerName(stack, service string) string {
	full := stack + "-" + service
	if len(full) <= 64 {
		return full
	}
	digest := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(digest[:8])
	return full[:47] + "-" + suffix
}

// Example returns a valid two-service deployment manifest.
func Example() string {
	return `{
  "version": 1,
  "name": "sample-app",
  "services": {
    "database": {
      "command": ["/usr/bin/database", "--foreground"],
      "options": {
        "rootfs": "/var/lib/boxr/rootfs/database",
        "readOnlyRootfs": true,
        "networkMode": "host",
        "securityProfile": "baseline",
        "memoryLimitBytes": 536870912,
        "pidsLimit": 128
      }
    },
    "web": {
      "command": ["/usr/bin/web", "--listen", "0.0.0.0:8080"],
      "options": {
        "rootfs": "/var/lib/boxr/rootfs/web",
        "networkMode": "host",
        "securityProfile": "baseline",
        "memoryLimitBytes": 268435456,
        "pidsLimit": 64
      },
      "dependsOn": ["database"]
    }
  }
}
`
}
