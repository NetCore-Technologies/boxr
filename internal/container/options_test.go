package container

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeExposedPortsExpandsAndCanonicalizes(t *testing.T) {
	got, err := NormalizeExposedPorts([]string{"80", "443/tcp", "53/UDP", "8000-8002/udp", "80/tcp"})
	if err != nil {
		t.Fatalf("NormalizeExposedPorts(): %v", err)
	}
	want := []string{"443/tcp", "53/udp", "80/tcp", "8000/udp", "8001/udp", "8002/udp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeExposedPorts() = %#v, want %#v", got, want)
	}
}

func TestNormalizeExposedPortsRejectsUnsafeAndUnboundedValues(t *testing.T) {
	for _, value := range []string{
		"", "0/tcp", "65536/tcp", "1-257/tcp", "2-1/tcp", "1--2/tcp",
		"80/icmp", "80/tcp/udp", " 80", "80 ", "80\n",
	} {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			if _, err := NormalizeExposedPorts([]string{value}); err == nil {
				t.Fatalf("NormalizeExposedPorts(%q) unexpectedly succeeded", value)
			}
		})
	}
	if err := (Options{ExposedPorts: []string{"0/tcp"}}).Validate(); err == nil {
		t.Fatal("Options.Validate accepted an invalid exposed port")
	}
	if _, err := NormalizeExposedPorts([]string{"1/tcp", "2-257/tcp"}); err == nil {
		t.Fatal("NormalizeExposedPorts accepted more than 256 expanded ports")
	}
}

