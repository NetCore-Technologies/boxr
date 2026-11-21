package engineapi

import (
	"encoding/json"
	"net/http"
	"testing"

	imageStore "github.com/manit6752025/boxr/internal/image"
)

func TestDockerImageHistoryReportsFlattenedSnapshotWithoutFabricatedSteps(t *testing.T) {
	server := setupEngineAPITest(t)
	importEngineAPITestImage(t, "history-test:latest")
	image, err := imageStore.Inspect("history-test:latest")
	if err != nil {
		t.Fatal(err)
	}
	response := engineAPIRequest(server, http.MethodGet, "/v1.45/images/history-test:latest/history")
	if response.Code != http.StatusOK {
		t.Fatalf("image history status=%d body=%s", response.Code, response.Body.String())
	}
	var history []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("flattened Boxr image history has %d entries, want one: %#v", len(history), history)
	}
	entry := history[0]
	if entry["Id"] != image.Layer.Digest || entry["CreatedBy"] != "boxr rootfs snapshot" ||
		entry["Size"] != float64(image.Layer.Size) {
		t.Fatalf("image history snapshot = %#v", entry)
	}
	tags, ok := entry["Tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != image.Reference {
		t.Fatalf("image history tags = %#v", entry["Tags"])
	}
	comment, ok := entry["Comment"].(string)
	if !ok || comment == "" {
		t.Fatalf("image history did not disclose flattened history: %#v", entry)
	}
}

func TestDockerImageHistoryRejectsUnsupportedRequests(t *testing.T) {
	server := setupEngineAPITest(t)
	importEngineAPITestImage(t, "history-method-test:latest")
	method := engineAPIRequest(server, http.MethodPost, "/images/history-method-test:latest/history")
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST image history status=%d body=%s", method.Code, method.Body.String())
	}
	missing := engineAPIRequest(server, http.MethodGet, "/images/missing-history:latest/history")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing image history status=%d body=%s", missing.Code, missing.Body.String())
	}
}
