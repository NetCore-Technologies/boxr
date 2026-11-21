//go:build linux

package container

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeIdentityFiles(t *testing.T, root, passwd, group string) {
	t.Helper()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "passwd"), []byte(passwd), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "group"), []byte(group), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveContainerUserAtResolvesNamedUserAndSupplementaryGroups(t *testing.T) {
	root := t.TempDir()
	writeIdentityFiles(t, root,
		"root:x:0:0:root:/root:/bin/sh\nalice:x:1001:100:Alice:/home/alice:/bin/sh\n",
		"root:x:0:\nusers:x:100:\nwheel:x:10:alice,bob\nshared:x:200:alice\nrepeat:x:200:alice\n",
	)
	got, err := resolveContainerUserAt(root, "alice:users")
	if err != nil {
		t.Fatal(err)
	}
	if got.uid != 1001 || got.gid != 100 || got.home != "/home/alice" || got.userName != "alice" {
		t.Fatalf("resolved identity = %#v", got)
	}
	if !reflect.DeepEqual(got.supplementary, []int{10, 200}) {
		t.Fatalf("supplementary groups = %v, want [10 200]", got.supplementary)
	}
}

func TestResolveContainerUserAtSupportsNumericIDsWithoutPasswd(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveContainerUserAt(root, "4242")
	if err != nil {
		t.Fatal(err)
	}
	if got.uid != 4242 || got.gid != 4242 {
		t.Fatalf("numeric identity = %#v, want uid/gid 4242", got)
	}
}

func TestResolveContainerUserAtSupportsExplicitNumericGroup(t *testing.T) {
	root := t.TempDir()
	writeIdentityFiles(t, root, "alice:x:1001:100:Alice:/home/alice:/bin/sh\n", "wheel:x:10:alice\n")
	got, err := resolveContainerUserAt(root, "alice:300")
	if err != nil {
		t.Fatal(err)
	}
	if got.uid != 1001 || got.gid != 300 || !reflect.DeepEqual(got.supplementary, []int{10}) {
		t.Fatalf("resolved identity = %#v", got)
	}
}

func TestResolveContainerUserAtRejectsUnknownNamesAndMalformedIDs(t *testing.T) {
	root := t.TempDir()
	writeIdentityFiles(t, root, "root:x:0:0:root:/root:/bin/sh\n", "root:x:0:\n")
	for _, spec := range []string{"missing", "root:missing", "alice:999999999999", "root:0:extra"} {
		if _, err := resolveContainerUserAt(root, spec); err == nil {
			t.Errorf("resolveContainerUserAt(%q) succeeded, want error", spec)
		}
	}
}

func TestIdentityDatabaseIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxIdentityDatabaseBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readIdentityDatabase(path); err == nil {
		t.Fatal("oversized identity database was accepted")
	}
}

func TestOptionsValidateContainerUserSpecAndUserNamespaceMapping(t *testing.T) {
	for _, spec := range []string{"alice:group:extra", "bad user", ": ", "alice/other", "alice\\other"} {
		if err := (Options{User: spec}).Validate(); err == nil {
			t.Errorf("Options{User:%q}.Validate() succeeded", spec)
		}
	}
	for _, spec := range []string{"", "root", "0", "root:root", "0:0", ":0"} {
		if err := (Options{User: spec, UserNamespace: true}).Validate(); err != nil {
			t.Errorf("root user spec %q rejected in user namespace: %v", spec, err)
		}
	}
	if err := (Options{User: "1000:1000", UserNamespace: true}).Validate(); err == nil {
		t.Fatal("non-root user was accepted by the single-ID user namespace mapping")
	}
}
