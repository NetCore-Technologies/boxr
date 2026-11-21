package engineapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestInfoCountsPausedContainersSeparately(t *testing.T) {
	server := setupEngineAPITest(t)

	_ = createEngineAPITestContainer(t, "info-running", container.Options{})
	paused := createEngineAPITestContainer(t, "info-paused", container.Options{})
	if paused.ProcessStartTicks == 0 {
		t.Skip("current platform does not expose process start ticks")
	}
	if err := state.SetPaused(paused.ID, paused.PID, paused.ProcessStartTicks, true); err != nil {
		t.Fatalf("mark test container paused: %v", err)
	}
	if _, err := state.Create("info-stopped", []string{"/bin/true"}, container.Options{}, nil); err != nil {
		t.Fatal(err)
	}

	response := engineAPIRequest(server, http.MethodGet, "/v1.45/info")
	if response.Code != http.StatusOK {
		t.Fatalf("info status=%d body=%s", response.Code, response.Body.String())
	}

	var info map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if got := info["Containers"]; got != float64(3) {
		t.Errorf("Containers = %v, want 3", got)
	}
	if got := info["ContainersRunning"]; got != float64(1) {
		t.Errorf("ContainersRunning = %v, want 1", got)
	}
	if got := info["ContainersPaused"]; got != float64(1) {
		t.Errorf("ContainersPaused = %v, want 1", got)
	}
	if got := info["ContainersStopped"]; got != float64(1) {
		t.Errorf("ContainersStopped = %v, want 1", got)
	}
}
