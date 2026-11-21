//go:build linux

package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetCgroupFrozenAtWaitsForKernelState(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	const pid = 5157
	directory := filepath.Join(root, "boxr", "5157")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		filepath.Join(directory, "cgroup.freeze"): "0",
		filepath.Join(directory, "cgroup.events"): "populated 1\nfrozen 0\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			value, err := os.ReadFile(filepath.Join(directory, "cgroup.freeze"))
			if err == nil && strings.TrimSpace(string(value)) == "1" {
				_ = os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte("populated 1\nfrozen 1\n"), 0644)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := setCgroupFrozenAt(root, pid, true, 500*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("setCgroupFrozenAt(frozen): %v", err)
	}
	<-done
	got, err := os.ReadFile(filepath.Join(directory, "cgroup.freeze"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("cgroup.freeze = %q, want 1", got)
	}
}

func TestSetCgroupFrozenAtRejectsMissingKernelConfirmation(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	const pid = 5158
	directory := filepath.Join(root, "boxr", "5158")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		filepath.Join(directory, "cgroup.freeze"): "0",
		filepath.Join(directory, "cgroup.events"): "populated 1\nfrozen 0\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setCgroupFrozenAt(root, pid, true, 15*time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("freezer transition succeeded without kernel confirmation")
	}
	restored, err := os.ReadFile(filepath.Join(directory, "cgroup.freeze"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(restored)) != "0" {
		t.Fatalf("failed freezer transition did not restore the prior state: %q", restored)
	}
}

func TestSetCgroupFrozenAtRejectsSymlinkedControlFiles(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	const pid = 5159
	directory := filepath.Join(root, "boxr", "5159")
	target := filepath.Join(root, "outside")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("0"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "cgroup.freeze")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte("frozen 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := setCgroupFrozenAt(root, pid, true, time.Second, time.Millisecond); err == nil {
		t.Fatal("symlinked cgroup.freeze was accepted")
	}
}

func TestParseCgroupFrozenEvent(t *testing.T) {
	for _, test := range []struct {
		name      string
		data      string
		want      bool
		found     bool
		wantError bool
	}{
		{name: "frozen", data: "populated 1\nfrozen 1\n", want: true, found: true},
		{name: "thawed", data: "populated 1\nfrozen 0\n", found: true},
		{name: "not reported yet", data: "populated 1\n"},
		{name: "invalid", data: "frozen 2\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, found, err := parseCgroupFrozenEvent([]byte(test.data))
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, wantError=%t", err, test.wantError)
			}
			if err == nil && (got != test.want || found != test.found) {
				t.Fatalf("parseCgroupFrozenEvent() = (%t,%t), want (%t,%t)", got, found, test.want, test.found)
			}
		})
	}
}

func TestSetCgroupFrozenAtWaitsForThawConfirmation(t *testing.T) {
	root := newExtendedCgroupFixture(t)
	const pid = 5160
	directory := filepath.Join(root, "boxr", "5160")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		filepath.Join(directory, "cgroup.freeze"): "1",
		filepath.Join(directory, "cgroup.events"): "populated 1\nfrozen 1\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			value, err := os.ReadFile(filepath.Join(directory, "cgroup.freeze"))
			if err == nil && strings.TrimSpace(string(value)) == "0" {
				_ = os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte("populated 1\nfrozen 0\n"), 0644)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := setCgroupFrozenAt(root, pid, false, 500*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("setCgroupFrozenAt(thawed): %v", err)
	}
	<-done
}
