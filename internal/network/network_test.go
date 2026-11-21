package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls       []string
	failAt      int
	failMessage string
	aliases     map[string]string
}

func (runner *fakeRunner) Run(_ context.Context, executable string, args ...string) error {
	call := executable + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	if runner.failAt > 0 && len(runner.calls) == runner.failAt {
		message := runner.failMessage
		if message == "" {
			message = "injected command failure"
		}
		return errors.New(message)
	}
	if strings.Contains(call, "iptables -C ") || strings.Contains(call, "iptables -t nat -C ") {
		return errors.New("Bad rule (does a matching rule exist in that chain?)")
	}
	if executable == "ip" && len(args) >= 6 && args[0] == "link" && args[1] == "set" && args[2] == "dev" && args[4] == "alias" {
		if runner.aliases == nil {
			runner.aliases = make(map[string]string)
		}
		runner.aliases[args[3]] = args[5]
	}
	if executable == "ip" && len(args) >= 4 && args[0] == "link" && args[1] == "del" && args[2] == "dev" {
		delete(runner.aliases, args[3])
	}
	return nil
}

func (runner *fakeRunner) Output(_ context.Context, executable string, args ...string) ([]byte, error) {
	call := executable + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, "OUTPUT "+call)
	if executable != "ip" || len(args) != 6 || args[0] != "-details" && args[0] != "-o" {
		return nil, errors.New("unexpected output command: " + call)
	}
	device := args[len(args)-1]
	alias, ok := runner.aliases[device]
	if !ok {
		return nil, errors.New("Cannot find device " + device)
	}
	return []byte("7: " + device + ": <BROADCAST,UP> alias " + alias + "\n"), nil
}

func TestCreateBridgePersistsNetworkAndConfiguresGateway(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &fakeRunner{}
	network, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{
		Subnet: "172.30.10.0/29",
		NAT:    false,
	}, runner)
	if err != nil {
		t.Fatalf("CreateWithRunner(): %v", err)
	}
	if network.Name != "frontend" || network.Driver != "bridge" ||
		network.Bridge == "" || network.Gateway != "172.30.10.1" || network.Subnet != "172.30.10.0/29" {
		t.Fatalf("unexpected network: %+v", network)
	}
	if len(runner.calls) != 3 ||
		!strings.Contains(runner.calls[0], "link add name "+network.Bridge+" type bridge") ||
		!strings.Contains(runner.calls[1], "addr add 172.30.10.1/29 dev "+network.Bridge) ||
		!strings.Contains(runner.calls[2], "link set dev "+network.Bridge+" up") {
		t.Fatalf("unexpected bridge provisioning: %v", runner.calls)
	}
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "frontend" || list[0].ContainerCount != 0 {
		t.Fatalf("List() = %+v", list)
	}
	inspected, err := Inspect(network.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Bridge != network.Bridge || inspected.Gateway != network.Gateway {
		t.Fatalf("Inspect() = %+v", inspected)
	}
	info, err := os.Stat(filepath.Join(mustStoreDir(t), "networks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("network store mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestCreateBridgeRejectsInvalidAndOverlappingNetworks(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &fakeRunner{}
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{Subnet: "172.30.0.0/24"}, runner); err != nil {
		t.Fatal(err)
	}
	tests := map[string]CreateOptions{
		"invalid name":       {Subnet: "172.31.0.0/24"},
		"non-canonical cidr": {Subnet: "172.30.1.1/24"},
		"external gateway":   {Subnet: "172.31.0.0/24", Gateway: "192.0.2.1"},
		"network address":    {Subnet: "172.31.0.0/24", Gateway: "172.31.0.0"},
		"broadcast address":  {Subnet: "172.31.0.0/24", Gateway: "172.31.0.255"},
		"ipv6 subnet":        {Subnet: "2001:db8::/64"},
		"small subnet":       {Subnet: "172.31.0.0/30"},
		"too broad subnet":   {Subnet: "172.0.0.0/8"},
		"overlapping subnet": {Subnet: "172.30.0.128/25"},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			networkName := "other"
			if name == "invalid name" {
				networkName = "../invalid"
			}
			if _, err := CreateWithRunner(context.Background(), networkName, options, runner); err == nil {
				t.Fatal("invalid network configuration unexpectedly accepted")
			}
		})
	}
}

