// Package network manages local Linux bridge networks for Boxr containers.
// It uses the host iproute2 and iptables tools; creating or attaching networks
// requires the privileges needed to manage links, network namespaces and rules.
package network

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/container"
)

const (
	storeVersion    = 1
	maxStoreBytes   = 4 << 20
	maxNetworkCount = 128
)

var (
	storeMu          sync.Mutex
	networkNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,30}$`)
	auxAddressNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

// Network is the persistent identity and IPAM state of one bridge network.
// Allocations are keyed by Boxr container ID, never by user-controlled names.
type Network struct {
	ID                 string                   `json:"id"`
	Name               string                   `json:"name"`
	Driver             string                   `json:"driver"`
	Bridge             string                   `json:"bridge"`
	Subnet             string                   `json:"subnet"`
	IPRange            string                   `json:"ipRange,omitempty"`
	AuxiliaryAddresses map[string]string        `json:"auxiliaryAddresses,omitempty"`
	Gateway            string                   `json:"gateway"`
	NAT                bool                     `json:"nat"`
	CreatedAt          time.Time                `json:"createdAt"`
	Allocations        map[string]string        `json:"allocations,omitempty"`
	MACAddresses       map[string]string        `json:"macAddresses,omitempty"`
	EndpointIDs        map[string]string        `json:"endpointIDs,omitempty"`
	Published          map[string][]PortMapping `json:"published,omitempty"`
	Labels             map[string]string        `json:"labels,omitempty"`
}

// PortMapping publishes one host IPv4 socket to a container IPv4 TCP or UDP port.
// PortMapping is shared with container run configuration.
type PortMapping = container.PortMapping

// CreateOptions configures an IPv4 bridge and whether it receives outbound NAT.
type CreateOptions struct {
	Subnet             string
	IPRange            string
	AuxiliaryAddresses map[string]string
	Gateway            string
	Bridge             string
	NAT                bool
	Labels             map[string]string
}

// StaleAttachment is a managed network lease whose container is not in the
// caller's verified set of live Boxr namespace helpers.
type StaleAttachment struct {
	Network        string        `json:"network"`
	ContainerID    string        `json:"containerId"`
	Address        string        `json:"address"`
	PublishedPorts []PortMapping `json:"publishedPorts,omitempty"`
}

// PruneReport counts verified stale bridge attachments removed from the host.
type PruneReport struct {
	AttachmentsRemoved    int `json:"attachmentsRemoved"`
	AddressesReleased     int `json:"addressesReleased"`
	PublishedPortsRemoved int `json:"publishedPortsRemoved"`
}

// Summary is safe for CLI output; it omits per-container allocation identities.
type Summary struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Driver             string            `json:"driver"`
	Bridge             string            `json:"bridge"`
	Subnet             string            `json:"subnet"`
	IPRange            string            `json:"ipRange,omitempty"`
	AuxiliaryAddresses map[string]string `json:"auxiliaryAddresses,omitempty"`
	Gateway            string            `json:"gateway"`
	NAT                bool              `json:"nat"`
	ContainerCount     int               `json:"containerCount"`
	CreatedAt          time.Time         `json:"createdAt"`
	Labels             map[string]string `json:"labels,omitempty"`
}

type store struct {
	Version  int       `json:"version"`
	Networks []Network `json:"networks"`
}

// Runner is injectable so network orchestration can be tested without
// requiring root or changing the host network.
type Runner interface {
	Run(ctx context.Context, executable string, args ...string) error
}

// OutputRunner executes a host command and returns its combined output. Network
// cleanup uses it to verify Boxr-owned link aliases before deleting interfaces.
type OutputRunner interface {
	Output(ctx context.Context, executable string, args ...string) ([]byte, error)
}

type RunnerFunc func(ctx context.Context, executable string, args ...string) error

func (f RunnerFunc) Run(ctx context.Context, executable string, args ...string) error {
	return f(ctx, executable, args...)
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, executable string, args ...string) error {
	command := exec.CommandContext(ctx, executable, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return fmt.Errorf("%s: %w", detail, err)
		}
		return err
	}
	return nil
}

func (commandRunner) Output(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return output, fmt.Errorf("%s: %w", detail, err)
		}
		return output, err
	}
	return output, nil
}

// StoreDir resolves the private directory used for network/IPAM state.
func StoreDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("BOXR_NETWORK_DIR")); value != "" {
		return filepath.Abs(value)
	}
	if value := strings.TrimSpace(os.Getenv("BOXR_DATA_DIR")); value != "" {
		return filepath.Join(value, "networks"), nil
	}
	if value := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); value != "" {
		return filepath.Join(value, "boxr", "networks"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "boxr", "networks"), nil
}

func ensureStoreDir() (string, error) {
	dir, err := StoreDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create network store: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("network store must be a real directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("secure network store: %w", err)
	}
	return dir, nil
}

func storePath(dir string) string {
	return filepath.Join(dir, "networks.json")
}

func lockStore() (func(), error) {
	storeMu.Lock()
	dir, err := ensureStoreDir()
	if err != nil {
		storeMu.Unlock()
		return nil, err
	}
	releaseFileLock, err := acquireNetworkStoreFileLock(dir)
	if err != nil {
		storeMu.Unlock()
		return nil, err
	}
	return func() {
		releaseFileLock()
		storeMu.Unlock()
	}, nil
}

func loadStore() (store, string, error) {
	var result store
	dir, err := ensureStoreDir()
	if err != nil {
		return result, "", err
	}
	result.Version = storeVersion
	path := storePath(dir)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, dir, nil
	}
	if err != nil {
		return result, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return result, "", fmt.Errorf("network store file must be a regular file")
	}
	if info.Size() > maxStoreBytes {
		return result, "", fmt.Errorf("network store exceeds %d bytes", maxStoreBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result, "", err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, "", fmt.Errorf("decode network store: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return result, "", fmt.Errorf("network store must contain exactly one JSON value")
	}
	if result.Version != storeVersion {
		return result, "", fmt.Errorf("unsupported network store version %d", result.Version)
	}
	if len(result.Networks) > maxNetworkCount {
		return result, "", fmt.Errorf("network store exceeds %d networks", maxNetworkCount)
	}
	if err := validateStoredNetworks(result.Networks); err != nil {
		return result, "", err
	}
	return result, dir, nil
}

func validateStoredNetworks(networks []Network) error {
	for _, network := range networks {
		if err := validateLabels(network.Labels); err != nil {
			return fmt.Errorf("network store contains invalid labels for %q: %w", network.Name, err)
		}
	}
	names := make(map[string]struct{}, len(networks))
	ids := make(map[string]struct{}, len(networks))
	bridges := make(map[string]struct{}, len(networks))
	prefixes := make([]netip.Prefix, 0, len(networks))
	containerNetworks := make(map[string]string)
	for _, network := range networks {
		if !networkNameRE.MatchString(network.Name) || len(network.ID) != 16 {
			return fmt.Errorf("network store contains an invalid network identity")
		}
		if _, err := hex.DecodeString(network.ID); err != nil {
			return fmt.Errorf("network store contains an invalid network ID")
		}
		if _, exists := names[network.Name]; exists {
			return fmt.Errorf("network store contains duplicate network name %q", network.Name)
		}
		if _, exists := ids[network.ID]; exists {
			return fmt.Errorf("network store contains duplicate network ID")
		}
		if _, exists := bridges[network.Bridge]; exists || !validInterfaceName(network.Bridge) {
			return fmt.Errorf("network store contains an invalid or duplicate bridge interface")
		}
		if network.Driver != "bridge" {
			return fmt.Errorf("network store contains unsupported driver %q", network.Driver)
		}
		prefix, gateway, err := parseIPv4Network(network.Subnet, network.Gateway)
		if err != nil || prefix.String() != network.Subnet || gateway.String() != network.Gateway {
			return fmt.Errorf("network store contains invalid subnet or gateway for %q", network.Name)
		}
		allocationPool := prefix
		if network.IPRange != "" {
			pool, err := parseIPv4IPRange(prefix, network.IPRange)
			if err != nil || pool.String() != network.IPRange {
				return fmt.Errorf("network store contains an invalid IPAM allocation range for %q", network.Name)
			}
			allocationPool = pool
		}
		reservedAddresses, err := validateAuxiliaryAddresses(prefix, gateway, network.AuxiliaryAddresses)
		if err != nil {
			return fmt.Errorf("network store contains invalid auxiliary addresses for %q: %w", network.Name, err)
		}
		for _, previous := range prefixes {
			if prefixesOverlap(prefix, previous) {
				return fmt.Errorf("network store contains overlapping IPv4 subnets")
			}
		}
		prefixes = append(prefixes, prefix)
		allocated := make(map[string]struct{}, len(network.Allocations))
		allocatedMACs := make(map[string]string, len(network.MACAddresses))
		for containerID, addressText := range network.Allocations {
			if previousNetwork, exists := containerNetworks[containerID]; exists {
				return fmt.Errorf("network store attaches container %q to both %q and %q", containerID, previousNetwork, network.Name)
			}
			containerNetworks[containerID] = network.Name
			address, err := netip.ParseAddr(addressText)
			if containerID == "" || strings.ContainsAny(containerID, "/\\\x00") ||
				err != nil || !address.Is4() || !prefix.Contains(address) || !allocationPool.Contains(address) ||
				address == gateway || address == prefix.Addr() || address == broadcastAddress(prefix) ||
				address == allocationPool.Addr() || address == broadcastAddress(allocationPool) || reservedAddresses[addressText] != "" {
				return fmt.Errorf("network store contains an invalid IPAM allocation for %q", network.Name)
			}
			if _, exists := allocated[addressText]; exists {
				return fmt.Errorf("network store contains a duplicate IPAM address")
			}
			allocated[addressText] = struct{}{}
		}
		for containerID, mac := range network.MACAddresses {
			if _, exists := network.Allocations[containerID]; !exists {
				return fmt.Errorf("network store has a MAC address without an IPAM lease")
			}
			if err := validateEndpointMAC(mac); err != nil {
				return fmt.Errorf("network store contains an invalid endpoint MAC address: %w", err)
			}
			if previousContainer, exists := allocatedMACs[mac]; exists {
				return fmt.Errorf("network store assigns MAC %s to both %q and %q", mac, previousContainer, containerID)
			}
			allocatedMACs[mac] = containerID
			if network.EndpointIDs[containerID] == "" {
				return fmt.Errorf("network store has a MAC address without an endpoint ID")
			}
		}
		for containerID, endpointID := range network.EndpointIDs {
			if _, exists := network.Allocations[containerID]; !exists {
				return fmt.Errorf("network store has an endpoint ID without an IPAM lease")
			}
			decoded, err := hex.DecodeString(endpointID)
			if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != endpointID ||
				network.MACAddresses[containerID] == "" {
				return fmt.Errorf("network store contains an invalid endpoint identity")
			}
		}
		for containerID, mappings := range network.Published {
			if _, allocatedAddress := network.Allocations[containerID]; !allocatedAddress {
				return fmt.Errorf("network store has published ports without an IPAM lease")
			}
			if len(mappings) == 0 || len(mappings) > 128 {
				return fmt.Errorf("network store contains an invalid published port count")
			}
			normalized, err := normalizePortMappings(mappings)
			if err != nil || len(normalized) != len(mappings) {
				return fmt.Errorf("network store contains invalid published port mappings")
			}
			for index := range normalized {
				if normalized[index] != mappings[index] {
					return fmt.Errorf("network store contains non-canonical published port mappings")
				}

			}
		}
		names[network.Name] = struct{}{}
		ids[network.ID] = struct{}{}
		bridges[network.Bridge] = struct{}{}
	}
	return validateAllPublishedPortReservations(networks)
}

func validateLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return fmt.Errorf("at most 64 network labels are supported")
	}
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 1024 ||
			strings.ContainsAny(key+value, "\x00\r\n") || strings.Contains(key, "=") {
			return fmt.Errorf("invalid network label %q", key)
		}
	}
	return nil
}

func cloneNetworkLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func validateAllPublishedPortReservations(networks []Network) error {
	var seen []PortMapping
	for _, network := range networks {
		for _, mappings := range network.Published {
			for _, mapping := range mappings {
				for _, previous := range seen {
					if portMappingsConflict(previous, mapping) {
						return fmt.Errorf("network store contains conflicting published host ports")
					}
				}
				seen = append(seen, mapping)
			}
		}
	}
	return nil
}

func saveStore(dir string, value store) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode network store: %w", err)
	}
	if len(data) > maxStoreBytes {
		return fmt.Errorf("network store exceeds %d bytes", maxStoreBytes)
	}
	temporary, err := os.CreateTemp(dir, ".networks-*.tmp")
	if err != nil {
		return fmt.Errorf("create network store staging file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, storePath(dir)); err != nil {
		return fmt.Errorf("publish network store: %w", err)
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// Create provisions a bridge, its gateway address and optional IPv4 NAT.
func Create(name string, options CreateOptions) (Network, error) {
	return CreateWithRunner(context.Background(), name, options, commandRunner{})
}

// CreateWithRunner is Create with an injectable host command executor.
func CreateWithRunner(ctx context.Context, name string, options CreateOptions, runner Runner) (Network, error) {
	var empty Network
	if runner == nil {
		return empty, fmt.Errorf("network command runner is required")
	}
	if !networkNameRE.MatchString(name) {
		return empty, fmt.Errorf("network name must start with a lowercase letter or digit and contain only lowercase letters, digits, '.', '_' or '-' (maximum 31 characters)")
	}
	if err := validateLabels(options.Labels); err != nil {
		return empty, fmt.Errorf("network labels: %w", err)
	}
	prefix, gateway, err := parseIPv4Network(options.Subnet, options.Gateway)
	if err != nil {
		return empty, err
	}
	allocationRange := ""
	if options.IPRange != "" {
		pool, err := parseIPv4IPRange(prefix, options.IPRange)
		if err != nil {
			return empty, err
		}
		allocationRange = pool.String()
	}
	if _, err := validateAuxiliaryAddresses(prefix, gateway, options.AuxiliaryAddresses); err != nil {
		return empty, err
	}
	unlock, err := lockStore()
	if err != nil {
		return empty, err
	}
	defer unlock()
	current, dir, err := loadStore()
	if err != nil {
		return empty, err
	}
	for _, existing := range current.Networks {
		if existing.Name == name {
			return empty, fmt.Errorf("network %q already exists", name)
		}
		other, _ := netip.ParsePrefix(existing.Subnet)
		if prefixesOverlap(prefix, other) {
			return empty, fmt.Errorf("subnet %s overlaps existing network %q (%s)", prefix, existing.Name, existing.Subnet)
		}
	}
	if len(current.Networks) >= maxNetworkCount {
		return empty, fmt.Errorf("maximum of %d bridge networks reached", maxNetworkCount)
	}
	id, err := newID()
	if err != nil {
		return empty, err
	}
	bridge := options.Bridge
	if bridge == "" {
		bridge = "brx" + id[:8]
	}
	if !validInterfaceName(bridge) {
		return empty, fmt.Errorf("bridge name must be 1 to 15 characters and contain only letters, digits, '_' or '-'")
	}
	for _, existing := range current.Networks {
		if existing.Bridge == bridge {
			return empty, fmt.Errorf("bridge interface %q is already managed by network %q", bridge, existing.Name)
		}
	}
	network := Network{
		ID:                 id,
		Name:               name,
		Driver:             "bridge",
		Bridge:             bridge,
		Subnet:             prefix.String(),
		IPRange:            allocationRange,
		AuxiliaryAddresses: cloneNetworkLabels(options.AuxiliaryAddresses),
		Gateway:            gateway.String(),
		NAT:                options.NAT,
		CreatedAt:          time.Now().UTC(),
		Allocations:        make(map[string]string),
		Labels:             cloneNetworkLabels(options.Labels),
	}
	if err := provisionBridge(ctx, runner, network); err != nil {
		return empty, err
	}
	current.Networks = append(current.Networks, network)
	if err := saveStore(dir, current); err != nil {
		if network.NAT {
			_ = removeNAT(ctx, runner, network)
		}
		_ = removeBridge(ctx, runner, network)
		return empty, err
	}
	return network, nil
}

func provisionBridge(ctx context.Context, runner Runner, network Network) error {
	prefix, _ := netip.ParsePrefix(network.Subnet)
	gateway, _ := netip.ParseAddr(network.Gateway)
	if err := runner.Run(ctx, "ip", "link", "add", "name", network.Bridge, "type", "bridge"); err != nil {
		// The requested interface may already belong to the host. Never delete
		// it when the create operation itself failed.
		return fmt.Errorf("create bridge %q: %w", network.Bridge, err)
	}
	steps := [][]string{
		{"addr", "add", gateway.String() + "/" + fmt.Sprint(prefix.Bits()), "dev", network.Bridge},
		{"link", "set", "dev", network.Bridge, "up"},
	}
	for _, args := range steps {
		if err := runner.Run(ctx, "ip", args...); err != nil {
			_ = removeBridge(ctx, runner, network)
			return fmt.Errorf("provision bridge %q (ip %s): %w", network.Name, strings.Join(args, " "), err)
		}
	}
	if network.NAT {
		if err := provisionNAT(ctx, runner, network); err != nil {
			_ = removeNAT(ctx, runner, network)
			_ = removeBridge(ctx, runner, network)
			return fmt.Errorf("provision outbound NAT for network %q: %w", network.Name, err)
		}
	}
	return nil
}

func provisionNAT(ctx context.Context, runner Runner, network Network) error {
	filterChain, natChain := firewallChains(network.ID)
	steps := []struct {
		binary string
		args   []string
	}{
		{"sysctl", []string{"-w", "net.ipv4.ip_forward=1"}},
		{"iptables", []string{"-N", filterChain}},
		{"iptables", []string{"-A", filterChain, "-i", network.Bridge, "-o", network.Bridge, "-j", "ACCEPT"}},
		{"iptables", []string{"-A", filterChain, "-i", network.Bridge, "!", "-o", network.Bridge, "-j", "ACCEPT"}},
		{"iptables", []string{"-A", filterChain, "-o", network.Bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
		{"iptables", []string{"-I", "FORWARD", "1", "-j", filterChain}},
		{"iptables", []string{"-t", "nat", "-N", natChain}},
		{"iptables", []string{"-t", "nat", "-A", natChain, "-s", network.Subnet, "!", "-o", network.Bridge, "-j", "MASQUERADE"}},
		{"iptables", []string{"-t", "nat", "-I", "POSTROUTING", "1", "-j", natChain}},
	}
	for _, step := range steps {
		if err := runner.Run(ctx, step.binary, step.args...); err != nil {
			return fmt.Errorf("%s %s: %w", step.binary, strings.Join(step.args, " "), err)
		}
	}
	return nil
}

func removeNAT(ctx context.Context, runner Runner, network Network) error {
	filterChain, natChain := firewallChains(network.ID)
	steps := [][]string{
		{"-D", "FORWARD", "-j", filterChain},
		{"-F", filterChain},
		{"-X", filterChain},
		{"-t", "nat", "-D", "POSTROUTING", "-j", natChain},
		{"-t", "nat", "-F", natChain},
		{"-t", "nat", "-X", natChain},
	}
	var failures []string
	for _, args := range steps {
		if err := runner.Run(ctx, "iptables", args...); err != nil && !missingFirewallObject(err) {
			failures = append(failures, strings.Join(args, " ")+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("remove network firewall rules: %s", strings.Join(failures, "; "))
	}
	return nil
}

func removeBridge(ctx context.Context, runner Runner, network Network) error {
	if err := runner.Run(ctx, "ip", "link", "del", "dev", network.Bridge); err != nil && !missingNetworkInterface(err) {
		return fmt.Errorf("remove bridge %q: %w", network.Bridge, err)
	}
	return nil
}

func missingFirewallObject(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "bad rule") ||
		strings.Contains(message, "no chain/target/match") ||
		strings.Contains(message, "does a matching rule exist")
}

func missingNetworkInterface(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cannot find device") ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "no such device")
}

// Join allocates a stable IPv4 address and connects a paused container network
// namespace to the host bridge. PID is the host PID of Boxr's namespace helper.
// EndpointOptions selects deterministic per-container bridge endpoint settings.
// Empty values use Boxr's generated IPv4 address and locally administered MAC.
type EndpointOptions struct {
	IPv4Address string
	MACAddress  string
}

func Join(networkName string, pid int, containerID string, ports ...PortMapping) (string, error) {
	return JoinWithRunner(context.Background(), networkName, pid, containerID, commandRunner{}, ports...)
}

// JoinWithOptions attaches a container using optional static IPv4 and MAC settings.
func JoinWithOptions(networkName string, pid int, containerID string, endpoint EndpointOptions, ports ...PortMapping) (string, error) {
	return JoinWithRunnerOptions(context.Background(), networkName, pid, containerID, commandRunner{}, endpoint, ports...)
}

// JoinWithRunner is Join with an injectable host command executor.
func JoinWithRunner(ctx context.Context, networkName string, pid int, containerID string, runner Runner, ports ...PortMapping) (string, error) {
	return JoinWithRunnerOptions(ctx, networkName, pid, containerID, runner, EndpointOptions{}, ports...)
}

// JoinWithRunnerOptions is JoinWithOptions with an injectable host command executor.
func JoinWithRunnerOptions(ctx context.Context, networkName string, pid int, containerID string, runner Runner, endpoint EndpointOptions, ports ...PortMapping) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("network command runner is required")
	}
	if pid <= 1 || strings.TrimSpace(containerID) == "" || strings.ContainsAny(containerID, "/\\\x00") {
		return "", fmt.Errorf("valid host PID and container ID are required")
	}
	unlock, err := lockStore()
	if err != nil {
		return "", err
	}
	defer unlock()
	current, dir, err := loadStore()
	if err != nil {
		return "", err
	}
	index := findNetwork(current.Networks, networkName)
	if index < 0 {
		return "", fmt.Errorf("network %q does not exist", networkName)
	}
	network := &current.Networks[index]
	for _, existingNetwork := range current.Networks {
		if existingNetworkName := existingNetwork.Allocations[containerID]; existingNetworkName != "" {
			return "", fmt.Errorf("container %q is already attached to managed network %q; disconnect it first", containerID, existingNetwork.Name)
		}
	}
	normalizedPorts, err := normalizePortMappings(ports)
	if err != nil {
		return "", err
	}
	if err := validatePortMappingReservations(current.Networks, normalizedPorts); err != nil {
		return "", err
	}
	address, err := allocateEndpointAddress(*network, endpoint.IPv4Address)
	if err != nil {
		return "", err
	}
	endpointID, macAddress := endpointIdentity(network.ID, containerID)
	if endpoint.MACAddress != "" {
		if err := validateEndpointMAC(endpoint.MACAddress); err != nil {
			return "", fmt.Errorf("EndpointConfig MAC address: %w", err)
		}
		for existingID, existingMAC := range network.MACAddresses {
			if existingID != containerID && strings.EqualFold(existingMAC, endpoint.MACAddress) {
				return "", fmt.Errorf("MAC address %s is already assigned on network %q", endpoint.MACAddress, network.Name)
			}
		}
		macAddress = strings.ToLower(endpoint.MACAddress)
	}
	if network.Allocations == nil {
		network.Allocations = make(map[string]string)
	}
	network.Allocations[containerID] = address
	if network.MACAddresses == nil {
		network.MACAddresses = make(map[string]string)
	}
	if network.EndpointIDs == nil {
		network.EndpointIDs = make(map[string]string)
	}
	network.MACAddresses[containerID] = macAddress
	network.EndpointIDs[containerID] = endpointID
	if len(normalizedPorts) > 0 {
		if network.Published == nil {
			network.Published = make(map[string][]PortMapping)
		}
		network.Published[containerID] = normalizedPorts
	}
	if err := saveStore(dir, current); err != nil {
		delete(network.Allocations, containerID)
		delete(network.MACAddresses, containerID)
		delete(network.EndpointIDs, containerID)
		delete(network.Published, containerID)
		return "", err
	}
	hostVeth, peerVeth := vethNames(containerID)
	releaseAllocation := func() error {
		delete(network.Allocations, containerID)
		delete(network.MACAddresses, containerID)
		delete(network.EndpointIDs, containerID)
		delete(network.Published, containerID)
		return saveStore(dir, current)
	}
	installedPorts := make([]PortMapping, 0, len(normalizedPorts))
	rollbackCreatedVeth := func(cause error) (string, error) {
		var cleanupErrors []string
		for _, mapping := range installedPorts {
			if err := removePortMappingRules(ctx, runner, *network, address, mapping); err != nil {
				cleanupErrors = append(cleanupErrors, err.Error())
			}
		}
		if err := runner.Run(ctx, "ip", "link", "del", "dev", hostVeth); err != nil && !missingNetworkInterface(err) {
			cleanupErrors = append(cleanupErrors, err.Error())
		}
		if len(cleanupErrors) > 0 {
			// Keep the IP and published-port reservation if host cleanup is
			// incomplete, preventing another workload from reusing live rules.
			return "", fmt.Errorf("%v; bridge rollback incomplete: %s", cause, strings.Join(cleanupErrors, "; "))
		}
		if err := releaseAllocation(); err != nil {
			return "", fmt.Errorf("%v; release IPAM reservation: %w", cause, err)
		}
		return "", cause
	}
	steps := []struct {
		binary string
		args   []string
	}{
		{"ip", []string{"link", "add", hostVeth, "type", "veth", "peer", "name", peerVeth}},
		{"ip", []string{"link", "set", "dev", hostVeth, "alias", vethAlias(network.ID, containerID)}},
		{"ip", []string{"link", "set", hostVeth, "master", network.Bridge}},
		{"ip", []string{"link", "set", "dev", hostVeth, "up"}},
		{"ip", []string{"link", "set", peerVeth, "netns", fmt.Sprint(pid)}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "link", "set", "dev", peerVeth, "name", "eth0"}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "link", "set", "dev", "eth0", "address", macAddress}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "link", "set", "dev", "lo", "up"}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "addr", "add", address + "/" + prefixLength(network.Subnet), "dev", "eth0"}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "link", "set", "dev", "eth0", "up"}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "route", "add", "default", "via", network.Gateway, "dev", "eth0"}},
	}
	for index, step := range steps {
		if err := runner.Run(ctx, step.binary, step.args...); err != nil {
			wrapped := fmt.Errorf("connect container to network %q (%s %s): %w", networkName, step.binary, strings.Join(step.args, " "), err)
			if index == 0 {
				// A colliding veth name may refer to an unrelated host interface.
				// The failed create did not give Boxr ownership of that link.
				if cleanupErr := releaseAllocation(); cleanupErr != nil {
					return "", fmt.Errorf("%v; release IPAM reservation: %w", wrapped, cleanupErr)
				}
				return "", wrapped
			}
			return rollbackCreatedVeth(wrapped)
		}
	}
	for _, mapping := range normalizedPorts {
		partial, err := addPortMappingRules(ctx, runner, *network, address, mapping)
		if partial {
			installedPorts = append(installedPorts, mapping)
		}
		if err != nil {
			return rollbackCreatedVeth(fmt.Errorf("publish host port %s:%d/%s: %w", mapping.HostIP, mapping.HostPort, mapping.Protocol, err))
		}
	}
	return address, nil
}

func portMappingsConflict(left, right PortMapping) bool {
	if left.HostPort != right.HostPort || left.Protocol != right.Protocol {
		return false
	}
	return left.HostIP == "0.0.0.0" || right.HostIP == "0.0.0.0" || left.HostIP == right.HostIP
}

func normalizePortMappings(mappings []PortMapping) ([]PortMapping, error) {
	result := make([]PortMapping, 0, len(mappings))
	for index, mapping := range mappings {
		normalized, err := mapping.Normalize()
		if err != nil {
			return nil, fmt.Errorf("published port %d: %w", index+1, err)
		}
		for _, previous := range result {
			if portMappingsConflict(previous, normalized) {
				return nil, fmt.Errorf("published host port %s:%d/%s is configured more than once", normalized.HostIP, normalized.HostPort, normalized.Protocol)
			}
		}
		result = append(result, normalized)
	}
	return result, nil
}

func validatePortMappingReservations(networks []Network, requested []PortMapping) error {
	for _, mapping := range requested {
		for _, existingNetwork := range networks {
			for containerID, mappings := range existingNetwork.Published {
				for _, existing := range mappings {
					if portMappingsConflict(existing, mapping) {
						return fmt.Errorf("host port %s:%d/%s is already published by container %s on network %s", mapping.HostIP, mapping.HostPort, mapping.Protocol, containerID, existingNetwork.Name)
					}
				}
			}
		}
	}
	return nil
}

func portDNATArgs(mapping PortMapping, destination string) []string {
	args := []string{"-p", mapping.Protocol}
	if mapping.HostIP != "" && mapping.HostIP != "0.0.0.0" {
		args = append(args, "-d", mapping.HostIP)
	}
	return append(args, "--dport", fmt.Sprint(mapping.HostPort), "-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", destination, mapping.ContainerPort))
}

func portMappingFirewallRules(network Network, address string, mapping PortMapping) ([][]string, [][]string) {
	natRule := portDNATArgs(mapping, address)
	adds := [][]string{
		append([]string{"-t", "nat", "-A", "PREROUTING"}, natRule...),
		append([]string{"-t", "nat", "-A", "OUTPUT"}, natRule...),
		{"-I", "FORWARD", "1", "-o", network.Bridge, "-d", address, "-p", mapping.Protocol, "--dport", fmt.Sprint(mapping.ContainerPort), "-m", "conntrack", "--ctstate", "NEW", "-j", "ACCEPT"},
	}
	checks := [][]string{
		append([]string{"-t", "nat", "-C", "PREROUTING"}, natRule...),
		append([]string{"-t", "nat", "-C", "OUTPUT"}, natRule...),
		{"-C", "FORWARD", "-o", network.Bridge, "-d", address, "-p", mapping.Protocol, "--dport", fmt.Sprint(mapping.ContainerPort), "-m", "conntrack", "--ctstate", "NEW", "-j", "ACCEPT"},
	}
	return adds, checks
}

func addPortMappingRules(ctx context.Context, runner Runner, network Network, address string, mapping PortMapping) (bool, error) {
	if err := runner.Run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return false, fmt.Errorf("enable IPv4 forwarding for published ports: %w", err)
	}
	adds, checks := portMappingFirewallRules(network, address, mapping)
	applied := make([]int, 0, len(adds))
	rollback := func() (bool, error) {
		var failures []string
		for index := len(applied) - 1; index >= 0; index-- {
			deleteArgs := append([]string(nil), adds[applied[index]]...)
			if applied[index] < 2 {
				deleteArgs[2] = "-D"
			} else {
				deleteArgs[0] = "-D"
				deleteArgs = append(deleteArgs[:2], deleteArgs[3:]...)
			}
			if err := runner.Run(ctx, "iptables", deleteArgs...); err != nil && !missingFirewallObject(err) {
				failures = append(failures, strings.Join(deleteArgs, " ")+": "+err.Error())
			}
		}
		return len(failures) > 0, fmt.Errorf("rollback partial published port rules: %s", strings.Join(failures, "; "))
	}
	for index, args := range adds {
		if err := runner.Run(ctx, "iptables", checks[index]...); err == nil {
			partial, rollbackErr := rollback()
			if rollbackErr != nil && partial {
				return true, fmt.Errorf("published port rule already exists outside Boxr; %w", rollbackErr)
			}
			return false, fmt.Errorf("published port rule already exists outside Boxr")
		} else if !missingFirewallObject(err) {
			partial, rollbackErr := rollback()
			if rollbackErr != nil && partial {
				return true, fmt.Errorf("check published port rule: %v; %w", err, rollbackErr)
			}
			return false, fmt.Errorf("check published port rule: %w", err)
		}
		if err := runner.Run(ctx, "iptables", args...); err != nil {
			partial, rollbackErr := rollback()
			if rollbackErr != nil && partial {
				return true, fmt.Errorf("add published port rule: %v; %w", err, rollbackErr)
			}
			return false, fmt.Errorf("add published port rule: %w", err)
		}
		applied = append(applied, index)
	}
	return true, nil
}

func removePortMappingRules(ctx context.Context, runner Runner, network Network, address string, mapping PortMapping) error {
	var failures []string
	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		args := append([]string{"-t", "nat", "-D", chain}, portDNATArgs(mapping, address)...)
		if err := runner.Run(ctx, "iptables", args...); err != nil && !missingFirewallObject(err) {
			failures = append(failures, strings.Join(args, " ")+": "+err.Error())
		}
	}
	filterArgs := []string{"-D", "FORWARD", "-o", network.Bridge, "-d", address, "-p", mapping.Protocol, "--dport", fmt.Sprint(mapping.ContainerPort), "-m", "conntrack", "--ctstate", "NEW", "-j", "ACCEPT"}
	if err := runner.Run(ctx, "iptables", filterArgs...); err != nil && !missingFirewallObject(err) {
		failures = append(failures, strings.Join(filterArgs, " ")+": "+err.Error())
	}
	if len(failures) > 0 {
		return fmt.Errorf("remove published port firewall rules: %s", strings.Join(failures, "; "))
	}
	return nil
}

// Leave disconnects a container veth and releases its address back to IPAM.
func Leave(networkName, containerID string) error {
	return LeaveWithRunner(context.Background(), networkName, containerID, commandRunner{})
}

// LeaveWithRunner is Leave with an injectable host command executor.
func LeaveWithRunner(ctx context.Context, networkName, containerID string, runner Runner) error {
	if runner == nil {
		return fmt.Errorf("network command runner is required")
	}
	unlock, err := lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	current, dir, err := loadStore()
	if err != nil {
		return err
	}
	index := findNetwork(current.Networks, networkName)
	if index < 0 {
		return fmt.Errorf("network %q does not exist", networkName)
	}
	return leaveFromStore(ctx, runner, &current, dir, index, containerID)
}

// LeaveAll disconnects a container from every managed bridge network. It is
// intended for the runtime exit hook, including containers attached later with
// "boxr network connect".
func LeaveAll(containerID string) error {
	return LeaveAllWithRunner(context.Background(), containerID, commandRunner{})
}

// LeaveAllWithRunner is LeaveAll with an injectable host command executor.
func LeaveAllWithRunner(ctx context.Context, containerID string, runner Runner) error {
	if runner == nil {
		return fmt.Errorf("network command runner is required")
	}
	if strings.TrimSpace(containerID) == "" || strings.ContainsAny(containerID, "/\\\x00") {
		return fmt.Errorf("valid container ID is required")
	}
	unlock, err := lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	current, dir, err := loadStore()
	if err != nil {
		return err
	}
	for index := range current.Networks {
		if _, exists := current.Networks[index].Allocations[containerID]; exists {
			return leaveFromStore(ctx, runner, &current, dir, index, containerID)
		}
	}
	return nil
}

func leaveFromStore(ctx context.Context, runner Runner, current *store, dir string, index int, containerID string) error {
	network := &current.Networks[index]
	address, exists := network.Allocations[containerID]
	if !exists {
		return fmt.Errorf("container %q has no allocation on network %q", containerID, network.Name)
	}
	hostVeth, _ := vethNames(containerID)
	if err := verifyVethOwnership(ctx, runner, network.ID, containerID); err != nil {
		return fmt.Errorf("refusing to remove veth %s: %w", hostVeth, err)
	}
	mappings := append([]PortMapping(nil), network.Published[containerID]...)
	for _, mapping := range mappings {
		if err := removePortMappingRules(ctx, runner, *network, address, mapping); err != nil {
			return fmt.Errorf("disconnect published port %s:%d/%s: %w", mapping.HostIP, mapping.HostPort, mapping.Protocol, err)
		}
	}
	if err := runner.Run(ctx, "ip", "link", "del", "dev", hostVeth); err != nil && !missingNetworkInterface(err) {
		return fmt.Errorf("disconnect container from network %q: %w", network.Name, err)
	}
	delete(network.Allocations, containerID)
	delete(network.MACAddresses, containerID)
	delete(network.EndpointIDs, containerID)
	delete(network.Published, containerID)
	return saveStore(dir, *current)
}

// StaleAttachments returns network leases whose container IDs are absent from
// the supplied live set. It does not mutate network state or host interfaces.
func StaleAttachments(liveContainerIDs map[string]struct{}) ([]StaleAttachment, error) {
	unlock, err := lockStore()
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, _, err := loadStore()
	if err != nil {
		return nil, err
	}
	var result []StaleAttachment
	for _, network := range current.Networks {
		containerIDs := make([]string, 0, len(network.Allocations))
		for containerID := range network.Allocations {
			if _, live := liveContainerIDs[containerID]; !live {
				containerIDs = append(containerIDs, containerID)
			}
		}
		sort.Strings(containerIDs)
		for _, containerID := range containerIDs {
			result = append(result, StaleAttachment{
				Network: network.Name, ContainerID: containerID,
				Address:        network.Allocations[containerID],
				PublishedPorts: append([]PortMapping(nil), network.Published[containerID]...),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Network != result[j].Network {
			return result[i].Network < result[j].Network
		}
		return result[i].ContainerID < result[j].ContainerID
	})
	return result, nil
}

// PruneStale reclaims stale bridge attachments using the host iproute2 and
// firewall tools.
func PruneStale(liveContainerIDs map[string]struct{}) (PruneReport, error) {
	return PruneStaleWithRunner(context.Background(), liveContainerIDs, commandRunner{})
}

// PruneStaleWithRunner disconnects stale leases only after proving that any
// remaining host veth carries the Boxr ownership alias for that network/ID.
// A foreign or unmarked interface is left untouched and its lease is retained.
func PruneStaleWithRunner(ctx context.Context, liveContainerIDs map[string]struct{}, runner Runner) (PruneReport, error) {
	var report PruneReport
	if runner == nil {
		return report, fmt.Errorf("network command runner is required")
	}
	unlock, err := lockStore()
	if err != nil {
		return report, err
	}
	defer unlock()
	current, dir, err := loadStore()
	if err != nil {
		return report, err
	}
	var failures []string
	for index := range current.Networks {
		containerIDs := make([]string, 0, len(current.Networks[index].Allocations))
		for containerID := range current.Networks[index].Allocations {
			if _, live := liveContainerIDs[containerID]; !live {
				containerIDs = append(containerIDs, containerID)
			}
		}
		sort.Strings(containerIDs)
		for _, containerID := range containerIDs {
			ports := len(current.Networks[index].Published[containerID])
			if err := leaveFromStore(ctx, runner, &current, dir, index, containerID); err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: %v", current.Networks[index].Name, containerID, err))
				continue
			}
			report.AttachmentsRemoved++
			report.AddressesReleased++
			report.PublishedPortsRemoved += ports
		}
	}
	if len(failures) > 0 {
		return report, fmt.Errorf("some stale network attachments could not be safely removed: %s", strings.Join(failures, "; "))
	}
	return report, nil
}

// List returns networks sorted by name without exposing allocation identities.
func List() ([]Summary, error) {
	unlock, err := lockStore()
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, _, err := loadStore()
	if err != nil {
		return nil, err
	}
	result := make([]Summary, 0, len(current.Networks))
	for _, network := range current.Networks {
		result = append(result, summarize(network))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// Inspect returns the public metadata of one network.
func Inspect(name string) (Summary, error) {
	unlock, err := lockStore()
	if err != nil {
		return Summary{}, err
	}
	defer unlock()
	current, _, err := loadStore()
	if err != nil {
		return Summary{}, err
	}
	index := findNetwork(current.Networks, name)
	if index < 0 {
		return Summary{}, fmt.Errorf("network %q does not exist", name)
	}
	return summarize(current.Networks[index]), nil
}

// InspectDetails returns an owner-local copy of a network including IPAM leases.
// Callers receive detached maps so they cannot mutate the persisted network store.
func InspectDetails(name string) (Network, error) {
	unlock, err := lockStore()
	if err != nil {
		return Network{}, err
	}
	defer unlock()
	current, _, err := loadStore()
	if err != nil {
		return Network{}, err
	}
	index := findNetwork(current.Networks, name)
	if index < 0 {
		return Network{}, fmt.Errorf("network %q does not exist", name)
	}
	item := current.Networks[index]
	item.Labels = cloneNetworkLabels(item.Labels)
	item.AuxiliaryAddresses = cloneNetworkLabels(item.AuxiliaryAddresses)
	if len(item.Allocations) > 0 {
		item.Allocations = make(map[string]string, len(item.Allocations))
		for id, address := range current.Networks[index].Allocations {
			item.Allocations[id] = address
		}
	}
	if len(item.MACAddresses) > 0 {
		item.MACAddresses = make(map[string]string, len(current.Networks[index].MACAddresses))
		for id, mac := range current.Networks[index].MACAddresses {
			item.MACAddresses[id] = mac
		}
	}
	if len(item.EndpointIDs) > 0 {
		item.EndpointIDs = make(map[string]string, len(current.Networks[index].EndpointIDs))
		for id, endpointID := range current.Networks[index].EndpointIDs {
			item.EndpointIDs[id] = endpointID
		}
	}
	if len(item.Published) > 0 {
		item.Published = make(map[string][]PortMapping, len(current.Networks[index].Published))
		for id, mappings := range current.Networks[index].Published {
			item.Published[id] = append([]PortMapping(nil), mappings...)
		}
	}
	return item, nil
}

// Remove deletes a bridge only after every container has disconnected.
func Remove(name string) error {
	return RemoveWithRunner(context.Background(), name, commandRunner{})
}

// RemoveWithRunner is Remove with an injectable host command executor.
func RemoveWithRunner(ctx context.Context, name string, runner Runner) error {
	if runner == nil {
		return fmt.Errorf("network command runner is required")
	}
	unlock, err := lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	current, dir, err := loadStore()
	if err != nil {
		return err
	}
	index := findNetwork(current.Networks, name)
	if index < 0 {
		return fmt.Errorf("network %q does not exist", name)
	}
	network := current.Networks[index]
	if len(network.Allocations) > 0 {
		return fmt.Errorf("network %q still has %d connected container(s)", name, len(network.Allocations))
	}
	if network.NAT {
		if err := removeNAT(ctx, runner, network); err != nil {
			return err
		}
	}
	if err := removeBridge(ctx, runner, network); err != nil {
		return err
	}
	current.Networks = append(current.Networks[:index], current.Networks[index+1:]...)
	return saveStore(dir, current)
}

func summarize(network Network) Summary {
	return Summary{
		ID: network.ID, Name: network.Name, Driver: network.Driver,
		Bridge: network.Bridge, Subnet: network.Subnet, IPRange: network.IPRange,
		AuxiliaryAddresses: cloneNetworkLabels(network.AuxiliaryAddresses), Gateway: network.Gateway,
		NAT: network.NAT, ContainerCount: len(network.Allocations), CreatedAt: network.CreatedAt,
		Labels: cloneNetworkLabels(network.Labels),
	}
}

func parseIPv4Network(subnet, gateway string) (netip.Prefix, netip.Addr, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(subnet))
	if err != nil || subnet == "" || strings.TrimSpace(subnet) != subnet || !prefix.Addr().Is4() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("subnet must be a canonical IPv4 CIDR")
	}
	if prefix != prefix.Masked() || prefix.Bits() < 16 || prefix.Bits() > 29 {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("subnet must be a canonical IPv4 range between /16 and /29")
	}
	gatewayIP := prefix.Addr().Next()
	if gateway != "" {
		gatewayIP, err = netip.ParseAddr(gateway)
		if err != nil || !gatewayIP.Is4() || strings.TrimSpace(gateway) != gateway {
			return netip.Prefix{}, netip.Addr{}, fmt.Errorf("gateway must be an IPv4 address inside the subnet")
		}
	}
	if !prefix.Contains(gatewayIP) || gatewayIP == prefix.Addr() || gatewayIP == broadcastAddress(prefix) {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("gateway must be a usable host address inside the subnet")
	}
	return prefix, gatewayIP, nil
}

func broadcastAddress(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Addr().As4()
	value := binary.BigEndian.Uint32(bytes[:])
	hostBits := 32 - prefix.Bits()
	if hostBits > 0 {
		value |= uint32(1<<hostBits) - 1
	}
	var result [4]byte
	binary.BigEndian.PutUint32(result[:], value)
	return netip.AddrFrom4(result)
}

func prefixesOverlap(left, right netip.Prefix) bool {
	if !left.IsValid() || !right.IsValid() {
		return false
	}
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func parseIPv4IPRange(subnet netip.Prefix, value string) (netip.Prefix, error) {
	pool, err := netip.ParsePrefix(value)
	if err != nil || !pool.Addr().Is4() || pool != pool.Masked() || pool.Bits() < subnet.Bits() || pool.Bits() > 30 ||
		!subnet.Contains(pool.Addr()) || !subnet.Contains(broadcastAddress(pool)) {
		return netip.Prefix{}, fmt.Errorf("IPAM IPRange %q must be a canonical IPv4 range contained by subnet %s", value, subnet)
	}
	return pool, nil
}

func allocationPrefix(network Network) (netip.Prefix, error) {
	if network.IPRange == "" {
		return netip.ParsePrefix(network.Subnet)
	}
	subnet, err := netip.ParsePrefix(network.Subnet)
	if err != nil {
		return netip.Prefix{}, err
	}
	return parseIPv4IPRange(subnet, network.IPRange)
}

func validateAuxiliaryAddresses(subnet netip.Prefix, gateway netip.Addr, addresses map[string]string) (map[string]string, error) {
	reserved := make(map[string]string, len(addresses))
	for name, value := range addresses {
		if !auxAddressNameRE.MatchString(name) {
			return nil, fmt.Errorf("auxiliary address name %q is invalid", name)
		}
		address, err := netip.ParseAddr(value)
		if err != nil || !address.Is4() || address.String() != value || !subnet.Contains(address) ||
			address == subnet.Addr() || address == broadcastAddress(subnet) || address == gateway {
			return nil, fmt.Errorf("auxiliary address %q must be a usable canonical IPv4 address in subnet %s", value, subnet)
		}
		if previous, exists := reserved[value]; exists {
			return nil, fmt.Errorf("auxiliary address %s is assigned to both %q and %q", value, previous, name)
		}
		reserved[value] = name
	}
	return reserved, nil
}

func allocateEndpointAddress(network Network, requested string) (string, error) {
	if requested == "" {
		return allocateAddress(network)
	}
	address, err := netip.ParseAddr(requested)
	if err != nil || !address.Is4() || address.String() != requested {
		return "", fmt.Errorf("requested endpoint IPv4 address %q is invalid or non-canonical", requested)
	}
	prefix, err := netip.ParsePrefix(network.Subnet)
	if err != nil || !prefix.Contains(address) || address == prefix.Addr() || address == broadcastAddress(prefix) {
		return "", fmt.Errorf("requested endpoint IPv4 address %s is outside usable subnet %s", requested, network.Subnet)
	}
	pool, err := allocationPrefix(network)
	if err != nil || !pool.Contains(address) || address == pool.Addr() || address == broadcastAddress(pool) {
		return "", fmt.Errorf("requested endpoint IPv4 address %s is outside usable IPAM range %s", requested, network.IPRange)
	}
	gateway, err := netip.ParseAddr(network.Gateway)
	if err != nil || address == gateway {
		return "", fmt.Errorf("requested endpoint IPv4 address %s conflicts with the network gateway", requested)
	}
	for name, reserved := range network.AuxiliaryAddresses {
		if reserved == requested {
			return "", fmt.Errorf("requested endpoint IPv4 address %s is reserved as auxiliary address %q", requested, name)
		}
	}
	for containerID, allocated := range network.Allocations {
		if allocated == requested {
			return "", fmt.Errorf("requested endpoint IPv4 address %s is already allocated to container %q", requested, containerID)
		}
	}
	return address.String(), nil
}

func ValidateEndpointMAC(value string) error {
	return validateEndpointMAC(value)
}

func validateEndpointMAC(value string) error {
	address, err := net.ParseMAC(value)
	if err != nil || len(address) != 6 || address.String() != strings.ToLower(value) ||
		address[0]&1 != 0 || address.String() == "00:00:00:00:00:00" {
		return fmt.Errorf("must be a canonical six-byte unicast MAC address")
	}
	return nil
}

func allocateAddress(network Network) (string, error) {
	prefix, err := allocationPrefix(network)
	if err != nil {
		return "", fmt.Errorf("network %q has an invalid IPAM range: %w", network.Name, err)
	}
	gateway, _ := netip.ParseAddr(network.Gateway)
	networkBytes := prefix.Addr().As4()
	first := binary.BigEndian.Uint32(networkBytes[:])
	lastBytes := broadcastAddress(prefix).As4()
	last := binary.BigEndian.Uint32(lastBytes[:])
	used := make(map[string]struct{}, len(network.Allocations)+len(network.AuxiliaryAddresses)+1)
	used[gateway.String()] = struct{}{}
	for _, value := range network.AuxiliaryAddresses {
		used[value] = struct{}{}
	}
	for _, value := range network.Allocations {
		used[value] = struct{}{}
	}
	for value := first + 1; value < last; value++ {
		var addressBytes [4]byte
		binary.BigEndian.PutUint32(addressBytes[:], value)
		address := netip.AddrFrom4(addressBytes).String()
		if _, exists := used[address]; !exists {
			return address, nil
		}
	}
	return "", fmt.Errorf("network %q has no free IPv4 addresses", network.Name)
}

func prefixLength(value string) string {
	prefix, _ := netip.ParsePrefix(value)
	return fmt.Sprint(prefix.Bits())
}

func firewallChains(id string) (string, string) {
	suffix := strings.ToUpper(id[:8])
	return "BXF" + suffix, "BXN" + suffix
}

func vethAlias(networkID, containerID string) string {
	return "boxr:" + networkID + ":" + containerID
}

func verifyVethOwnership(ctx context.Context, runner Runner, networkID, containerID string) error {
	inspector, ok := runner.(OutputRunner)
	if !ok {
		return fmt.Errorf("network command runner cannot inspect link ownership")
	}
	hostVeth, _ := vethNames(containerID)
	output, err := inspector.Output(ctx, "ip", "-details", "-o", "link", "show", "dev", hostVeth)
	if err != nil {
		if missingNetworkInterface(err) {
			return nil
		}
		return fmt.Errorf("inspect host veth: %w", err)
	}
	fields := strings.Fields(string(output))
	for index, field := range fields {
		if field == "alias" && index+1 < len(fields) && fields[index+1] == vethAlias(networkID, containerID) {
			return nil
		}
	}
	return fmt.Errorf("interface alias does not match the recorded Boxr network and container identity")
}

func endpointIdentity(networkID, containerID string) (string, string) {
	identity := []byte("boxr-network-endpoint\x00" + networkID + "\x00" + containerID)
	endpointDigest := sha256.Sum256(identity)
	macDigest := sha256.Sum256(append([]byte("boxr-network-mac\x00"), identity...))
	mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x",
		macDigest[0], macDigest[1], macDigest[2], macDigest[3], macDigest[4])
	return hex.EncodeToString(endpointDigest[:]), mac
}

func vethNames(containerID string) (string, string) {
	digest := sha256.Sum256([]byte(containerID))
	suffix := hex.EncodeToString(digest[:])[:8]
	return "vbx" + suffix, "cbx" + suffix
}

func validInterfaceName(value string) bool {
	if value == "." || value == ".." || len(value) == 0 || len(value) > 15 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.') {
			return false
		}
	}
	return true
}

func findNetwork(networks []Network, name string) int {
	for index := range networks {
		if networks[index].Name == name || networks[index].ID == name {
			return index
		}
	}
	return -1
}

func newID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate network ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
