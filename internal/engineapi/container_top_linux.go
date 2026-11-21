//go:build linux

package engineapi

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/state"
)

const (
	maxContainerTopProcesses  = 4096
	maxContainerProcFileBytes = 1 << 20
	procClockTicksPerSecond   = 100
)

type dockerTopProcess struct {
	hostPID    int
	threadPIDs []int
	pid        int
	ppid       int
	uid        uint32
	comm       string
	command    string
	tty        string
	cpuTime    uint64
	start      uint64
}

func (s *Server) handleContainerTop(writer http.ResponseWriter, _ *http.Request, rawID string) {
	id, err := decodeContainerID(rawID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	unlock := s.containerTransitions.lock(id)
	defer unlock()

	record, err := state.Get(id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if record.Status != "running" && record.Status != "paused" {
		writeError(writer, http.StatusConflict, fmt.Errorf("container %s is not running", record.ID))
		return
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		writeError(writer, http.StatusConflict, fmt.Errorf("verify container process: %w", err))
		return
	}
	processes, bootTime, err := readContainerProcessTree("/proc", record.PID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("read container process table: %w", err))
		return
	}

	rows := make([][]string, 0, len(processes))
	pidByHost := make(map[int]int, len(processes))
	for _, process := range processes {
		pidByHost[process.hostPID] = process.pid
		for _, threadPID := range process.threadPIDs {
			pidByHost[threadPID] = process.pid
		}
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].pid < processes[j].pid })
	for _, process := range processes {
		ppid := pidByHost[process.ppid]
		started := time.Unix(int64(bootTime)+int64(process.start/procClockTicksPerSecond), 0).Format("15:04")
		command := process.command
		if command == "" {
			command = process.comm
		}
		rows = append(rows, []string{
			strconv.FormatUint(uint64(process.uid), 10),
			strconv.Itoa(process.pid),
			strconv.Itoa(ppid),
			"0",
			started,
			process.tty,
			formatProcessCPUTime(process.cpuTime),
			command,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"Titles": []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"},
		"Processes": rows,
	})
}

func readContainerProcessTree(procRoot string, rootPID int) ([]dockerTopProcess, int64, error) {
	if rootPID <= 1 {
		return nil, 0, fmt.Errorf("container host PID must be greater than 1")
	}
	bootTime, err := readProcBootTime(procRoot)
	if err != nil {
		return nil, 0, err
	}
	queue := []int{rootPID}
	visited := make(map[int]struct{})
	processes := make([]dockerTopProcess, 0, 16)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, exists := visited[pid]; exists {
			continue
		}
		visited[pid] = struct{}{}
		process, err := readContainerProcessAt(procRoot, pid)
		if err != nil {
			if pid == rootPID {
				return nil, 0, fmt.Errorf("read init process %d: %w", pid, err)
			}
			continue // short-lived descendants may exit while /proc is scanned
		}
		processes = append(processes, process)
		if len(processes) > maxContainerTopProcesses {
			return nil, 0, fmt.Errorf("container process tree exceeds the %d-process limit", maxContainerTopProcesses)
		}
		children, err := readProcChildren(procRoot, pid)
		if err != nil {
			if os.IsNotExist(err) {
				continue // the process exited after its stat record was verified
			}
			return nil, 0, fmt.Errorf("read process %d child list: %w", pid, err)
		}
		if len(queue)+len(children)+len(processes) > maxContainerTopProcesses*2 {
			return nil, 0, fmt.Errorf("container process tree exceeds the scan limit")
		}
		queue = append(queue, children...)
	}
	return processes, bootTime, nil
}