func TestCreateBridgeDoesNotDeleteExistingInterfaceWhenCreateFails(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &fakeRunner{failAt: 1, failMessage: "RTNETLINK answers: File exists"}
	if _, err := CreateWithRunner(context.Background(), "collision", CreateOptions{
		Subnet: "172.30.15.0/24",
		Bridge: "br-existing",
	}, runner); err == nil {
		t.Fatal("bridge interface collision unexpectedly succeeded")
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], "link add name br-existing") {
		t.Fatalf("failed bridge create attempted to remove a pre-existing interface: %v", runner.calls)
	}
	items, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("failed bridge create was persisted: %+v", items)
	}
}

func TestCreateBridgeRollsBackAfterProvisioningFailure(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &fakeRunner{failAt: 2}
	if _, err := CreateWithRunner(context.Background(), "broken", CreateOptions{
		Subnet: "172.30.20.0/24",
	}, runner); err == nil {
		t.Fatal("bridge provisioning failure unexpectedly succeeded")
	}
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("failed bridge was persisted: %+v", list)
	}
	if len(runner.calls) < 3 || !strings.Contains(runner.calls[len(runner.calls)-1], "link del dev") {
		t.Fatalf("failed bridge cleanup was not attempted: %v", runner.calls)
	}
}

func TestCreateBridgeNATInstallsScopedFirewallRules(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	runner := &fakeRunner{}
	network, err := CreateWithRunner(context.Background(), "egress", CreateOptions{
		Subnet: "172.30.30.0/24",
		NAT:    true,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	filterChain, natChain := firewallChains(network.ID)
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"sysctl -w net.ipv4.ip_forward=1",
		"iptables -N " + filterChain,
		"iptables -t nat -N " + natChain,
		"iptables -I FORWARD 1 -j " + filterChain,
		"iptables -t nat -I POSTROUTING 1 -j " + natChain,
		"iptables -t nat -A " + natChain + " -s 172.30.30.0/24 ! -o " + network.Bridge + " -j MASQUERADE",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("network NAT setup missing %q; calls: %s", expected, joined)
		}
	}
}

func TestJoinAllocatesConfiguresAndLeaveReleasesAddress(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	creator := &fakeRunner{}
	network, err := CreateWithRunner(context.Background(), "backend", CreateOptions{
		Subnet: "172.30.40.0/29",
	}, creator)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	address, err := JoinWithRunner(context.Background(), "backend", 1234, "container-01", runner)
	if err != nil {
		t.Fatalf("JoinWithRunner(): %v", err)
	}
	if address != "172.30.40.2" {
		t.Fatalf("allocated address = %s, want 172.30.40.2", address)
	}
	if len(runner.calls) != 11 {
		t.Fatalf("join command count = %d, want 11: %v", len(runner.calls), runner.calls)
	}
	if !strings.Contains(runner.calls[0], "link add vbx") ||
		!strings.Contains(runner.calls[1], "alias boxr:"+network.ID+":container-01") ||
		!strings.Contains(runner.calls[4], "netns 1234") ||
		!strings.Contains(runner.calls[5], "nsenter -t 1234 -n -- ip link set dev cbx") ||
		!strings.Contains(runner.calls[6], "ip link set dev eth0 address 02:") ||
		!strings.Contains(runner.calls[10], "route add default via "+network.Gateway) {
		t.Fatalf("unexpected veth/namespace setup sequence: %v", runner.calls)
	}
	if _, err := JoinWithRunner(context.Background(), "backend", 1235, "container-01", runner); err == nil {
		t.Fatal("duplicate container network attachment unexpectedly accepted")
	}
	summary, err := Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 1 {
		t.Fatalf("container count = %d, want 1", summary.ContainerCount)
	}
	details, err := InspectDetails("backend")
	if err != nil {
		t.Fatal(err)
	}
	macAddress := details.MACAddresses["container-01"]
	endpointID := details.EndpointIDs["container-01"]
	if macAddress == "" || !strings.HasPrefix(macAddress, "02:") || len(endpointID) != 64 {
		t.Fatalf("persisted endpoint identity = MAC %q ID %q", macAddress, endpointID)
	}
	if !strings.Contains(runner.calls[6], macAddress) {
		t.Fatalf("configured interface MAC does not match persisted identity: %v", runner.calls[6])
	}
	if err := RemoveWithRunner(context.Background(), "backend", runner); err == nil {
		t.Fatal("network with an attached container unexpectedly removed")
	}
	if err := LeaveWithRunner(context.Background(), "backend", "container-01", runner); err != nil {
		t.Fatalf("LeaveWithRunner(): %v", err)
	}
	summary, err = Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("container count after leave = %d, want 0", summary.ContainerCount)
	}
	details, err = InspectDetails("backend")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := details.MACAddresses["container-01"]; exists {
		t.Fatal("disconnected endpoint retained its MAC address")
	}
	if _, exists := details.EndpointIDs["container-01"]; exists {
		t.Fatal("disconnected endpoint retained its endpoint ID")
	}
	if _, err := JoinWithRunner(context.Background(), "backend", 1234, "container-02", runner); err != nil {
		t.Fatalf("released IPv4 address was not reusable: %v", err)
	}
}

