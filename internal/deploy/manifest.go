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
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/manit6752025/boxr/internal/container"
	volumeStore "github.com/manit6752025/boxr/internal/volume"
)

const (
	MaxManifestBytes = 1 << 20
	MaxServices      = 64
	MaxCommandArgs   = 128
	MaxCommandBytes  = 32 << 10
	StackLabel       = "io.boxr.deployment"
	ServiceLabel     = "io.boxr.service"
	OrderLabel       = "io.boxr.deployment-order"
	StopTimeoutLabel = "io.boxr.deployment-stop-timeout-ms"
)

var (
	stackNamePattern      = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,31}$")
	serviceNamePattern    = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,62}$")
	imageReferencePattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$")
	profileNamePattern    = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,62}$")
)

// File is a versioned deployment manifest. Each service maps to one Boxr
// container configuration and may depend on other services in the same file.
type File struct {
	Version  int                `json:"version"`
	Name     string             `json:"name,omitempty"`
	Services map[string]Service `json:"services"`
}

// Service describes one process-isolated workload.
type VolumeMount struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

type Service struct {
	Image              string            `json:"image,omitempty"`
	Command            []string          `json:"command,omitempty"`
	Options            container.Options `json:"options"`
	DependsOn          []string          `json:"dependsOn,omitempty"`
	DependsOnHealthy   []string          `json:"dependsOnHealthy,omitempty"`
	DependsOnCompleted []string          `json:"dependsOnCompleted,omitempty"`
	Volumes            []VolumeMount     `json:"volumes,omitempty"`
	Profiles           []string          `json:"profiles,omitempty"`
	EnvFiles           []string          `json:"envFiles,omitempty"`
	StartTimeoutMS     int               `json:"startTimeoutMs,omitempty"`
	StopTimeoutMS      int               `json:"stopTimeoutMs,omitempty"`
	Readiness          *ReadinessProbe   `json:"readiness,omitempty"`
	Healthcheck        *Healthcheck      `json:"healthcheck,omitempty"`
}

// ServicePlan contains only safe-to-display metadata. It deliberately excludes
// command contents, environment values, labels and host paths.
type ServicePlan struct {
	Name                     string                  `json:"name"`
	ContainerName            string                  `json:"containerName,omitempty"`
	Image                    string                  `json:"image,omitempty"`
	DependsOn                []string                `json:"dependsOn,omitempty"`
	DependsOnHealthy         []string                `json:"dependsOnHealthy,omitempty"`
	DependsOnCompleted       []string                `json:"dependsOnCompleted,omitempty"`
	Profiles                 []string                `json:"profiles,omitempty"`
	ReadinessType            string                  `json:"readinessType,omitempty"`
	Volumes                  []VolumeMount           `json:"volumes,omitempty"`
	PublishedPorts           []container.PortMapping `json:"publishedPorts,omitempty"`
	NetworkMode              string                  `json:"networkMode"`
	SecurityProfile          string                  `json:"securityProfile,omitempty"`
	ResourceProfile          string                  `json:"resourceProfile,omitempty"`
	RootFSConfigured         bool                    `json:"rootfsConfigured"`
	ReadOnlyRootFS           bool                    `json:"readOnlyRootfs"`
	MemoryLimitBytes         int64                   `json:"memoryLimitBytes,omitempty"`
	CPUs                     float64                 `json:"cpus,omitempty"`
	PidsLimit                int64                   `json:"pidsLimit,omitempty"`
	CommandArgumentCount     int                     `json:"commandArgumentCount"`
	EnvironmentEntryCount    int                     `json:"environmentEntryCount"`
	EnvironmentFileCount     int                     `json:"environmentFileCount"`
	StartTimeoutMS           int                     `json:"startTimeoutMs,omitempty"`
	StopTimeoutMS            int                     `json:"stopTimeoutMs,omitempty"`
	HealthcheckConfigured    bool                    `json:"healthcheckConfigured,omitempty"`
	HealthcheckType          string                  `json:"healthcheckType,omitempty"`
	HealthcheckIntervalMS    int                     `json:"healthcheckIntervalMs,omitempty"`
	HealthcheckTimeoutMS     int                     `json:"healthcheckTimeoutMs,omitempty"`
	HealthcheckRetries       int                     `json:"healthcheckRetries,omitempty"`
	HealthcheckStartPeriodMS int                     `json:"healthcheckStartPeriodMs,omitempty"`
	LabelCount               int                     `json:"labelCount"`
}