func readContainerProcessAt(procRoot string, pid int) (dockerTopProcess, error) {
	var result dockerTopProcess
	if pid <= 0 {
		return result, fmt.Errorf("invalid PID")
	}
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	statPath := filepath.Join(base, "stat")
	statBefore, err := readProcFile(statPath, 64<<10)
	if err != nil {
		return result, err
	}
	parsed, err := parseDockerTopProcStat(string(statBefore))
	if err != nil {
		return result, err
	}
	status, err := readProcFile(filepath.Join(base, "status"), maxContainerProcFileBytes)
	if err != nil {
		return result, err
	}
	uid, namespacePID, err := parseDockerTopProcStatus(string(status), pid)
	if err != nil {
		return result, err
	}
	uid, err = mapContainerUID(procRoot, pid, uid)
	if err != nil {
		return result, err
	}
	threadPIDs, err := readProcTaskIDs(procRoot, pid)
	if err != nil {
		return result, err
	}
	cmdline, _ := readProcFile(filepath.Join(base, "cmdline"), maxContainerProcFileBytes)
	command := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
	tty := readProcTTY(filepath.Join(base, "fd", "0"))

	// A PID can exit and be reused while its proc files are read. Recheck
	// starttime before including the collected metadata in the response.
	statAfter, err := readProcFile(statPath, 64<<10)
	if err != nil {
		return result, err
	}
	parsedAfter, err := parseDockerTopProcStat(string(statAfter))
	if err != nil || parsed.start != parsedAfter.start {
		return result, fmt.Errorf("process %d exited or was reused while inspecting", pid)
	}
	return dockerTopProcess{
		hostPID: pid,
		threadPIDs: threadPIDs,
		pid: namespacePID,
		ppid: parsed.ppid,
		uid: uid,
		comm: parsed.comm,
		command: command,
		tty: tty,
		cpuTime: parsed.cpuTime,
		start: parsed.start,
	}, nil
}

type dockerTopProcStat struct {
	comm    string
	ppid    int
	cpuTime uint64
	start   uint64
}

func parseDockerTopProcStat(value string) (dockerTopProcStat, error) {
	var result dockerTopProcStat
	open := strings.IndexByte(value, '(')
	close := strings.LastIndexByte(value, ')')
	if open < 1 || close <= open || close+1 >= len(value) {
		return result, fmt.Errorf("malformed /proc stat record")
	}
	result.comm = value[open+1:close]
	fields := strings.Fields(value[close+1:])
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" {
		return result, fmt.Errorf("process is gone or /proc stat is incomplete")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return result, fmt.Errorf("invalid parent PID in /proc stat")
	}
	userTicks, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return result, fmt.Errorf("invalid user CPU time in /proc stat")
	}
	systemTicks, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return result, fmt.Errorf("invalid system CPU time in /proc stat")
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return result, fmt.Errorf("invalid process start time in /proc stat")
	}
	result.ppid = ppid
	result.cpuTime = userTicks + systemTicks
	result.start = startTicks
	return result, nil
}

func parseDockerTopProcStatus(value string, fallbackPID int) (uint32, int, error) {
	var uid uint64
	namespacePID := fallbackPID
	foundUID := false
	scanner := bufio.NewScanner(strings.NewReader(value))
	for scanner.Scan() {
		key, field, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(field)
		switch key {
		case "Uid":
			if len(fields) < 2 {
				return 0, 0, fmt.Errorf("missing effective UID in /proc status")
			}
			parsed, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid UID in /proc status")
			}
			uid, foundUID = parsed, true
		case "NSpid":
			if len(fields) == 0 {
				return 0, 0, fmt.Errorf("empty NSpid in /proc status")
			}
			parsed, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil || parsed <= 0 {
				return 0, 0, fmt.Errorf("invalid namespace PID in /proc status")
			}
			namespacePID = parsed
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan /proc status: %w", err)
	}
	if !foundUID {
		return 0, 0, fmt.Errorf("UID not found in /proc status")
	}
	return uint32(uid), namespacePID, nil
}