func TestJoinDoesNotDeletePreExistingVethWhenCreateFails(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "backend", CreateOptions{
		Subnet: "172.30.45.0/29",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{failAt: 1, failMessage: "RTNETLINK answers: File exists"}
	if _, err := JoinWithRunner(context.Background(), "backend", 2222, "container-collision", runner); err == nil {
		t.Fatal("veth name collision unexpectedly succeeded")
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], "link add vbx") {
		t.Fatalf("veth collision attempted to remove an unrelated interface: %v", runner.calls)
	}
	summary, err := Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("veth collision retained an IPAM allocation: %+v", summary)
	}
}

func TestLeaveReleasesAllocationWhenVethWasAlreadyRemoved(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "backend", CreateOptions{
		Subnet: "172.30.46.0/29",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := JoinWithRunner(context.Background(), "backend", 2222, "container-gone", &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{failAt: 1, failMessage: "Cannot find device vbxdeadbeef"}
	if err := LeaveWithRunner(context.Background(), "backend", "container-gone", runner); err != nil {
		t.Fatalf("LeaveWithRunner() for a removed veth: %v", err)
	}
	summary, err := Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("stale allocation was not released: %+v", summary)
	}
}

func TestJoinPublishesPortsAndLeaveRemovesScopedRules(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{
		Subnet: "172.30.70.0/24",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	ports := []PortMapping{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80},
		{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
	}
	address, err := JoinWithRunner(context.Background(), "frontend", 3322, "container-ports", runner, ports...)
	if err != nil {
		t.Fatalf("JoinWithRunner(ports): %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"iptables -t nat -A PREROUTING -p tcp -d 127.0.0.1 --dport 8080 -j DNAT --to-destination " + address + ":80",
		"iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 8080 -j DNAT --to-destination " + address + ":80",
		"iptables -I FORWARD 1 -o br",
		"iptables -t nat -A PREROUTING -p udp --dport 5353 -j DNAT --to-destination " + address + ":53",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("published port rules missing %q; calls: %s", expected, joined)
		}
	}
	if err := LeaveWithRunner(context.Background(), "frontend", "container-ports", runner); err != nil {
		t.Fatalf("LeaveWithRunner(): %v", err)
	}
	leaveCalls := strings.Join(runner.calls[len(runner.calls)-8:], "\n")
	for _, expected := range []string{
		"iptables -t nat -D PREROUTING -p tcp -d 127.0.0.1 --dport 8080",
		"iptables -t nat -D OUTPUT -p tcp -d 127.0.0.1 --dport 8080",
		"iptables -D FORWARD -o br",
		"iptables -t nat -D PREROUTING -p udp --dport 5353",
	} {
		if !strings.Contains(leaveCalls, expected) {
			t.Errorf("published port cleanup missing %q; calls: %s", expected, leaveCalls)
		}
	}
	if _, err := JoinWithRunner(context.Background(), "frontend", 3322, "container-reuse", runner, PortMapping{HostPort: 8080, ContainerPort: 80}); err != nil {
		t.Fatalf("released host port could not be reused: %v", err)
	}
}

func TestJoinRejectsPublishedPortConflictsAcrossNetworks(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	for _, network := range []struct{ name, subnet string }{
		{name: "frontend", subnet: "172.30.71.0/24"},
		{name: "backend", subnet: "172.30.72.0/24"},
	} {
		if _, err := CreateWithRunner(context.Background(), network.name, CreateOptions{Subnet: network.subnet}, &fakeRunner{}); err != nil {
			t.Fatal(err)
		}
	}
	mapping := PortMapping{HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443}
	if _, err := JoinWithRunner(context.Background(), "frontend", 3323, "container-front", &fakeRunner{}, mapping); err != nil {
		t.Fatal(err)
	}
	wildcardConflict := &fakeRunner{}
	if _, err := JoinWithRunner(context.Background(), "backend", 3324, "container-back", wildcardConflict, PortMapping{HostPort: 8443, ContainerPort: 8443}); err == nil {
		t.Fatal("wildcard bind conflict across networks unexpectedly succeeded")
	}
	if len(wildcardConflict.calls) != 0 {
		t.Fatalf("host port conflict modified host networking before rejection: %v", wildcardConflict.calls)
	}
	summary, err := Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("rejected port conflict retained an IPAM lease: %+v", summary)
	}
}

func TestJoinRollsBackPublishedRulesWhenFirewallSetupFails(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{Subnet: "172.30.73.0/24"}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	// Eleven link/namespace operations succeed. Fail after the first DNAT rule is installed and
	// enabling forwarding and then verify every partial rule and lease is cleaned.
	runner := &fakeRunner{failAt: 15}
	_, err := JoinWithRunner(context.Background(), "frontend", 3325, "container-firewall-fail", runner, PortMapping{HostPort: 8081, ContainerPort: 80})
	if err == nil {
		t.Fatal("injected firewall failure unexpectedly succeeded")
	}
	if _, err := Inspect("frontend"); err != nil {
		t.Fatal(err)
	}
	summary, err := Inspect("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("failed published-port setup retained IPAM allocation: %+v", summary)
	}
	if !strings.Contains(strings.Join(runner.calls, "\n"), "iptables -t nat -D PREROUTING") {
		t.Fatalf("partial DNAT rule was not rolled back: %v", runner.calls)
	}
}

func TestJoinRollsBackIPAMWhenNamespaceSetupFails(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "backend", CreateOptions{
		Subnet: "172.30.50.0/29",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{failAt: 5}
	if _, err := JoinWithRunner(context.Background(), "backend", 2222, "container-fail", runner); err == nil {
		t.Fatal("injected namespace setup failure unexpectedly succeeded")
	}
	summary, err := Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("failed attachment retained an IPAM allocation: %+v", summary)
	}
	if !strings.Contains(runner.calls[len(runner.calls)-1], "link del dev vbx") {
		t.Fatalf("failed veth was not removed: %v", runner.calls)
	}
}

func TestRemoveBridgeDeletesFirewallChainsAndInterface(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	network, err := CreateWithRunner(context.Background(), "remove-me", CreateOptions{
		Subnet: "172.30.60.0/24",
		NAT:    true,
	}, &fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if err := RemoveWithRunner(context.Background(), "remove-me", runner); err != nil {
		t.Fatalf("RemoveWithRunner(): %v", err)
	}
	filterChain, natChain := firewallChains(network.ID)
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"iptables -D FORWARD -j " + filterChain,
		"iptables -F " + filterChain,
		"iptables -X " + filterChain,
		"iptables -t nat -D POSTROUTING -j " + natChain,
		"iptables -t nat -F " + natChain,
		"iptables -t nat -X " + natChain,
		"ip link del dev " + network.Bridge,
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("network removal missing %q; calls: %s", expected, joined)
		}
	}
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("removed network remains in store: %+v", list)
	}
}