// Plan is deterministic for the same deployment file. Services are listed in
// dependency-safe execution order; no process is started by building a plan.
type Plan struct {
	Version        int           `json:"version"`
	Name           string        `json:"name,omitempty"`
	ExecutionOrder []string      `json:"executionOrder"`
	StartGroups    [][]string    `json:"startGroups"`
	Profiles       []string      `json:"profiles,omitempty"`
	Services       []ServicePlan `json:"services"`
}

// ProfileGroup describes the services enabled by one opt-in profile.
type ProfileGroup struct {
	Name     string   `json:"name"`
	Services []string `json:"services"`
}

// ProfileInventory separates always-enabled services from opt-in groups.
type ProfileInventory struct {
	DefaultServices []string       `json:"defaultServices"`
	Profiles        []ProfileGroup `json:"profiles"`
}

// ListProfiles returns a deterministic, secret-free inventory of all service
// profiles. Full manifest validation remains the caller's responsibility.
func ListProfiles(file File) ProfileInventory {
	groups := make(map[string][]string)
	inventory := ProfileInventory{
		DefaultServices: make([]string, 0),
		Profiles:        make([]ProfileGroup, 0),
	}
	for serviceName, service := range file.Services {
		if len(service.Profiles) == 0 {
			inventory.DefaultServices = append(inventory.DefaultServices, serviceName)
			continue
		}
		for _, profile := range service.Profiles {
			groups[profile] = append(groups[profile], serviceName)
		}
	}
	sort.Strings(inventory.DefaultServices)
	profileNames := make([]string, 0, len(groups))
	for name := range groups {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, name := range profileNames {
		services := groups[name]
		sort.Strings(services)
		inventory.Profiles = append(inventory.Profiles, ProfileGroup{
			Name:     name,
			Services: services,
		})
	}
	return inventory
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
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return result, fmt.Errorf("deployment manifest is empty")
	}
	extension := strings.ToLower(filepath.Ext(path))
	if extension == ".yaml" || extension == ".yml" || trimmed[0] != '{' {
		return decodeCompose(data, path)
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
// BuildPlan validates the complete manifest and plans only services that do
// not opt into a profile. Profile-bound services are inactive until selected.
func normalizePublishedPorts(mappings []container.PortMapping) []container.PortMapping {
	result := make([]container.PortMapping, 0, len(mappings))
	for _, mapping := range mappings {
		normalized, err := mapping.Normalize()
		if err == nil {
			result = append(result, normalized)
		}
	}
	return result
}

func BuildPlan(file File) (Plan, error) {
	return BuildPlanWithProfiles(file, nil)
}

// BuildPlanWithProfiles validates every service before selecting the active
// profile set. Services without profiles are always included; a profiled
// service is included when any of its profiles is selected. Dependencies on
// omitted services are rejected by the final graph validation.
func BuildPlanWithProfiles(file File, selectedProfiles []string) (Plan, error) {
	if _, err := buildPlan(file); err != nil {
		return Plan{}, err
	}

	selected := make(map[string]struct{}, len(selectedProfiles))
	for _, profile := range selectedProfiles {
		if !profileNamePattern.MatchString(profile) {
			return Plan{}, fmt.Errorf("invalid profile name %q", profile)
		}
		if _, exists := selected[profile]; exists {
			return Plan{}, fmt.Errorf("profile %q was selected more than once", profile)
		}
		selected[profile] = struct{}{}
	}

	available := make(map[string]struct{})
	for _, service := range file.Services {
		for _, profile := range service.Profiles {
			available[profile] = struct{}{}
		}
	}
	for profile := range selected {
		if _, exists := available[profile]; !exists {
			return Plan{}, fmt.Errorf("unknown deployment profile %q", profile)
		}
	}

	filtered := File{Version: file.Version, Name: file.Name, Services: make(map[string]Service)}
	for name, service := range file.Services {
		active := len(service.Profiles) == 0
		if !active {
			for _, profile := range service.Profiles {
				if _, exists := selected[profile]; exists {
					active = true
					break
				}
			}
		}
		if active {
			filtered.Services[name] = service
		}
	}
	if len(filtered.Services) == 0 {
		return Plan{}, fmt.Errorf("deployment has no active services; select a profile with --profile")
	}

	plan, err := buildPlan(filtered)
	if err != nil {
		return plan, err
	}
	if len(selected) > 0 {
		plan.Profiles = make([]string, 0, len(selected))
		for profile := range selected {
			plan.Profiles = append(plan.Profiles, profile)
		}
		sort.Strings(plan.Profiles)
	}
	return plan, nil
}

func buildPlan(file File) (Plan, error) {
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
		seenProfiles := make(map[string]struct{}, len(service.Profiles))
		for _, profile := range service.Profiles {
			if !profileNamePattern.MatchString(profile) {
				return plan, fmt.Errorf("service %q has invalid profile name %q", name, profile)
			}
			if _, exists := seenProfiles[profile]; exists {
				return plan, fmt.Errorf("service %q repeats profile %q", name, profile)
			}
			seenProfiles[profile] = struct{}{}
		}
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
		if service.StartTimeoutMS < 0 || service.StartTimeoutMS > 600000 || (service.StartTimeoutMS > 0 && service.StartTimeoutMS < 10) {
			return plan, fmt.Errorf("service %q startTimeoutMs must be 0 (default) or between 10 and 600000", name)
		}
		if service.StopTimeoutMS < 0 || service.StopTimeoutMS > 600000 || (service.StopTimeoutMS > 0 && service.StopTimeoutMS < 10) {
			return plan, fmt.Errorf("service %q stopTimeoutMs must be 0 (default) or between 10 and 600000", name)
		}
		if len(service.EnvFiles) > 16 {
			return plan, fmt.Errorf("service %q has more than 16 environment files", name)
		}
		seenEnvFiles := make(map[string]struct{}, len(service.EnvFiles))
		for index, path := range service.EnvFiles {
			if path == "" || strings.TrimSpace(path) != path || len(path) > 4096 || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return plan, fmt.Errorf("service %q environment file %d must be a clean absolute path", name, index+1)
			}
			for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
				if segment == ".." {
					return plan, fmt.Errorf("service %q environment file %d cannot contain '..'", name, index+1)
				}
			}
			if _, exists := seenEnvFiles[path]; exists {
				return plan, fmt.Errorf("service %q repeats environment file %q", name, path)
			}
			seenEnvFiles[path] = struct{}{}
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
			if key == StackLabel || key == ServiceLabel || key == OrderLabel || key == StopTimeoutLabel {
				return plan, fmt.Errorf("service %q uses reserved Boxr deployment label %q", name, key)
			}
		}
		if len(service.Volumes) > 32 {
			return plan, fmt.Errorf("service %q has more than 32 named volume mounts", name)
		}
		if len(service.Volumes) > 0 && service.Options.RootFS == "" && service.Image == "" {
			return plan, fmt.Errorf("service %q must configure a rootfs or image to mount named volumes", name)
		}
		volumeTargets := make(map[string]string, len(service.Options.BindMounts)+len(service.Options.TmpfsMounts)+len(service.Volumes))
		for _, mount := range service.Options.BindMounts {
			volumeTargets[filepath.Clean(mount.Target)] = "bind mount"
		}
		for _, mount := range service.Options.TmpfsMounts {
			target := filepath.Clean(mount.Target)
			if previous, exists := volumeTargets[target]; exists {
				return plan, fmt.Errorf("service %q has conflicting %s and tmpfs mount targets at %s", name, previous, target)
			}
			volumeTargets[target] = "tmpfs mount"
		}
		for index, mount := range service.Volumes {
			if err := volumeStore.ValidateName(mount.Name); err != nil {
				return plan, fmt.Errorf("service %q volume mount %d has an invalid volume name: %w", name, index+1, err)
			}
			if !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) == "/" || strings.ContainsRune(mount.Target, '\\') {
				return plan, fmt.Errorf("service %q volume mount %d target must be an absolute container path other than /", name, index+1)
			}
			for _, segment := range strings.Split(filepath.ToSlash(mount.Target), "/") {
				if segment == ".." {
					return plan, fmt.Errorf("service %q volume mount %d target cannot contain '..'", name, index+1)
				}
			}
			target := filepath.Clean(mount.Target)
			if previous, exists := volumeTargets[target]; exists {
				return plan, fmt.Errorf("service %q has conflicting %s and named volume targets at %s", name, previous, target)
			}
			volumeTargets[target] = "named volume"
		}
		validationOptions := service.Options
		if service.Image != "" && validationOptions.RootFS == "" {
			// Image services receive their private unpacked rootfs immediately
			// before boxr run validates the final mount configuration.
			validationOptions.RootFS = "/boxr-image-rootfs-validation"
		}
		if err := validationOptions.Validate(); err != nil {
			return plan, fmt.Errorf("service %q has invalid container options: %w", name, err)
		}
		readinessType := ""
		if service.Readiness != nil {
			if err := service.Readiness.Validate(); err != nil {
				return plan, fmt.Errorf("service %q has invalid readiness probe: %w", name, err)
			}
			securityProfile := strings.ToLower(strings.TrimSpace(service.Options.SecurityProfile))
			if service.Options.NetworkMode == "none" || securityProfile == "restricted" || securityProfile == "untrusted" {
				return plan, fmt.Errorf("service %q must use a host-networking security profile for a loopback readiness probe", name)
			}
			readinessType = service.Readiness.Type()
		}
		healthcheckType := ""
		if service.Healthcheck != nil {
			if err := service.Healthcheck.Validate(); err != nil {
				return plan, fmt.Errorf("service %q has invalid healthcheck: %w", name, err)
			}
			securityProfile := strings.ToLower(strings.TrimSpace(service.Options.SecurityProfile))
			if service.Options.NetworkMode == "none" || securityProfile == "restricted" || securityProfile == "untrusted" {
				return plan, fmt.Errorf("service %q must use a host-networking security profile for a loopback healthcheck", name)
			}
			healthcheckType = service.Healthcheck.Probe.Type()
		}
		healthcheckIntervalMS := 0
		healthcheckTimeoutMS := 0
		healthcheckRetries := 0
		healthcheckStartPeriodMS := 0
		if service.Healthcheck != nil {
			healthcheckIntervalMS = service.Healthcheck.IntervalMS
			healthcheckTimeoutMS = service.Healthcheck.Probe.TimeoutMS
			healthcheckRetries = service.Healthcheck.Retries
			healthcheckStartPeriodMS = service.Healthcheck.StartPeriodMS
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
		dependencySet := make(map[string]struct{}, len(deps))
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
			dependencySet[dependency] = struct{}{}
		}
		healthyDeps := append([]string(nil), service.DependsOnHealthy...)
		if len(healthyDeps) > MaxServices {
			return plan, fmt.Errorf("service %q has more than %d healthy dependencies", name, MaxServices)
		}
		sort.Strings(healthyDeps)
		for index, dependency := range healthyDeps {
			if !serviceNamePattern.MatchString(dependency) {
				return plan, fmt.Errorf("service %q has invalid healthy dependency name %q", name, dependency)
			}
			if dependency == name {
				return plan, fmt.Errorf("service %q cannot wait for itself to become healthy", name)
			}
			if index > 0 && healthyDeps[index-1] == dependency {
				return plan, fmt.Errorf("service %q repeats healthy dependency %q", name, dependency)
			}
			if _, declared := dependencySet[dependency]; !declared {
				return plan, fmt.Errorf("service %q marks %q as healthy without declaring it in dependsOn", name, dependency)
			}
		}
		completedDeps := append([]string(nil), service.DependsOnCompleted...)
		if len(completedDeps) > MaxServices {
			return plan, fmt.Errorf("service %q has more than %d completed dependencies", name, MaxServices)
		}
		sort.Strings(completedDeps)
		for index, dependency := range completedDeps {
			if !serviceNamePattern.MatchString(dependency) {
				return plan, fmt.Errorf("service %q has invalid completed dependency name %q", name, dependency)
			}
			if dependency == name {
				return plan, fmt.Errorf("service %q cannot wait for itself to complete", name)
			}
			if index > 0 && completedDeps[index-1] == dependency {
				return plan, fmt.Errorf("service %q repeats completed dependency %q", name, dependency)
			}
			if _, declared := dependencySet[dependency]; !declared {
				return plan, fmt.Errorf("service %q marks %q as completed without declaring it in dependsOn", name, dependency)
			}
			if slices.Contains(healthyDeps, dependency) {
				return plan, fmt.Errorf("service %q cannot require %q to be both healthy and completed", name, dependency)
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
			Name:                     name,
			ContainerName:            containerName,
			Image:                    service.Image,
			DependsOn:                deps,
			DependsOnHealthy:         healthyDeps,
			DependsOnCompleted:       completedDeps,
			Profiles:                 append([]string(nil), service.Profiles...),
			ReadinessType:            readinessType,
			Volumes:                  append([]VolumeMount(nil), service.Volumes...),
			PublishedPorts:           normalizePublishedPorts(service.Options.PublishedPorts),
			NetworkMode:              networkMode,
			SecurityProfile:          service.Options.SecurityProfile,
			ResourceProfile:          service.Options.ResourceProfile,
			RootFSConfigured:         service.Options.RootFS != "",
			ReadOnlyRootFS:           service.Options.ReadOnlyRootFS,
			MemoryLimitBytes:         service.Options.MemoryLimitBytes,
			CPUs:                     service.Options.CPUs,
			PidsLimit:                service.Options.PidsLimit,
			CommandArgumentCount:     len(service.Command),
			EnvironmentEntryCount:    len(service.Options.Env),
			EnvironmentFileCount:     len(service.EnvFiles),
			StartTimeoutMS:           service.StartTimeoutMS,
			StopTimeoutMS:            service.StopTimeoutMS,
			HealthcheckConfigured:    service.Healthcheck != nil,
			HealthcheckType:          healthcheckType,
			HealthcheckIntervalMS:    healthcheckIntervalMS,
			HealthcheckTimeoutMS:     healthcheckTimeoutMS,
			HealthcheckRetries:       healthcheckRetries,
			HealthcheckStartPeriodMS: healthcheckStartPeriodMS,
			LabelCount:               len(service.Options.Labels),
		}
	}

	for _, name := range names {
		for _, dependency := range dependencies[name] {
			if _, exists := file.Services[dependency]; !exists {
				return plan, fmt.Errorf("service %q depends on unknown service %q", name, dependency)
			}
			dependents[dependency] = append(dependents[dependency], name)
		}
		for _, dependency := range file.Services[name].DependsOnHealthy {
			dependencyService := file.Services[dependency]
			if !container.HealthcheckEnabled(dependencyService.Options.Healthcheck) {
				return plan, fmt.Errorf("service %q requires %q to become healthy, but that service has no enabled container healthcheck", name, dependency)
			}
		}
	}
	for name := range dependents {
		sort.Strings(dependents[name])
	}

	completedTargets := make(map[string]bool)
	for _, service := range file.Services {
		for _, dependency := range service.DependsOnCompleted {
			completedTargets[dependency] = true
		}
	}
	healthyTargets := make(map[string]bool)
	for _, service := range file.Services {
		for _, dependency := range service.DependsOnHealthy {
			healthyTargets[dependency] = true
		}
	}
	for serviceName := range completedTargets {
		if healthyTargets[serviceName] {
			return plan, fmt.Errorf("service %q cannot be both a healthy dependency and a successful-completion dependency", serviceName)
		}
	}
	for serviceName := range completedTargets {
		service := file.Services[serviceName]
		if service.Readiness != nil {
			return plan, fmt.Errorf("service %q is used as a successful-completion dependency and cannot declare a readiness probe", serviceName)
		}
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
		ready = ready[:0]
		completionReady := make([]string, 0, len(current))
		regularReady := make([]string, 0, len(current))
		for _, name := range current {
			if completedTargets[name] {
				completionReady = append(completionReady, name)
			} else {
				regularReady = append(regularReady, name)
			}
		}
		// Completion-gated jobs run in isolated waves so their normal process
		// exit can be observed and recorded before any later wave starts.
		for _, name := range completionReady {
			startGroups = append(startGroups, []string{name})
		}
		if len(regularReady) > 0 {
			startGroups = append(startGroups, regularReady)
		}
		orderedCurrent := append(completionReady, regularReady...)
		for _, name := range orderedCurrent {
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
      "readiness": {
        "tcpAddress": "127.0.0.1:8080",
        "intervalMs": 250,
        "timeoutMs": 500,
        "attempts": 40
      },
      "dependsOn": ["database"]
    }
  }
}
`
}