func readProcTaskIDs(procRoot string, pid int) ([]int, error) {
	taskDir := filepath.Join(procRoot, strconv.Itoa(pid), "task")
	directory, err := os.Open(taskDir)
	if err != nil {
		return nil, err
	}
	entries, err := directory.ReadDir(maxContainerTopProcesses + 1)
	closeErr := directory.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > maxContainerTopProcesses {
		return nil, fmt.Errorf("process %d exceeds the thread inspection limit", pid)
	}
	threadIDs := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tid, parseErr := strconv.Atoi(entry.Name())
		if parseErr == nil && tid > 0 {
			threadIDs = append(threadIDs, tid)
		}
	}
	if len(threadIDs) == 0 {
		return nil, fmt.Errorf("process %d has no readable tasks", pid)
	}
	return threadIDs, nil
}

func readProcChildren(procRoot string, pid int) ([]int, error) {
	threadIDs, err := readProcTaskIDs(procRoot, pid)
	if err != nil {
		return nil, err
	}
	children := make([]int, 0)
	for _, tid := range threadIDs {
		path := filepath.Join(procRoot, strconv.Itoa(pid), "task", strconv.Itoa(tid), "children")
		data, readErr := readProcFile(path, 256<<10)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue // the thread exited during inspection
			}
			return nil, fmt.Errorf("read children for task %d: %w", tid, readErr)
		}
		for _, field := range strings.Fields(string(data)) {
			child, parseErr := strconv.Atoi(field)
			if parseErr != nil || child <= 0 {
				return nil, fmt.Errorf("invalid child PID %q", field)
			}
			children = append(children, child)
			if len(children) > maxContainerTopProcesses {
				return nil, fmt.Errorf("process %d has too many children", pid)
			}
		}
	}
	return children, nil
}

func mapContainerUID(procRoot string, pid int, hostUID uint32) (uint32, error) {
	data, err := readProcFile(filepath.Join(procRoot, strconv.Itoa(pid), "uid_map"), 64<<10)
	if err != nil {
		if os.IsNotExist(err) {
			return hostUID, nil
		}
		return 0, fmt.Errorf("read process UID map: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return 0, fmt.Errorf("malformed UID mapping for process %d", pid)
		}
		containerStart, parseErr := strconv.ParseUint(fields[0], 10, 32)
		if parseErr != nil {
			return 0, fmt.Errorf("invalid container UID mapping for process %d", pid)
		}
		hostStart, parseErr := strconv.ParseUint(fields[1], 10, 32)
		if parseErr != nil {
			return 0, fmt.Errorf("invalid host UID mapping for process %d", pid)
		}
		length, parseErr := strconv.ParseUint(fields[2], 10, 32)
		if parseErr != nil || length == 0 {
			return 0, fmt.Errorf("invalid UID mapping length for process %d", pid)
		}
		hostValue := uint64(hostUID)
		if hostValue < hostStart || hostValue-hostStart >= length {
			continue
		}
		containerValue := containerStart + hostValue - hostStart
		if containerValue > uint64(^uint32(0)) {
			return 0, fmt.Errorf("container UID mapping overflows for process %d", pid)
		}
		return uint32(containerValue), nil
	}
	return 0, fmt.Errorf("host UID %d is not covered by process %d UID map", hostUID, pid)
}

func readProcBootTime(procRoot string) (int64, error) {
	file, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, fmt.Errorf("open /proc/stat: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		value, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil || value <= 0 {
			return 0, fmt.Errorf("invalid boot time in /proc/stat")
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	return 0, fmt.Errorf("boot time is missing from /proc/stat")
}

func readProcFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("proc file %s exceeds %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

func readProcTTY(path string) string {
	target, err := os.Readlink(path)
	if err != nil || !strings.HasPrefix(target, "/dev/") {
		return "?"
	}
	return strings.TrimPrefix(target, "/dev/")
}

func formatProcessCPUTime(ticks uint64) string {
	seconds := ticks / procClockTicksPerSecond
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, (seconds/60)%60, seconds%60)
}
