package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls       []string
	failAt      int
	failMessage string
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
	return nil
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
		"iptables -N " + natChain,
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
	if len(runner.calls) != 9 {
		t.Fatalf("join command count = %d, want 9: %v", len(runner.calls), runner.calls)
	}
	if !strings.Contains(runner.calls[0], "link add vbx") ||
		!strings.Contains(runner.calls[3], "netns 1234") ||
		!strings.Contains(runner.calls[4], "nsenter -t 1234 -n -- ip link set dev cbx") ||
		!strings.Contains(runner.calls[8], "route add default via "+network.Gateway) {
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
	leaveCalls := strings.Join(runner.calls[len(runner.calls)-7:], "\n")
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
	for _, network := range []struct{name, subnet string}{
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
	// Nine link/namespace operations succeed. Fail the PREROUTING rule after
	// enabling forwarding and then verify every partial rule and lease is cleaned.
	runner := &fakeRunner{failAt: 11}
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
		"trailing JSON": `{"version":1,"networks":[]} {}`,
		"unknown field": `{"version":1,"networks":[],"unexpected":true}`,
		"invalid bridge name": `{"version":1,"networks":[{"id":"0123456789abcdef","name":"frontend","driver":"bridge","bridge":"br/name","subnet":"172.30.0.0/24","gateway":"172.30.0.1","nat":false}]}`,
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
	failing := &fakeRunner{failAt: 1, failMessage: "firewall backend unavailable"}
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

