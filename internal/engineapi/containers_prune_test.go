package engineapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func requestContainerPrune(t *testing.T, server *Server, filters map[string][]string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1.45/containers/prune"
	if filters != nil {
		data, err := json.Marshal(filters)
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.RawURLEncoding.EncodeToString(data)
		target += "?filters=" + url.QueryEscape(encoded)
	}
	return engineAPIRequest(server, http.MethodPost, target)
}

func createContainerPruneRecord(t *testing.T, name string, labels map[string]string) state.Record {
	t.Helper()
	record, err := state.Create(name, []string{"/bin/true"}, container.Options{}, labels)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func setContainerPruneCreatedAt(t *testing.T, record state.Record, createdAt time.Time) {
	t.Helper()
	directory, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, record.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored state.Record
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.CreatedAt = createdAt
	data, err = json.MarshalIndent(stored, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestEngineAPIContainerPruneRemovesCreatedAndExitedButKeepsRunning(t *testing.T) {
	server := setupEngineAPITest(t)
	created := createContainerPruneRecord(t, "prune-created", map[string]string{"team": "platform"})
	exited := createContainerPruneRecord(t, "prune-exited", map[string]string{"team": "platform"})
	if err := state.Finish(exited.ID, nil); err != nil {
		t.Fatal(err)
	}
	running := createContainerPruneRecord(t, "prune-running", map[string]string{"team": "platform"})
	if err := state.Start(running.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	response := requestContainerPrune(t, server, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("prune status=%d body=%s", response.Code, response.Body.String())
	}
	var result dockerContainerPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	deleted := make(map[string]bool, len(result.ContainersDeleted))
	for _, id := range result.ContainersDeleted {
		deleted[id] = true
	}
	if !deleted[created.ID] || !deleted[exited.ID] || deleted[running.ID] {
		t.Fatalf("unexpected prune result: %#v", result)
	}
	if result.SpaceReclaimed != 0 {
		t.Fatalf("SpaceReclaimed=%d, want zero until writable-layer accounting is implemented", result.SpaceReclaimed)
	}
	if _, err := state.Get(running.ID); err != nil {
		t.Fatalf("running container was removed: %v", err)
	}
	if _, err := state.Get(created.ID); err == nil {
		t.Fatal("created container still exists after prune")
	}
	if _, err := state.Get(exited.ID); err == nil {
		t.Fatal("exited container still exists after prune")
	}
}

func TestEngineAPIContainerPruneHonorsUntilAndPositiveLabelFilters(t *testing.T) {
	server := setupEngineAPITest(t)
	oldMatch := createContainerPruneRecord(t, "prune-old-match", map[string]string{"team": "cleanup"})
	setContainerPruneCreatedAt(t, oldMatch, time.Now().Add(-2*time.Hour))
	oldOther := createContainerPruneRecord(t, "prune-old-other", map[string]string{"team": "retain"})
	setContainerPruneCreatedAt(t, oldOther, time.Now().Add(-2*time.Hour))
	recentMatch := createContainerPruneRecord(t, "prune-recent-match", map[string]string{"team": "cleanup"})

	response := requestContainerPrune(t, server, map[string][]string{
		"until": {"1h"},
		"label": {"team=cleanup"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("prune status=%d body=%s", response.Code, response.Body.String())
	}
	var result dockerContainerPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.ContainersDeleted) != 1 || result.ContainersDeleted[0] != oldMatch.ID {
		t.Fatalf("deleted containers = %#v, want only %s", result.ContainersDeleted, oldMatch.ID)
	}
	for _, id := range []string{oldOther.ID, recentMatch.ID} {
		if _, err := state.Get(id); err != nil {
			t.Errorf("non-matching container %s was removed: %v", id, err)
		}
	}
}

func TestEngineAPIContainerPruneHonorsNegativeLabelFilters(t *testing.T) {
	server := setupEngineAPITest(t)
	retained := createContainerPruneRecord(t, "prune-retained", map[string]string{"team": "retain"})
	removed := createContainerPruneRecord(t, "prune-removed", map[string]string{"team": "delete"})
	unlabelled := createContainerPruneRecord(t, "prune-unlabelled", nil)

	response := requestContainerPrune(t, server, map[string][]string{"label!": {"team=retain"}})
	if response.Code != http.StatusOK {
		t.Fatalf("prune status=%d body=%s", response.Code, response.Body.String())
	}
	var result dockerContainerPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	deleted := make(map[string]bool, len(result.ContainersDeleted))
	for _, id := range result.ContainersDeleted {
		deleted[id] = true
	}
	if !deleted[removed.ID] || !deleted[unlabelled.ID] || deleted[retained.ID] {
		t.Fatalf("negative label filter returned unexpected result: %#v", result)
	}
	if _, err := state.Get(retained.ID); err != nil {
		t.Fatalf("label-excluded container was removed: %v", err)
	}
}

func TestEngineAPIContainerPruneCombinesMultipleNegativeLabelsLikeDocker(t *testing.T) {
	server := setupEngineAPITest(t)
	both := createContainerPruneRecord(t, "prune-both-labels", map[string]string{"foo": "", "bar": ""})
	fooOnly := createContainerPruneRecord(t, "prune-foo-only", map[string]string{"foo": ""})
	barOnly := createContainerPruneRecord(t, "prune-bar-only", map[string]string{"bar": ""})
	neither := createContainerPruneRecord(t, "prune-no-labels", nil)

	response := requestContainerPrune(t, server, map[string][]string{"label!": {"foo", "bar"}})
	if response.Code != http.StatusOK {
		t.Fatalf("prune status=%d body=%s", response.Code, response.Body.String())
	}
	var result dockerContainerPruneResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	deleted := make(map[string]bool, len(result.ContainersDeleted))
	for _, id := range result.ContainersDeleted {
		deleted[id] = true
	}
	if deleted[both.ID] || !deleted[fooOnly.ID] || !deleted[barOnly.ID] || !deleted[neither.ID] {
		t.Fatalf("multiple negative labels did not match Docker's combined-exclusion semantics: %#v", result)
	}
	if _, err := state.Get(both.ID); err != nil {
		t.Fatalf("container with both excluded labels should be protected: %v", err)
	}
}

func TestEngineAPIContainerPruneRejectsInvalidFiltersWithoutMutation(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createContainerPruneRecord(t, "prune-invalid", map[string]string{"team": "platform"})
	if err := state.Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		filters map[string][]string
	}{
		{name: "unsupported key", filters: map[string][]string{"status": {"exited"}}},
		{name: "invalid timestamp", filters: map[string][]string{"until": {"not-a-timestamp"}}},
		{name: "multiple until values", filters: map[string][]string{"until": {"1h", "2h"}}},
		{name: "empty label key", filters: map[string][]string{"label": {"=value"}}},
		{name: "empty filter values", filters: map[string][]string{"label": {}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := requestContainerPrune(t, server, test.filters)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("prune status=%d body=%s, want 400", response.Code, response.Body.String())
			}
			if _, err := state.Get(record.ID); err != nil {
				t.Fatalf("invalid filter removed a container: %v", err)
			}
		})
	}
}
