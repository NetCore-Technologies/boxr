// Package state stores local Boxr container lifecycle records and an event log.
// Records deliberately omit environment values because they may contain secrets.
package state

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/container"
)

const maxEventFileBytes = 4 << 20

var storeMu sync.Mutex

type Record struct {
	ID            string            `json:"id"`
	Name          string            `json:"name,omitempty"`
	PID               int               `json:"pid,omitempty"`
	ProcessStartTicks uint64            `json:"processStartTicks,omitempty"`
	Command           []string          `json:"command"`
	Status        string            `json:"status"`
	ExitCode      *int              `json:"exitCode,omitempty"`
	LastExitCode  *int              `json:"lastExitCode,omitempty"`
	RestartCount  int               `json:"restartCount,omitempty"`
	StopRequested bool              `json:"stopRequested,omitempty"`
	Error         string            `json:"error,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Options       container.Options `json:"options"`
	CreatedAt     time.Time         `json:"createdAt"`
	StartedAt     *time.Time        `json:"startedAt,omitempty"`
	FinishedAt    *time.Time        `json:"finishedAt,omitempty"`
}

type Event struct {
	Time     time.Time `json:"time"`
	Action   string    `json:"action"`
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	PID      int       `json:"pid,omitempty"`
	ExitCode *int      `json:"exitCode,omitempty"`
}

func Dir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("BOXR_STATE_DIR")); value != "" {
		return filepath.Abs(value)
	}
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "boxr", "containers"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "boxr", "containers"), nil
}

func ensureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("state path %s must be a real directory", dir)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("secure state directory: %w", err)
	}
	return dir, nil
}

func Create(name string, command []string, options container.Options, labels map[string]string) (Record, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if name != "" {
		if err := validateName(name); err != nil {
			return Record{}, err
		}
		records, err := listRecordsLocked(true)
		if err != nil {
			return Record{}, err
		}
		for _, record := range records {
			if record.Name == name {
				return Record{}, fmt.Errorf("container name %q is already in use by %s", name, record.ID)
			}
		}
	}
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	options.Env = nil // Never persist environment values; they frequently contain credentials.
	record := Record{
		ID: id, Name: name, Command: append([]string(nil), command...), Status: "created",
		Labels: cloneLabels(labels), Options: options, CreatedAt: time.Now().UTC(),
	}
	if err := saveRecord(record); err != nil {
		return Record{}, err
	}
	if err := appendEvent(Event{Time: record.CreatedAt, Action: "create", ID: id, Name: name}); err != nil {
		_ = os.Remove(recordPath(id))
		return Record{}, err
	}
	return record, nil
}

func Start(id string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("host PID must be positive")
	}
	// Best-effort capture retains compatibility with records written before
	// process identity was introduced while protecting newly launched containers.
	startTicks, _ := processStartTicks(pid)
	return update(id, func(record *Record) error {
		if record.Status != "created" && record.Status != "restarting" {
			return fmt.Errorf("cannot start container in %q state", record.Status)
		}
		if record.StopRequested {
			return fmt.Errorf("container %s has a pending stop request", record.ID)
		}
		now := time.Now().UTC()
		record.Status, record.PID, record.StartedAt = "running", pid, &now
		record.ProcessStartTicks = startTicks
		record.ExitCode = nil
		record.LastExitCode = nil
		record.Error = ""
		record.FinishedAt = nil
		if err := appendEvent(Event{Time: now, Action: "start", ID: record.ID, Name: record.Name, PID: pid}); err != nil {
			return err
		}
		return nil
	})
}

// PrepareRestart moves a completed container into a bounded restart delay.
// The previous exit code remains inspectable until the next attempt starts.
func PrepareRestart(id string) error {
	return update(id, func(record *Record) error {
		if record.Status != "exited" && record.Status != "failed" {
			return fmt.Errorf("cannot prepare restart in %q state", record.Status)
		}
		if record.StopRequested {
			return fmt.Errorf("container %s has a pending stop request", record.ID)
		}
		if record.ExitCode != nil {
			code := *record.ExitCode
			record.LastExitCode = &code
		}
		record.RestartCount++
		record.PID = 0
		record.ProcessStartTicks = 0
		record.Status = "restarting"
		return appendEvent(Event{Time: time.Now().UTC(), Action: "restart", ID: record.ID, Name: record.Name})
	})
}

// RequestStop prevents any configured restart policy from starting another
// attempt. It is safe to call repeatedly for running or restarting records.
func RequestStop(id string) error {
	return update(id, func(record *Record) error {
		if record.Status == "restarting" {
			record.StopRequested = true
			record.PID = 0
			record.ProcessStartTicks = 0
			if record.ExitCode != nil && *record.ExitCode != 0 {
				record.Status = "failed"
			} else {
				record.Status = "exited"
			}
			return appendEvent(Event{Time: time.Now().UTC(), Action: "restart-cancelled", ID: record.ID, Name: record.Name})
		}
		if record.Status != "running" {
			return fmt.Errorf("cannot request stop in %q state", record.Status)
		}
		if record.StopRequested {
			return nil
		}
		record.StopRequested = true
		return appendEvent(Event{Time: time.Now().UTC(), Action: "stop-requested", ID: record.ID, Name: record.Name, PID: record.PID})
	})
}

// CancelStopRequest rolls back an unsuccessful stop signal delivery.
func CancelStopRequest(id string) error {
	return update(id, func(record *Record) error {
		if record.Status != "running" || !record.StopRequested {
			return fmt.Errorf("container %s has no cancellable stop request", record.ID)
		}
		record.StopRequested = false
		return appendEvent(Event{Time: time.Now().UTC(), Action: "stop-cancelled", ID: record.ID, Name: record.Name, PID: record.PID})
	})
}

func Finish(id string, runErr error) error {
	return update(id, func(record *Record) error {
		if record.Status != "running" && record.Status != "created" && record.Status != "restarting" {
			return fmt.Errorf("cannot finish container in %q state", record.Status)
		}
		now := time.Now().UTC()
		record.FinishedAt = &now
		code := 0
		if runErr != nil {
			var exitErr *container.ExitError
			if errors.As(runErr, &exitErr) {
				code = exitErr.Code
			} else {
				var processExit *exec.ExitError
				if errors.As(runErr, &processExit) {
					if exitCode, ok := processExitCode(processExit); ok {
						code = exitCode
					} else {
						code = 125
						record.Error = runErr.Error()
					}
				} else {
					code = 125
					record.Error = runErr.Error()
				}
			}
		}
		record.ExitCode = &code
		if code == 0 {
			record.Status = "exited"
		} else {
			record.Status = "failed"
		}
		return appendEvent(Event{Time: now, Action: "die", ID: record.ID, Name: record.Name, PID: record.PID, ExitCode: &code})
	})
}

func RemoveCreated(id string) error {
	return remove(id, true)
}

func Remove(query string) error {
	return remove(query, false)
}

func remove(query string, allowCreated bool) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(query)
	if err != nil {
		return err
	}
	if record.Status == "running" || record.Status == "restarting" {
		return fmt.Errorf("container %s is %s; stop it before removal", record.ID, record.Status)
	}
	if record.Status == "created" && !allowCreated {
		return fmt.Errorf("container %s has not completed; refusing removal", record.ID)
	}
	if err := os.Remove(recordPath(record.ID)); err != nil {
		return fmt.Errorf("remove container record: %w", err)
	}
	if err := os.Remove(logPath(record.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove container log: %w", err)
	}
	return appendEvent(Event{Time: time.Now().UTC(), Action: "remove", ID: record.ID, Name: record.Name})
}

func Get(query string) (Record, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return findLocked(query)
}

func List(all bool) ([]Record, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return listRecordsLocked(all)
}

func Events(limit int) ([]Event, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	dir, err := ensureDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "events.jsonl")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("event log must be a regular file")
	}
	if info.Size() > maxEventFileBytes {
		return nil, fmt.Errorf("event log exceeds %d bytes; rotate it before reading", maxEventFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode event log: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func update(id string, apply func(*Record) error) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	record, err := findLocked(id)
	if err != nil {
		return err
	}
	if err := apply(&record); err != nil {
		return err
	}
	return saveRecord(record)
}

func findLocked(query string) (Record, error) {
	if strings.TrimSpace(query) == "" {
		return Record{}, fmt.Errorf("container ID or name is required")
	}
	records, err := listRecordsLocked(true)
	if err != nil {
		return Record{}, err
	}
	var matches []Record
	for _, record := range records {
		if record.ID == query || record.Name == query || strings.HasPrefix(record.ID, query) {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return Record{}, fmt.Errorf("no container matches %q", query)
	}
	if len(matches) > 1 {
		return Record{}, fmt.Errorf("container query %q is ambiguous; use a full ID or name", query)
	}
	return matches[0], nil
}

func listRecordsLocked(all bool) ([]Record, error) {
	dir, err := ensureDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "events.json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, fmt.Errorf("state record %s is not a bounded regular file", entry.Name())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode state record %s: %w", entry.Name(), err)
		}
		if all || record.Status == "created" || record.Status == "running" || record.Status == "restarting" {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt.After(records[j].CreatedAt) })
	return records, nil
}

func saveRecord(record Record) error {
	dir, err := ensureDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".record-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, recordPath(record.ID)); err != nil {
		return err
	}
	ok = true
	return nil
}

func appendEvent(event Event) error {
	dir, err := ensureDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "events.jsonl")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("event log must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func recordPath(id string) string {
	dir, _ := Dir()
	return filepath.Join(dir, id+".json")
}

func logPath(id string) string {
	dir, _ := Dir()
	return filepath.Join(dir, id+".log")
}

func newID() (string, error) {
	var bytes [6]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate container ID: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func validateName(name string) error {
	if len(name) > 64 || name == "" {
		return fmt.Errorf("container name must be between 1 and 64 characters")
	}
	for index, char := range name {
		valid := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || index > 0 && (char == '-' || char == '_' || char == '.')
		if !valid {
			return fmt.Errorf("container name %q contains an invalid character", name)
		}
	}
	return nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	copyOf := make(map[string]string, len(labels))
	for key, value := range labels {
		copyOf[key] = value
	}
	return copyOf
}

// Prune removes completed records older than before. Running containers are never removed.
func Prune(before time.Time) (int, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	records, err := listRecordsLocked(true)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, record := range records {
		if record.Status == "running" || record.Status == "created" || record.CreatedAt.After(before) {
			continue
		}
		if err := os.Remove(recordPath(record.ID)); err != nil {
			return removed, err
		}
		if err := os.Remove(logPath(record.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		if err := appendEvent(Event{Time: time.Now().UTC(), Action: "prune", ID: record.ID, Name: record.Name}); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
