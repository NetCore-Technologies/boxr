package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestEventsFollowStreamsNewMatchingEvents(t *testing.T) {
	old := state.Event{Time: time.Now().Add(-time.Minute), Action: "create", ID: "old"}
	ignored := state.Event{Time: time.Now(), Action: "create", ID: "new-a"}
	matching := state.Event{Time: time.Now(), Action: "die", ID: "new-b", PID: 123}
	fetchCalls := 0
	fetch := func() ([]state.Event, error) {
		fetchCalls++
		return []state.Event{old, ignored, matching}, nil
	}
	matches := func(event state.Event) bool { return event.Action == "die" }
	var stdout, stderr bytes.Buffer
	if code := streamEvents(&stdout, &stderr, time.Millisecond, 1, "json", nil, 1, matches, fetch); code != 0 {
		t.Fatalf("streamEvents returned %d: %s", code, stderr.String())
	}
	var got state.Event
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("decode streamed event: %v: %q", err, stdout.String())
	}
	if got.ID != matching.ID || got.Action != "die" || fetchCalls != 1 {
		t.Fatalf("unexpected streamed event %+v, fetch calls %d", got, fetchCalls)
	}
}

func TestEventsFollowEmitsSinceCatchupBeforePolling(t *testing.T) {
	event := state.Event{Time: time.Now(), Action: "start", ID: "catch-up"}
	fetchCalls := 0
	fetch := func() ([]state.Event, error) {
		fetchCalls++
		return nil, nil
	}
	var stdout, stderr bytes.Buffer
	if code := streamEvents(&stdout, &stderr, time.Millisecond, 1, "json", []state.Event{event}, 1, func(state.Event) bool { return true }, fetch); code != 0 {
		t.Fatalf("streamEvents returned %d: %s", code, stderr.String())
	}
	var got state.Event
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("decode catch-up event: %v", err)
	}
	if got.ID != event.ID || fetchCalls != 0 {
		t.Fatalf("unexpected catch-up result %+v, fetch calls %d", got, fetchCalls)
	}
}

func TestEventsFollowReportsSourceAndOutputErrors(t *testing.T) {
	var stderr bytes.Buffer
	fetchFailure := func() ([]state.Event, error) { return nil, errors.New("event log unavailable") }
	if code := streamEvents(&bytes.Buffer{}, &stderr, time.Millisecond, 0, "json", nil, 0, func(state.Event) bool { return true }, fetchFailure); code != 1 {
		t.Fatalf("fetch failure returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "event log unavailable") {
		t.Fatalf("missing fetch error: %q", stderr.String())
	}

	stderr.Reset()
	event := state.Event{Time: time.Now(), Action: "create", ID: "write-fail"}
	if code := streamEvents(eventsFailWriter{}, &stderr, time.Millisecond, 1, "json", []state.Event{event}, 1, func(state.Event) bool { return true }, fetchFailure); code != 1 {
		t.Fatalf("write failure returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "write event") {
		t.Fatalf("missing write error: %q", stderr.String())
	}
}

type eventsFailWriter struct{}

func (eventsFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken event output")
}

func TestRunEventsFollowValidatesWatchOptions(t *testing.T) {
	for _, args := range [][]string{
		{"events", "--follow", "--interval", "1ms"},
		{"events", "--follow", "--interval", "0s"},
		{"events", "--follow", "--count", "-1"},
		{"events", "--count", "2"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) returned %d, want 2; stderr=%q", args, code, stderr.String())
		}
	}
}

func TestRunEventsFollowCatchesUpSince(t *testing.T) {
	t.Setenv("BOXR_STATE_DIR", t.TempDir())
	record, err := state.Create("event-follow", []string{"/bin/true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"events", "--follow", "--since", "1h", "--count", "1", "--format", "json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("events follow returned %d: %s", code, stderr.String())
	}
	var event state.Event
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &event); err != nil {
		t.Fatalf("decode catch-up JSON line: %v: %q", err, stdout.String())
	}
	if event.ID != record.ID || event.Action != "create" {
		t.Fatalf("unexpected catch-up event: %+v", event)
	}
}
