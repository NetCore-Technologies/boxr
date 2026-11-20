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
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	storeVersion   = 1
	maxStoreBytes  = 4 << 20
	maxNetworkCount = 128
)

var (
	storeMu       sync.Mutex
	networkNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,30}$`)
)

// Network is the persistent identity and IPAM state of one bridge network.
// Allocations are keyed by Boxr container ID, never by user-controlled names.
type Network struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Driver      string            `json:"driver"`
	Bridge      string            `json:"bridge"`
	Subnet      string            `json:"subnet"`
	Gateway     string            `json:"gateway"`
	NAT         bool              `json:"nat"`
	CreatedAt   time.Time         `json:"createdAt"`
	Allocations map[string]string `json:"allocations,omitempty"`
}

// CreateOptions configures an IPv4 bridge and whether it receives outbound NAT.
type CreateOptions struct {
	Subnet  string
	Gateway string
	Bridge  string
	NAT     bool
}

// Summary is safe for CLI output; it omits per-container allocation identities.
type Summary struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Driver         string    `json:"driver"`
	Bridge         string    `json:"bridge"`
	Subnet         string    `json:"subnet"`
	Gateway        string    `json:"gateway"`
	NAT            bool      `json:"nat"`
	ContainerCount int       `json:"containerCount"`
	CreatedAt      time.Time `json:"createdAt"`
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
	names := make(map[string]struct{}, len(networks))
	ids := make(map[string]struct{}, len(networks))
	bridges := make(map[string]struct{}, len(networks))
	prefixes := make([]netip.Prefix, 0, len(networks))
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
		for _, previous := range prefixes {
			if prefixesOverlap(prefix, previous) {
				return fmt.Errorf("network store contains overlapping IPv4 subnets")
			}
		}
		prefixes = append(prefixes, prefix)
		allocated := make(map[string]struct{}, len(network.Allocations))
		for containerID, addressText := range network.Allocations {
			address, err := netip.ParseAddr(addressText)
			if containerID == "" || strings.ContainsAny(containerID, "/\\\x00") ||
				err != nil || !address.Is4() || !prefix.Contains(address) || address == gateway || address == prefix.Addr() || address == broadcastAddress(prefix) {
				return fmt.Errorf("network store contains an invalid IPAM allocation for %q", network.Name)
			}
			if _, exists := allocated[addressText]; exists {
				return fmt.Errorf("network store contains a duplicate IPAM address")
			}
			allocated[addressText] = struct{}{}
		}
		names[network.Name] = struct{}{}
		ids[network.ID] = struct{}{}
		bridges[network.Bridge] = struct{}{}
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
	prefix, gateway, err := parseIPv4Network(options.Subnet, options.Gateway)
	if err != nil {
		return empty, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
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
		ID:          id,
		Name:        name,
		Driver:      "bridge",
		Bridge:      bridge,
		Subnet:      prefix.String(),
		Gateway:     gateway.String(),
		NAT:         options.NAT,
		CreatedAt:   time.Now().UTC(),
		Allocations: make(map[string]string),
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
func Join(networkName string, pid int, containerID string) (string, error) {
	return JoinWithRunner(context.Background(), networkName, pid, containerID, commandRunner{})
}

// JoinWithRunner is Join with an injectable host command executor.
func JoinWithRunner(ctx context.Context, networkName string, pid int, containerID string, runner Runner) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("network command runner is required")
	}
	if pid <= 1 || strings.TrimSpace(containerID) == "" || strings.ContainsAny(containerID, "/\\\x00") {
		return "", fmt.Errorf("valid host PID and container ID are required")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	current, dir, err := loadStore()
	if err != nil {
		return "", err
	}
	index := findNetwork(current.Networks, networkName)
	if index < 0 {
		return "", fmt.Errorf("network %q does not exist", networkName)
	}
	network := &current.Networks[index]
	if _, exists := network.Allocations[containerID]; exists {
		return "", fmt.Errorf("container %q is already connected to network %q", containerID, networkName)
	}
	address, err := allocateAddress(*network)
	if err != nil {
		return "", err
	}
	if network.Allocations == nil {
		network.Allocations = make(map[string]string)
	}
	network.Allocations[containerID] = address
	if err := saveStore(dir, current); err != nil {
		delete(network.Allocations, containerID)
		return "", err
	}
	hostVeth, peerVeth := vethNames(containerID)
	releaseAllocation := func() {
		delete(network.Allocations, containerID)
		_ = saveStore(dir, current)
	}
	rollbackCreatedVeth := func(cause error) (string, error) {
		_ = runner.Run(ctx, "ip", "link", "del", "dev", hostVeth)
		releaseAllocation()
		return "", cause
	}
	steps := []struct {
		binary string
		args   []string
	}{
		{"ip", []string{"link", "add", hostVeth, "type", "veth", "peer", "name", peerVeth}},
		{"ip", []string{"link", "set", hostVeth, "master", network.Bridge}},
		{"ip", []string{"link", "set", "dev", hostVeth, "up"}},
		{"ip", []string{"link", "set", peerVeth, "netns", fmt.Sprint(pid)}},
		{"nsenter", []string{"-t", fmt.Sprint(pid), "-n", "--", "ip", "link", "set", "dev", peerVeth, "name", "eth0"}},
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
				releaseAllocation()
				return "", wrapped
			}
			return rollbackCreatedVeth(wrapped)
		}
	}
	return address, nil
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
	storeMu.Lock()
	defer storeMu.Unlock()
	current, dir, err := loadStore()
	if err != nil {
		return err
	}
	index := findNetwork(current.Networks, networkName)
	if index < 0 {
		return fmt.Errorf("network %q does not exist", networkName)
	}
	network := &current.Networks[index]
	if _, exists := network.Allocations[containerID]; !exists {
		return fmt.Errorf("container %q has no allocation on network %q", containerID, networkName)
	}
	hostVeth, _ := vethNames(containerID)
	if err := runner.Run(ctx, "ip", "link", "del", "dev", hostVeth); err != nil && !missingNetworkInterface(err) {
		return fmt.Errorf("disconnect container from network %q: %w", networkName, err)
	}
	delete(network.Allocations, containerID)
	return saveStore(dir, current)
}

// List returns networks sorted by name without exposing allocation identities.
func List() ([]Summary, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
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
	storeMu.Lock()
	defer storeMu.Unlock()
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

// Remove deletes a bridge only after every container has disconnected.
func Remove(name string) error {
	return RemoveWithRunner(context.Background(), name, commandRunner{})
}

// RemoveWithRunner is Remove with an injectable host command executor.
func RemoveWithRunner(ctx context.Context, name string, runner Runner) error {
	if runner == nil {
		return fmt.Errorf("network command runner is required")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
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
		Bridge: network.Bridge, Subnet: network.Subnet, Gateway: network.Gateway,
		NAT: network.NAT, ContainerCount: len(network.Allocations), CreatedAt: network.CreatedAt,
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

func allocateAddress(network Network) (string, error) {
	prefix, _ := netip.ParsePrefix(network.Subnet)
	gateway, _ := netip.ParseAddr(network.Gateway)
	networkBytes := prefix.Addr().As4()
	first := binary.BigEndian.Uint32(networkBytes[:])
	lastBytes := broadcastAddress(prefix).As4()
	last := binary.BigEndian.Uint32(lastBytes[:])
	used := make(map[string]struct{}, len(network.Allocations)+1)
	used[gateway.String()] = struct{}{}
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