func TestMergeEnvironmentAppliesOverridesWithoutDuplicateKeys(t *testing.T) {
	base := []string{"PATH=/usr/bin", "APP_ENV=production", "EMPTY=", "APP_ENV=staging"}
	overrides := []string{"APP_ENV=development", "NEW_SETTING=value"}
	got := mergeEnvironment(base, overrides)
	want := []string{"PATH=/usr/bin", "APP_ENV=development", "EMPTY=", "NEW_SETTING=value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironment() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvironmentIgnoresMalformedBaseEntries(t *testing.T) {
	got := mergeEnvironment([]string{"=invalid", "NO_EQUALS", "GOOD=value"}, nil)
	want := []string{"GOOD=value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironment() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvironmentDoesNotMutateInputs(t *testing.T) {
	base := []string{"A=one"}
	overrides := []string{"A=two"}
	_ = mergeEnvironment(base, overrides)
	if !reflect.DeepEqual(base, []string{"A=one"}) || !reflect.DeepEqual(overrides, []string{"A=two"}) {
		t.Fatalf("mergeEnvironment() mutated inputs: base=%#v overrides=%#v", base, overrides)
	}
}

func TestOptionsValidateRootFSRequirements(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		valid   bool
	}{
		{name: "default host mode", options: Options{NetworkMode: "host"}, valid: true},
		{name: "private network", options: Options{NetworkMode: "none"}, valid: true},
		{name: "unsupported network", options: Options{NetworkMode: "bridge"}},
		{name: "rootfs relative", options: Options{RootFS: "./rootfs"}},
		{name: "host root rejected", options: Options{RootFS: "/"}},
		{name: "read only requires rootfs", options: Options{ReadOnlyRootFS: true}},
		{name: "proc requires rootfs", options: Options{MountProc: true}},
		{name: "absolute workdir required in rootfs", options: Options{RootFS: "/tmp/rootfs", Workdir: "tmp"}},
		{name: "valid rootfs", options: Options{RootFS: "/tmp/rootfs", Workdir: "/tmp", MountProc: true}, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.options.Validate()
			if test.valid && err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}
func TestOptionsValidateResourceWeightsAndMountTargets(t *testing.T) {
	valid := Options{CPUWeight: 10000, IOWeight: 1, PidsLimit: 1, RootFS: "/tmp/rootfs", BindMounts: []BindMount{{Source: "/etc/hosts", Target: "/etc/hosts", ReadOnly: true}}, TmpfsMounts: []TmpfsMount{{Target: "/tmp", SizeBytes: 64 << 20, Mode: 01777}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	invalid := []Options{
		{CPUWeight: 10001}, {CPUWeight: -1}, {IOWeight: 10001}, {PidsLimit: -1},
		{RootFS: "/tmp/rootfs", BindMounts: []BindMount{{Source: "relative", Target: "/data"}}},
		{RootFS: "/tmp/rootfs", BindMounts: []BindMount{{Source: "/data", Target: "/../etc"}}},
		{RootFS: "/tmp/rootfs", TmpfsMounts: []TmpfsMount{{Target: "relative"}}},
		{DropCapabilities: []string{"DOES_NOT_EXIST"}},
	}
	for i, options := range invalid {
		if err := options.Validate(); err == nil {
			t.Errorf("invalid options %d unexpectedly succeeded", i)
		}
	}
}

func TestCapabilityNumberAndNormalization(t *testing.T) {
	for input, want := range map[string]int{"CHOWN": 0, "CAP_SYS_ADMIN": 21, "net_raw": 13, "checkpoint_restore": 40} {
		got, ok := capabilityNumber(input)
		if !ok || got != want {
			t.Errorf("capabilityNumber(%q) = %d, %t; want %d, true", input, got, ok, want)
		}
	}
	if _, ok := capabilityNumber("not-a-capability"); ok {
		t.Fatal("unknown capability was accepted")
	}
	got, all := normalizedCapabilities([]string{"CAP_NET_RAW", "CAP_NET_ADMIN", "NET_RAW"})
	if all || len(got) != 2 || got[0] != 12 || got[1] != 13 {
		t.Fatalf("normalized capabilities = %v, all=%t", got, all)
	}
	got, all = normalizedCapabilities([]string{"ALL"})
	if !all || len(got) != 41 || got[0] != 0 || got[len(got)-1] != 40 {
		t.Fatalf("ALL capabilities = %v, all=%t", got, all)
	}
}

func TestPrepareRootPathCreatesContainedDirectoryAndFile(t *testing.T) {
	root := t.TempDir()
	path, err := PrepareRootPath(root, "/etc/hosts", false)
	if err != nil {
		t.Fatalf("PrepareRootPath(): %v", err)
	}
	if path != filepath.Join(root, "etc", "hosts") {
		t.Fatalf("path = %q", path)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("expected file mountpoint, info=%v err=%v", info, err)
	}
	directory, err := PrepareRootPath(root, "/var/tmp", true)
	if err != nil {
		t.Fatalf("PrepareRootPath directory: %v", err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("expected directory mountpoint, info=%v err=%v", info, err)
	}
}

func TestPrepareRootPathRejectsSymlinkEscapeAndTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	for _, target := range []string{"../outside", "/escape/child"} {
		if _, err := PrepareRootPath(root, target, true); err == nil {
			t.Errorf("PrepareRootPath(%q) unexpectedly succeeded", target)
		}
	}
}

func TestParseTmpfsSize(t *testing.T) {
	for input, want := range map[string]int64{"64m": 64 << 20, "1GiB": 1 << 30, "4096": 4096, "2k": 2048} {
		got, err := ParseTmpfsSize(input)
		if err != nil || got != want {
			t.Errorf("ParseTmpfsSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "1.5g", "999999999999999999999g"} {
		if _, err := ParseTmpfsSize(input); input == "" {
			if err != nil {
				t.Errorf("empty size should mean no explicit cap: %v", err)
			}
		} else if err == nil {
			t.Errorf("ParseTmpfsSize(%q) unexpectedly succeeded", input)
		}
	}
}

func TestOverlayOptionsRequireSeparateSafeDirectories(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "boxr-rootfs")
	valid := filepath.Join(string(filepath.Separator), "tmp", "boxr-overlay")
	cases := []struct {
		name    string
		options Options
		wantErr string
	}{
		{name: "valid", options: Options{RootFS: root, OverlayDir: valid}},
		{name: "requires rootfs", options: Options{OverlayDir: valid}, wantErr: "--rootfs is required"},
		{name: "inside lower", options: Options{RootFS: root, OverlayDir: filepath.Join(root, "overlay")}, wantErr: "separate directory trees"},
		{name: "ancestor of lower", options: Options{RootFS: root, OverlayDir: "/tmp"}, wantErr: "separate directory trees"},
		{name: "root overlay", options: Options{RootFS: root, OverlayDir: "/"}, wantErr: "absolute directory"},
		{name: "comma option delimiter", options: Options{RootFS: "/tmp/root,one", OverlayDir: valid}, wantErr: "cannot contain commas"},
		{name: "colon option delimiter", options: Options{RootFS: root, OverlayDir: "/tmp/overlay:two"}, wantErr: "cannot contain commas"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.options.Validate()
			if test.wantErr == "" && err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestSeccompProfileValidation(t *testing.T) {
	if err := (Options{SeccompProfile: "baseline"}).Validate(); err != nil {
		t.Fatalf("baseline profile rejected: %v", err)
	}
	if err := (Options{SeccompProfile: "unrestricted"}).Validate(); err == nil {
		t.Fatal("unknown profile unexpectedly accepted")
	}
}

func TestApplySecurityProfiles(t *testing.T) {
	tests := []struct {
		name         string
		options      Options
		wantErr      string
		wantNetwork  string
		wantUserNS   bool
		wantNNP      bool
		wantSeccomp  string
		wantReadOnly bool
		wantCaps     []string
	}{
		{name: "default", options: Options{SecurityProfile: "default", NetworkMode: "host"}, wantNetwork: "host"},
		{name: "baseline", options: Options{SecurityProfile: "baseline", DropCapabilities: []string{"NET_RAW", "CHOWN"}}, wantNetwork: "", wantNNP: true, wantSeccomp: "baseline", wantCaps: []string{"NET_RAW", "CHOWN"}},
		{name: "restricted", options: Options{SecurityProfile: "restricted", NetworkMode: "host", DropCapabilities: []string{"CHOWN"}}, wantNetwork: "none", wantUserNS: true, wantNNP: true, wantSeccomp: "baseline", wantCaps: []string{"CHOWN", "SYS_ADMIN", "SYS_MODULE", "SYS_RAWIO", "SYS_PTRACE", "NET_ADMIN", "NET_RAW", "SYS_BOOT", "SYSLOG", "MKNOD", "DAC_OVERRIDE", "DAC_READ_SEARCH"}},
		{name: "untrusted requires rootfs", options: Options{SecurityProfile: "untrusted"}, wantErr: "requires --rootfs"},
		{name: "untrusted", options: Options{SecurityProfile: "untrusted", RootFS: "/tmp/rootfs", NetworkMode: "host"}, wantNetwork: "none", wantUserNS: true, wantNNP: true, wantSeccomp: "baseline", wantReadOnly: true, wantCaps: []string{"ALL"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.options
			err := got.ApplySecurityProfile()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ApplySecurityProfile() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplySecurityProfile(): %v", err)
			}
			if got.NetworkMode != test.wantNetwork || got.UserNamespace != test.wantUserNS || got.NoNewPrivileges != test.wantNNP || got.SeccompProfile != test.wantSeccomp || got.ReadOnlyRootFS != test.wantReadOnly {
				t.Fatalf("unexpected profile options: %#v", got)
			}
			if !reflect.DeepEqual(got.DropCapabilities, test.wantCaps) {
				t.Fatalf("capabilities = %#v, want %#v", got.DropCapabilities, test.wantCaps)
			}
		})
	}
}

func TestApplySecurityProfileRejectsUnknownProfile(t *testing.T) {
	options := Options{SecurityProfile: "super-secure"}
	if err := options.ApplySecurityProfile(); err == nil {
		t.Fatal("unknown security profile unexpectedly accepted")
	}
}

func TestApplyResourceProfilesFillOnlyUnsetLimits(t *testing.T) {
	tests := []struct {
		name      string
		memory    int64
		cpus      float64
		pids      int64
		cpuWeight int
		ioWeight  int
	}{
		{name: "micro", memory: 64 << 20, cpus: 0.25, pids: 64, cpuWeight: 50, ioWeight: 50},
		{name: "small", memory: 256 << 20, cpus: 0.5, pids: 128, cpuWeight: 100, ioWeight: 100},
		{name: "medium", memory: 512 << 20, cpus: 1, pids: 256, cpuWeight: 100, ioWeight: 100},
		{name: "large", memory: 1 << 30, cpus: 2, pids: 512, cpuWeight: 200, ioWeight: 200},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := Options{ResourceProfile: test.name}
			if err := options.ApplyResourceProfile(); err != nil {
				t.Fatal(err)
			}
			if options.MemoryLimitBytes != test.memory || options.CPUs != test.cpus || options.PidsLimit != test.pids || options.CPUWeight != test.cpuWeight || options.IOWeight != test.ioWeight {
				t.Fatalf("unexpected resource profile: %#v", options)
			}
		})
	}
}

func TestApplyResourceProfilePreservesExplicitLimits(t *testing.T) {
	options := Options{ResourceProfile: "small", MemoryLimitBytes: 128 << 20, CPUs: 1.5, PidsLimit: 300, CPUWeight: 400}
	if err := options.ApplyResourceProfile(); err != nil {
		t.Fatal(err)
	}
	if options.MemoryLimitBytes != 128<<20 || options.CPUs != 1.5 || options.PidsLimit != 300 || options.CPUWeight != 400 || options.IOWeight != 100 {
		t.Fatalf("resource profile overwrote explicit settings or failed to fill unset value: %#v", options)
	}
}

func TestApplyResourceProfileRejectsUnknownAndNil(t *testing.T) {
	options := Options{ResourceProfile: "unlimited"}
	if err := options.ApplyResourceProfile(); err == nil {
		t.Fatal("unknown resource profile unexpectedly accepted")
	}
	var nilOptions *Options
	if err := nilOptions.ApplyResourceProfile(); err == nil {
		t.Fatal("nil options unexpectedly accepted")
	}
}

func TestOptionsValidateManagedBridgeNetworking(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		valid   bool
	}{
		{name: "bridge with named network", options: Options{NetworkMode: "bridge", NetworkName: "frontend"}, valid: true},
		{name: "bridge with dotted name", options: Options{NetworkMode: "bridge", NetworkName: "api.v2"}, valid: true},
		{name: "bridge without network name", options: Options{NetworkMode: "bridge"}},
		{name: "bridge with uppercase network name", options: Options{NetworkMode: "bridge", NetworkName: "Frontend"}},
		{name: "bridge with traversal name", options: Options{NetworkMode: "bridge", NetworkName: "../frontend"}},
		{name: "bridge name with host mode", options: Options{NetworkMode: "host", NetworkName: "frontend"}},
		{name: "bridge name with none mode", options: Options{NetworkMode: "none", NetworkName: "frontend"}},
		{name: "unknown mode", options: Options{NetworkMode: "overlay", NetworkName: "frontend"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.options.Validate()
			if test.valid && err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestPortMappingNormalizeUsesSafeIPv4AndProtocolDefaults(t *testing.T) {
	normalized, err := (PortMapping{HostPort: 8080, ContainerPort: 80}).Normalize()
	if err != nil {
		t.Fatalf("Normalize(): %v", err)
	}
	want := PortMapping{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
	if normalized != want {
		t.Fatalf("normalized mapping = %+v, want %+v", normalized, want)
	}
	udp, err := (PortMapping{HostIP: "127.0.0.1", HostPort: 5353, ContainerPort: 53, Protocol: "UDP"}).Normalize()
	if err != nil {
		t.Fatalf("Normalize(UDP): %v", err)
	}
	if udp.Protocol != "udp" || udp.HostIP != "127.0.0.1" {
		t.Fatalf("normalized UDP mapping = %+v", udp)
	}
}

func TestPortMappingNormalizeRejectsInvalidMappings(t *testing.T) {
	tests := map[string]PortMapping{
		"zero host port":       {HostPort: 0, ContainerPort: 80},
		"zero container port":  {HostPort: 8080, ContainerPort: 0},
		"ipv6 host address":    {HostIP: "::1", HostPort: 8080, ContainerPort: 80},
		"noncanonical host ip": {HostIP: "127.000.0.1", HostPort: 8080, ContainerPort: 80},
		"unknown protocol":     {HostPort: 8080, ContainerPort: 80, Protocol: "sctp"},
	}
	for name, mapping := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := mapping.Normalize(); err == nil {
				t.Fatal("invalid port mapping unexpectedly normalized")
			}
		})
	}
}

func TestOptionsValidatePublishedPortsOnlyForBridgeAndRejectConflicts(t *testing.T) {
	valid := Options{
		NetworkMode: "bridge",
		NetworkName: "frontend",
		PublishedPorts: []PortMapping{
			{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80},
			{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid published ports): %v", err)
	}
	tests := map[string]Options{
		"host mode": {
			NetworkMode: "host", PublishedPorts: []PortMapping{{HostPort: 8080, ContainerPort: 80}},
		},
		"none mode": {
			NetworkMode: "none", PublishedPorts: []PortMapping{{HostPort: 8080, ContainerPort: 80}},
		},
		"wildcard conflicts with specific host": {
			NetworkMode: "bridge", NetworkName: "frontend",
			PublishedPorts: []PortMapping{
				{HostPort: 8080, ContainerPort: 80},
				{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 8080},
			},
		},
		"same endpoint duplicate": {
			NetworkMode: "bridge", NetworkName: "frontend",
			PublishedPorts: []PortMapping{
				{HostPort: 8080, ContainerPort: 80},
				{HostPort: 8080, ContainerPort: 8080},
			},
		},
		"mapping count exceeded": {
			NetworkMode: "bridge", NetworkName: "frontend",
			PublishedPorts: make([]PortMapping, 129),
		},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			if err := options.Validate(); err == nil {
				t.Fatal("invalid published port configuration unexpectedly validated")
			}
		})
	}
}

func TestOptionsValidateRestartPoliciesAndBudgets(t *testing.T) {
	valid := []Options{
		{RestartPolicy: "no"},
		{RestartPolicy: "always"},
		{RestartPolicy: "unless-stopped", RestartDelayMS: 5000},
		{RestartPolicy: "on-failure", RestartMaxRetries: 3, RestartDelayMS: 250},
		{RestartPolicy: "on-failure", RestartMaxRetries: 0},
	}
	for _, options := range valid {
		if err := options.Validate(); err != nil {
			t.Errorf("Validate(%+v): %v", options, err)
		}
	}
	invalid := map[string]Options{
		"unknown policy":         {RestartPolicy: "sometimes"},
		"negative retries":       {RestartPolicy: "on-failure", RestartMaxRetries: -1},
		"too many retries":       {RestartPolicy: "on-failure", RestartMaxRetries: 1001},
		"retries with always":    {RestartPolicy: "always", RestartMaxRetries: 1},
		"delay below minimum":    {RestartPolicy: "always", RestartDelayMS: 99},
		"delay above maximum":    {RestartPolicy: "always", RestartDelayMS: 300001},
		"delay without policy":   {RestartPolicy: "no", RestartDelayMS: 1000},
		"retries without policy": {RestartPolicy: "no", RestartMaxRetries: 2},
	}
	for name, options := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := options.Validate(); err == nil {
				t.Fatalf("invalid restart settings unexpectedly validated: %+v", options)
			}
		})
	}
}

func TestNormalizeStopSignalCanonicalizesNamesAndNumbers(t *testing.T) {
	for input, want := range map[string]string{
		"SIGTERM": "SIGTERM",
		"term":    "SIGTERM",
		"SIGQUIT": "SIGQUIT",
		"2":       "SIGINT",
		"15":      "SIGTERM",
	} {
		got, err := NormalizeStopSignal(input)
		if err != nil || got != want {
			t.Errorf("NormalizeStopSignal(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if got, err := NormalizeStopSignal(""); err != nil || got != "" {
		t.Fatalf("empty stop signal = %q, %v", got, err)
	}
}

func TestNormalizeStopSignalRejectsUnsupportedAndUnsafeValues(t *testing.T) {
	for _, input := range []string{"0", "11", "SIGFOO", "SIGTERM KILL", " TERM", "TERM ", "TERM\n"} {
		if _, err := NormalizeStopSignal(input); err == nil {
			t.Errorf("NormalizeStopSignal(%q) unexpectedly succeeded", input)
		}
	}
	if err := (Options{StopSignal: "SIGFOO"}).Validate(); err == nil {
		t.Fatal("Options.Validate accepted an unsupported stop signal")
	}
}