func TestNetworkStorageDirectoryUsesExpectedPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BOXR_NETWORK_DIR", "")
	t.Setenv("BOXR_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	dir, err := StoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(home, ".local", "share", "boxr", "networks") {
		t.Fatalf("StoreDir() = %q", dir)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
	dir, err = StoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(home, "xdg", "boxr", "networks") {
		t.Fatalf("XDG StoreDir() = %q", dir)
	}
}

func mustStoreDir(t *testing.T) string {
	t.Helper()
	dir, err := StoreDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNetworkStoreRejectsTrailingJSONAndUnsafeRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "networks")
	t.Setenv("BOXR_NETWORK_DIR", dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"trailing JSON":        `{"version":1,"networks":[]} {}`,
		"unknown field":        `{"version":1,"networks":[],"unexpected":true}`,
		"invalid bridge name":  `{"version":1,"networks":[{"id":"0123456789abcdef","name":"frontend","driver":"bridge","bridge":"br/name","subnet":"172.30.0.0/24","gateway":"172.30.0.1","nat":false}]}`,
		"overlapping networks": `{"version":1,"networks":[{"id":"0123456789abcdef","name":"frontend","driver":"bridge","bridge":"brfront","subnet":"172.30.0.0/24","gateway":"172.30.0.1","nat":false},{"id":"fedcba9876543210","name":"backend","driver":"bridge","bridge":"brback","subnet":"172.30.0.128/25","gateway":"172.30.0.129","nat":false}]}`,
		"duplicate allocation": `{"version":1,"networks":[{"id":"0123456789abcdef","name":"frontend","driver":"bridge","bridge":"brfront","subnet":"172.30.0.0/24","gateway":"172.30.0.1","nat":false,"allocations":{"container-a":"172.30.0.2","container-b":"172.30.0.2"}}]}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "networks.json"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := List(); err == nil {
				t.Fatal("unsafe network store unexpectedly loaded")
			}
		})
	}
}

func TestNetworkStoreCreatesOwnerOnlyCrossProcessLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "networks")
	t.Setenv("BOXR_NETWORK_DIR", dir)
	if _, err := List(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "networks.lock"))
	if err != nil {
		t.Fatalf("stat network store lock: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("network store lock mode = %v, want a regular 0600 file", info.Mode())
	}
}

func TestLeaveKeepsIPAMReservationUntilPublishedRulesAreRemoved(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{Subnet: "172.30.74.0/24"}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	mapping := PortMapping{HostPort: 18080, ContainerPort: 80}
	if _, err := JoinWithRunner(context.Background(), "frontend", 3326, "container-cleanup", &fakeRunner{}, mapping); err != nil {
		t.Fatal(err)
	}
	failing := &fakeRunner{failAt: 2, failMessage: "firewall backend unavailable"}
	if err := LeaveWithRunner(context.Background(), "frontend", "container-cleanup", failing); err == nil {
		t.Fatal("published port cleanup failure unexpectedly succeeded")
	}
	summary, err := Inspect("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 1 {
		t.Fatalf("failed firewall cleanup released the IPAM reservation: %+v", summary)
	}
	blocked := &fakeRunner{}
	if _, err := JoinWithRunner(context.Background(), "frontend", 3327, "container-blocked", blocked, mapping); err == nil {
		t.Fatal("host port reservation was released before cleanup succeeded")
	}
	if len(blocked.calls) != 0 {
		t.Fatalf("blocked port reuse changed host network: %v", blocked.calls)
	}
	if err := LeaveWithRunner(context.Background(), "frontend", "container-cleanup", &fakeRunner{}); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if _, err := JoinWithRunner(context.Background(), "frontend", 3327, "container-retry", &fakeRunner{}, mapping); err != nil {
		t.Fatalf("released port could not be reused after cleanup: %v", err)
	}
}

func TestContainerCannotJoinTwoManagedBridgeNetworksAtOnce(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	for _, item := range []struct {
		name, subnet string
	}{
		{name: "frontend", subnet: "172.30.81.0/24"},
		{name: "backend", subnet: "172.30.82.0/24"},
	} {
		if _, err := CreateWithRunner(context.Background(), item.name, CreateOptions{Subnet: item.subnet}, &fakeRunner{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := JoinWithRunner(context.Background(), "frontend", 4101, "container-shared", &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if _, err := JoinWithRunner(context.Background(), "backend", 4101, "container-shared", runner); err == nil {
		t.Fatal("container was attached to two bridge networks with colliding eth0/veth identity")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("second network attempted host mutation before rejecting the container: %v", runner.calls)
	}
	frontend, err := Inspect("frontend")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := Inspect("backend")
	if err != nil {
		t.Fatal(err)
	}
	if frontend.ContainerCount != 1 || backend.ContainerCount != 0 {
		t.Fatalf("unexpected network membership: frontend=%+v backend=%+v", frontend, backend)
	}
}

func TestLeaveAllDetachesDynamicContainerAndReleasesPortLease(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{
		Subnet: "172.30.83.0/24",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := JoinWithRunner(context.Background(), "frontend", 4201, "container-dynamic", &fakeRunner{},
		PortMapping{HostPort: 18443, ContainerPort: 443}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if err := LeaveAllWithRunner(context.Background(), "container-dynamic", runner); err != nil {
		t.Fatalf("LeaveAllWithRunner(): %v", err)
	}
	summary, err := Inspect("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("dynamic container lease survived LeaveAll: %+v", summary)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"iptables -t nat -D PREROUTING",
		"iptables -t nat -D OUTPUT",
		"iptables -D FORWARD",
		"ip link del dev vbx",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("LeaveAll omitted cleanup %q: %s", expected, joined)
		}
	}
	if err := LeaveAllWithRunner(context.Background(), "container-dynamic", &fakeRunner{}); err != nil {
		t.Fatalf("LeaveAll should be idempotent for a container with no network lease: %v", err)
	}
}

func TestStaleAttachmentPruneRemovesOnlyVerifiedBoxrLinks(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{
		Subnet: "172.30.84.0/24",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if _, err := JoinWithRunner(context.Background(), "frontend", 4301, "container-stale", runner,
		PortMapping{HostPort: 18444, ContainerPort: 443}); err != nil {
		t.Fatal(err)
	}
	live := map[string]struct{}{"container-live": {}}
	stale, err := StaleAttachments(live)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ContainerID != "container-stale" ||
		stale[0].Network != "frontend" || stale[0].Address != "172.30.84.2" ||
		len(stale[0].PublishedPorts) != 1 {
		t.Fatalf("stale attachments = %+v", stale)
	}
	report, err := PruneStaleWithRunner(context.Background(), live, runner)
	if err != nil {
		t.Fatalf("PruneStaleWithRunner(): %v", err)
	}
	if report.AttachmentsRemoved != 1 || report.AddressesReleased != 1 || report.PublishedPortsRemoved != 1 {
		t.Fatalf("prune report = %+v", report)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"OUTPUT ip -details -o link show dev vbx",
		"ip link del dev vbx",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("stale prune did not perform %q: %s", expected, joined)
		}
	}
	summary, err := Inspect("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 0 {
		t.Fatalf("stale lease remained after prune: %+v", summary)
	}
}

func TestStaleAttachmentPruneRefusesUnownedVethAndRetainsReservation(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	network, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{
		Subnet: "172.30.85.0/24",
	}, &fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if _, err := JoinWithRunner(context.Background(), "frontend", 4302, "container-foreign", runner); err != nil {
		t.Fatal(err)
	}
	hostVeth, _ := vethNames("container-foreign")
	runner.aliases[hostVeth] = "host:someone-else"
	report, err := PruneStaleWithRunner(context.Background(), map[string]struct{}{}, runner)
	if err == nil || report.AttachmentsRemoved != 0 {
		t.Fatalf("prune report=%+v error=%v; expected refusal to delete foreign interface", report, err)
	}
	summary, err := Inspect(network.Name)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 1 {
		t.Fatalf("foreign veth refusal released the IPAM reservation: %+v", summary)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "ip link del dev "+hostVeth) {
			t.Fatalf("prune attempted to delete an interface without a Boxr ownership alias: %v", runner.calls)
		}
	}
}

func TestLeaveRefusesVethWithMismatchedOwnershipAlias(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "frontend", CreateOptions{
		Subnet: "172.30.86.0/24",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if _, err := JoinWithRunner(context.Background(), "frontend", 4303, "container-mismatch", runner,
		PortMapping{HostPort: 18445, ContainerPort: 445}); err != nil {
		t.Fatal(err)
	}
	hostVeth, _ := vethNames("container-mismatch")
	runner.aliases[hostVeth] = "boxr:another-network:container-mismatch"
	callsBeforeDetach := len(runner.calls)
	if err := LeaveWithRunner(context.Background(), "frontend", "container-mismatch", runner); err == nil {
		t.Fatal("LeaveWithRunner deleted a veth with a mismatched ownership alias")
	}
	for _, call := range runner.calls[callsBeforeDetach:] {
		if strings.Contains(call, "iptables -D") || strings.Contains(call, "iptables -t nat -D") {
			t.Fatalf("ownership mismatch removed published port firewall rules: %v", runner.calls[callsBeforeDetach:])
		}
	}
	summary, err := Inspect("frontend")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ContainerCount != 1 {
		t.Fatalf("ownership mismatch released allocation: %+v", summary)
	}
}

func TestJoinWithOptionsUsesStaticIPv4AndMAC(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "static-endpoint", CreateOptions{
		Subnet: "172.30.48.0/29",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	options := EndpointOptions{
		IPv4Address: "172.30.48.5",
		MACAddress:  "02:aa:bb:cc:dd:ee",
	}
	address, err := JoinWithRunnerOptions(context.Background(), "static-endpoint", 2401, "container-static", runner, options)
	if err != nil {
		t.Fatalf("JoinWithRunnerOptions(): %v", err)
	}
	if address != options.IPv4Address {
		t.Fatalf("allocated IPv4 address=%q, want %q", address, options.IPv4Address)
	}
	if len(runner.calls) != 11 || !strings.Contains(runner.calls[6], "address "+options.MACAddress) {
		t.Fatalf("configured endpoint MAC missing from veth setup: %v", runner.calls)
	}
	network, err := InspectDetails("static-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	if network.Allocations["container-static"] != options.IPv4Address ||
		network.MACAddresses["container-static"] != options.MACAddress ||
		len(network.EndpointIDs["container-static"]) != 64 {
		t.Fatalf("persisted static endpoint metadata = %+v", network)
	}

	before := len(runner.calls)
	if _, err := JoinWithRunnerOptions(context.Background(), "static-endpoint", 2402, "container-duplicate-ip", runner, options); err == nil {
		t.Fatal("duplicate static IPv4 address unexpectedly allocated")
	}
	if len(runner.calls) != before {
		t.Fatalf("duplicate address touched host interfaces: %v", runner.calls[before:])
	}
	duplicateMAC := EndpointOptions{IPv4Address: "172.30.48.6", MACAddress: options.MACAddress}
	if _, err := JoinWithRunnerOptions(context.Background(), "static-endpoint", 2403, "container-duplicate-mac", runner, duplicateMAC); err == nil {
		t.Fatal("duplicate endpoint MAC unexpectedly allocated")
	}
	if len(runner.calls) != before {
		t.Fatalf("duplicate MAC touched host interfaces: %v", runner.calls[before:])
	}
}

func TestJoinWithOptionsRejectsReservedAndOutOfSubnetIPv4Addresses(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	if _, err := CreateWithRunner(context.Background(), "static-invalid", CreateOptions{
		Subnet: "172.30.49.0/29",
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{
		"172.30.49.0", "172.30.49.1", "172.30.49.7",
		"172.30.49.8", "172.30.49.10", "172.030.049.5", "not-an-ip",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := JoinWithRunnerOptions(context.Background(), "static-invalid", 2403, "container-invalid", &fakeRunner{}, EndpointOptions{IPv4Address: address}); err == nil {
				t.Fatalf("invalid or reserved IPv4 address %q was accepted", address)
			}
		})
	}
}

func TestValidateEndpointMACRejectsUnsafeAndNonCanonicalAddresses(t *testing.T) {
	for _, address := range []string{
		"", "00:00:00:00:00:00", "01:00:5e:00:00:01",
		"ff:ff:ff:ff:ff:ff", "02:aa:bb:cc:dd", "not-a-mac",
	} {
		t.Run(address, func(t *testing.T) {
			if err := validateEndpointMAC(address); err == nil {
				t.Fatalf("unsafe or malformed MAC address %q was accepted", address)
			}
		})
	}
	if err := validateEndpointMAC("02:aa:bb:cc:dd:ee"); err != nil {
		t.Fatalf("valid locally administered unicast MAC rejected: %v", err)
	}
	if err := validateEndpointMAC("00:11:22:33:44:55"); err != nil {
		t.Fatalf("valid globally administered unicast MAC rejected: %v", err)
	}
}

func TestNetworkIPRangeBoundsAutomaticAndStaticAllocations(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	created, err := CreateWithRunner(context.Background(), "bounded-pool", CreateOptions{
		Subnet: "172.30.50.0/24", IPRange: "172.30.50.128/25", Gateway: "172.30.50.1",
		AuxiliaryAddresses: map[string]string{"reserved": "172.30.50.129", "reserved-second": "172.30.50.130"},
	}, &fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if created.IPRange != "172.30.50.128/25" {
		t.Fatalf("persisted IPAM range=%q", created.IPRange)
	}
	automatic, err := JoinWithRunner(context.Background(), "bounded-pool", 2501, "container-auto", &fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if automatic != "172.30.50.131" {
		t.Fatalf("automatic address=%q, want first address in configured pool", automatic)
	}
	if _, err := JoinWithRunnerOptions(context.Background(), "bounded-pool", 2502, "container-static-reserved", &fakeRunner{},
		EndpointOptions{IPv4Address: "172.30.50.129"}); err == nil {
		t.Fatal("static address reserved by IPAM auxiliary configuration was accepted")
	}
	if _, err := JoinWithRunnerOptions(context.Background(), "bounded-pool", 2504, "container-static-outside", &fakeRunner{},
		EndpointOptions{IPv4Address: "172.30.50.20"}); err == nil {
		t.Fatal("static address inside subnet but outside IPRange was accepted")
	}
	static, err := JoinWithRunnerOptions(context.Background(), "bounded-pool", 2503, "container-static-inside", &fakeRunner{},
		EndpointOptions{IPv4Address: "172.30.50.140"})
	if err != nil {
		t.Fatalf("static address inside IPRange rejected: %v", err)
	}
	if static != "172.30.50.140" {
		t.Fatalf("static address=%q", static)
	}
	summary, err := Inspect("bounded-pool")
	if err != nil {
		t.Fatal(err)
	}
	if summary.IPRange != "172.30.50.128/25" || summary.ContainerCount != 2 ||
		summary.AuxiliaryAddresses["reserved"] != "172.30.50.129" {
		t.Fatalf("bounded pool summary = %+v", summary)
	}
}

func TestCreateRejectsInvalidOrOutOfSubnetIPRange(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	for _, value := range []string{
		"172.30.51.0/23", "172.30.52.0/24", "172.30.51.128/31",
		"172.30.51.128", "not-a-prefix", "172.030.051.0/25",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := CreateWithRunner(context.Background(), "invalid-pool", CreateOptions{
				Subnet: "172.30.51.0/24", IPRange: value, Gateway: "172.30.51.1",
			}, &fakeRunner{})
			if err == nil {
				t.Fatalf("invalid IPAM range %q was accepted", value)
			}
		})
	}
}

func TestStoredIPAMLeasesCannotOverlapAuxiliaryReservations(t *testing.T) {
	network := Network{
		ID:                 "0123456789abcdef",
		Name:               "reserved-overlap",
		Driver:             "bridge",
		Bridge:             "br-reserved0",
		Subnet:             "192.0.2.0/24",
		Gateway:            "192.0.2.1",
		Allocations:        map[string]string{"container-lease": "192.0.2.10"},
		AuxiliaryAddresses: map[string]string{"database": "192.0.2.10"},
	}
	if err := validateStoredNetworks([]Network{network}); err == nil {
		t.Fatal("persisted IPAM lease overlapping an auxiliary reservation was accepted")
	}
}

func TestCreateRejectsInvalidAuxiliaryIPAMAddresses(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	for name, addresses := range map[string]map[string]string{
		"outside subnet":    {"reserved": "192.0.2.10"},
		"network address":   {"reserved": "172.30.53.0"},
		"broadcast address": {"reserved": "172.30.53.255"},
		"gateway":           {"reserved": "172.30.53.1"},
		"duplicate values":  {"first": "172.30.53.10", "second": "172.30.53.10"},
		"invalid name":      {"not valid": "172.30.53.10"},
		"invalid address":   {"reserved": "not-an-ip"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CreateWithRunner(context.Background(), "invalid-aux", CreateOptions{
				Subnet: "172.30.53.0/24", Gateway: "172.30.53.1",
				AuxiliaryAddresses: addresses,
			}, &fakeRunner{})
			if err == nil {
				t.Fatalf("invalid auxiliary IPAM addresses were accepted: %#v", addresses)
			}
		})
	}
}

func TestAuxiliaryAddressReservationsAreBoundedAndDeepCopied(t *testing.T) {
	t.Setenv("BOXR_NETWORK_DIR", filepath.Join(t.TempDir(), "networks"))
	reserved := map[string]string{"database": "172.30.54.10"}
	created, err := CreateWithRunner(context.Background(), "aux-copy", CreateOptions{
		Subnet: "172.30.54.0/24", Gateway: "172.30.54.1",
		AuxiliaryAddresses: reserved,
	}, &fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	created.AuxiliaryAddresses["database"] = "172.30.54.11"
	details, err := InspectDetails("aux-copy")
	if err != nil {
		t.Fatal(err)
	}
	if details.AuxiliaryAddresses["database"] != "172.30.54.10" {
		t.Fatalf("caller mutation changed persisted auxiliary map: %#v", details.AuxiliaryAddresses)
	}
	details.AuxiliaryAddresses["database"] = "172.30.54.12"
	summary, err := Inspect("aux-copy")
	if err != nil {
		t.Fatal(err)
	}
	if summary.AuxiliaryAddresses["database"] != "172.30.54.10" {
		t.Fatalf("InspectDetails mutation changed stored summary: %#v", summary.AuxiliaryAddresses)
	}
	summary.AuxiliaryAddresses["database"] = "172.30.54.13"
	next, err := InspectDetails("aux-copy")
	if err != nil {
		t.Fatal(err)
	}
	if next.AuxiliaryAddresses["database"] != "172.30.54.10" {
		t.Fatalf("summary mutation changed persisted IPAM map: %#v", next.AuxiliaryAddresses)
	}

	tooMany := make(map[string]string, maxAuxiliaryAddresses+1)
	for index := 0; index <= maxAuxiliaryAddresses; index++ {
		tooMany[fmt.Sprintf("reserved-%03d", index)] = fmt.Sprintf("172.30.55.%d", index+2)
	}
	if _, err := CreateWithRunner(context.Background(), "aux-limit", CreateOptions{
		Subnet: "172.30.55.0/24", Gateway: "172.30.55.1",
		AuxiliaryAddresses: tooMany,
	}, &fakeRunner{}); err == nil {
		t.Fatal("network with more than the auxiliary IPAM limit was accepted")
	}
}
