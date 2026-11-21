//go:build linux

package engineapi

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeProcFixture(t *testing.T, root string, pid, ppid int, comm string, nsPID int, uid uint32, start, user, system uint64, children string, command string) {
	t.Helper()
	base := filepath.Join(root, strconv.Itoa(pid))
	for _, dir := range []string{base, filepath.Join(base, "task", strconv.Itoa(pid)), filepath.Join(base, "fd")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fields := make([]string, 20)
	fields[0] = "S"
	fields[1] = strconv.Itoa(ppid)
	fields[4] = "0"
	fields[11] = strconv.FormatUint(user, 10)
	fields[12] = strconv.FormatUint(system, 10)
	fields[19] = strconv.FormatUint(start, 10)
	if err := os.WriteFile(filepath.Join(base, "stat"), []byte(strconv.Itoa(pid)+" ("+comm+") "+strings.Join(fields, " ")), 0600); err != nil {
		t.Fatal(err)
	}
	status := "Name:\t"+comm+"\nUid:\t"+strconv.FormatUint(uint64(uid),10)+" "+strconv.FormatUint(uint64(uid),10)+" "+strconv.FormatUint(uint64(uid),10)+" "+strconv.FormatUint(uint64(uid),10)+"\nNSpid:\t"+strconv.Itoa(nsPID)+"\n"
	if err := os.WriteFile(filepath.Join(base, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cmdline"), []byte(command), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "task", strconv.Itoa(pid), "children"), []byte(children), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadContainerProcessTreeMapsPIDsAndBoundsCPUTime(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3 4\nbtime 1700000000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeProcFixture(t, root, 5000, 4000, "boxr init ) name", 1, 0, 200, 100, 50, "5001", "boxr\x00init\x00")
	writeProcFixture(t, root, 5001, 5000, "worker", 2, 1001, 300, 100, 50, "", "worker\x00--serve\x00")

	processes, bootTime, err := readContainerProcessTree(root, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if bootTime != 1700000000 {
		t.Fatalf("boot time=%d, want 1700000000", bootTime)
	}
	if len(processes) != 2 {
		t.Fatalf("process count=%d, want 2: %#v", len(processes), processes)
	}
	if processes[0].pid != 1 || processes[1].pid != 2 {
		t.Fatalf("namespace PIDs = [%d %d], want [1 2]", processes[0].pid, processes[1].pid)
	}
	if processes[1].ppid != 5000 || processes[1].uid != 1001 || processes[1].command != "worker --serve" {
		t.Fatalf("worker process was parsed incorrectly: %#v", processes[1])
	}
	if got := formatProcessCPUTime(processes[0].cpuTime); got != "00:00:01" {
		t.Fatalf("CPU time=%q, want 00:00:01", got)
	}
}

func TestParseDockerTopProcStatHandlesClosingParenthesisInCommand(t *testing.T) {
	fields := make([]string, 20)
	fields[0], fields[1], fields[11], fields[12], fields[19] = "S", "7", "12", "8", "99"
	got, err := parseDockerTopProcStat("100 (name ) with ) parens) "+strings.Join(fields, " "))
	if err != nil {
		t.Fatal(err)
	}
	if got.comm != "name ) with ) parens" || got.ppid != 7 || got.cpuTime != 20 || got.start != 99 {
		t.Fatalf("parsed proc stat = %#v", got)
	}
}

func TestReadContainerProcessTreeRejectsOversizedProcessTree(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1700000000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeProcFixture(t, root, 7000, 0, "init", 1, 0, 10, 0, 0, "7001", "")
	for index := 0; index < maxContainerTopProcesses; index++ {
		pid := 8000 + index
		writeProcFixture(t, root, pid, 7000, "child", index+2, 0, 11, 0, 0, "", "")
	}
	if err := os.WriteFile(filepath.Join(root, "7000", "task", "7000", "children"), []byte(func() string {
		parts := make([]string, maxContainerTopProcesses)
		for index := range parts {
			parts[index] = strconv.Itoa(8000 + index)
		}
		return strings.Join(parts, " ")
	}()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readContainerProcessTree(root, 7000); err == nil {
		t.Fatal("oversized process tree was accepted")
	}
}
