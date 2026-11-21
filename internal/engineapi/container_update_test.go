package engineapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerContainerUpdateAppliesAndPersistsLiveResourceChanges(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "resource-update", container.Options{CgroupEnabled: true})
	var gotPID int
	var gotOptions container.Options
	var gotUpdate container.CgroupResourceUpdate
	server.verifyUpdatePID = func(state.Record) error { return nil }
	server.updateCgroupResources = func(pid int, options container.Options, update container.CgroupResourceUpdate) error {
		gotPID, gotOptions, gotUpdate = pid, options, update
		return nil
	}

	request := httptest.NewRequest(http.MethodPost, "/v1.45/containers/"+record.ID+"/update", strings.NewReader(
		`{"Memory":134217728,"MemoryReservation":33554432,"MemorySwap":268435456,"NanoCpus":500000000,"PidsLimit":128}`,
	))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string][]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if warnings, ok := payload["Warnings"]; !ok || len(warnings) != 0 {
		t.Fatalf("update response = %#v", payload)
	}
	running, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPID != running.PID {
		t.Fatalf("updated PID = %d, want %d", gotPID, running.PID)
	}
	if gotOptions.MemoryLimitBytes != 128<<20 || gotOptions.MemoryLowBytes != 32<<20 ||
		gotOptions.MemorySwapMaxBytes != 128<<20 || gotOptions.CPUs != 0.5 || gotOptions.PidsLimit != 128 {
		t.Fatalf("updated options = %+v", gotOptions)
	}
	if !gotUpdate.Memory || !gotUpdate.MemoryReservation || !gotUpdate.MemorySwap || !gotUpdate.NanoCPUs || !gotUpdate.PidsLimit {
		t.Fatalf("resource update mask = %+v", gotUpdate)
	}
	saved, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Options.MemoryLimitBytes != gotOptions.MemoryLimitBytes ||
		saved.Options.MemoryLowBytes != gotOptions.MemoryLowBytes ||
		saved.Options.MemorySwapMaxBytes != gotOptions.MemorySwapMaxBytes ||
		saved.Options.MemorySwapMaxSet != gotOptions.MemorySwapMaxSet ||
		saved.Options.CPUs != gotOptions.CPUs ||
		saved.Options.PidsLimit != gotOptions.PidsLimit {
		t.Fatalf("saved resource options = %+v", saved.Options)
	}
}

func TestDockerContainerUpdateRejectsInvalidRequestsBeforeApplying(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "resource-update-invalid", container.Options{CgroupEnabled: true})
	calls := 0
	server.verifyUpdatePID = func(state.Record) error { return nil }
	server.updateCgroupResources = func(int, container.Options, container.CgroupResourceUpdate) error {
		calls++
		return nil
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty", body: `{}`},
		{name: "negative memory", body: `{"Memory":-1}`},
		{name: "negative reservation", body: `{"MemoryReservation":-1}`},
		{name: "negative swap", body: `{"MemorySwap":-2}`},
		{name: "swap below memory", body: `{"Memory":1048576,"MemorySwap":524288}`},
		{name: "reservation above memory", body: `{"Memory":1048576,"MemoryReservation":2097152}`},
		{name: "negative CPU", body: `{"NanoCpus":-1}`},
		{name: "negative PID limit", body: `{"PidsLimit":-2}`},
		{name: "reservation equal to hard limit", body: `{"Memory":1048576,"MemoryReservation":1048576}`},
		{name: "unsupported field", body: `{"CpuShares":1024}`},
		{name: "trailing JSON", body: `{"Memory":1} {"Memory":2}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/update", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("resource updater called %d times for invalid requests", calls)
	}
}

func TestDockerContainerUpdateDoesNotPersistWhenKernelUpdateFails(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "resource-update-failure", container.Options{
		CgroupEnabled: true, MemoryLimitBytes: 64 << 20,
	})
	server.verifyUpdatePID = func(state.Record) error { return nil }
	server.updateCgroupResources = func(int, container.Options, container.CgroupResourceUpdate) error {
		return errors.New("controller is not delegated")
	}
	request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/update", strings.NewReader(`{"Memory":134217728}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	saved, err := state.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Options.MemoryLimitBytes != 64<<20 {
		t.Fatalf("failed kernel update changed persisted memory limit to %d", saved.Options.MemoryLimitBytes)
	}
}

func TestDockerContainerUpdateRequiresRunningVerifiedContainer(t *testing.T) {
	server := setupEngineAPITest(t)
	record, err := state.Create("resource-update-created", []string{"/bin/true"}, container.Options{CgroupEnabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.verifyUpdatePID = func(state.Record) error { return nil }
	server.updateCgroupResources = func(int, container.Options, container.CgroupResourceUpdate) error {
		t.Fatal("resource updater called for an unstarted container")
		return nil
	}
	request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/update", strings.NewReader(`{"Memory":1048576}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDockerContainerUpdateSupportsZeroSwapAndUnlimitedPids(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "resource-update-reset", container.Options{
		CgroupEnabled: true, MemoryLimitBytes: 64 << 20,
	})
	server.verifyUpdatePID = func(state.Record) error { return nil }
	var applied container.Options
	server.updateCgroupResources = func(_ int, options container.Options, _ container.CgroupResourceUpdate) error {
		applied = options
		return nil
	}
	request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/update",
		strings.NewReader(`{"MemorySwap":67108864,"PidsLimit":-1}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !applied.MemorySwapMaxSet || applied.MemorySwapMaxBytes != 0 {
		t.Fatalf("explicit zero swap ceiling was not preserved: %+v", applied)
	}
	if applied.PidsLimit != 0 {
		t.Fatalf("PidsLimit=-1 was not normalized to unlimited: %d", applied.PidsLimit)
	}
}

func TestDockerOptionsCreatesManagedCgroupAndAcceptsUnlimitedSwap(t *testing.T) {
	options, err := dockerOptions(dockerHostConfig{Memory: 64 << 20, MemorySwap: -1})
	if err != nil {
		t.Fatalf("dockerOptions with unlimited swap: %v", err)
	}
	if !options.CgroupEnabled || !options.MemorySwapUnlimited || options.MemorySwapMaxSet || options.MemorySwapMaxBytes != 0 {
		t.Fatalf("Docker options do not create a managed unlimited cgroup: %+v", options)
	}
}

func TestDockerOptionsAppliesDockerDefaultSwapAlongsideMemoryLimit(t *testing.T) {
	options, err := dockerOptions(dockerHostConfig{Memory: 64 << 20})
	if err != nil {
		t.Fatalf("dockerOptions with default swap: %v", err)
	}
	if !options.MemorySwapDefaulted || !options.MemorySwapMaxSet || options.MemorySwapMaxBytes != 64<<20 {
		t.Fatalf("Docker default memory+swap settings = %+v", options)
	}
}

func TestDockerOptionsDefaultSwapCanBeActivatedByLaterMemoryUpdate(t *testing.T) {
	options, err := dockerOptions(dockerHostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !options.MemorySwapDefaulted || options.MemorySwapMaxSet {
		t.Fatalf("unlimited-memory Docker default should remain implicit: %+v", options)
	}
}

func TestDockerContainerUpdatePreservesMemorySwapSemanticsWhenMemoryChanges(t *testing.T) {
	for index, test := range []struct {
		name          string
		initial       container.Options
		body          string
		wantSwap      int64
		wantSet       bool
		wantDefaulted bool
		wantStatus    int
	}{
		{
			name:    "default swap follows the new memory limit",
			initial: container.Options{CgroupEnabled: true, MemoryLimitBytes: 64 << 20, MemorySwapMaxBytes: 64 << 20, MemorySwapMaxSet: true, MemorySwapDefaulted: true},
			body:    `{"Memory":134217728}`, wantSwap: 128 << 20, wantSet: true, wantDefaulted: true, wantStatus: http.StatusOK,
		},
		{
			name:    "explicit total swap remains constant",
			initial: container.Options{CgroupEnabled: true, MemoryLimitBytes: 64 << 20, MemorySwapMaxBytes: 64 << 20, MemorySwapMaxSet: true},
			body:    `{"Memory":100663296}`, wantSwap: 32 << 20, wantSet: true, wantStatus: http.StatusOK,
		},
		{
			name:    "memory cannot exceed an explicit total swap ceiling",
			initial: container.Options{CgroupEnabled: true, MemoryLimitBytes: 64 << 20, MemorySwapMaxBytes: 64 << 20, MemorySwapMaxSet: true},
			body:    `{"Memory":150994944}`, wantStatus: http.StatusBadRequest,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := setupEngineAPITest(t)
			record := createEngineAPITestContainer(t, "swap-update-"+string(rune('a'+index)), test.initial)
			server.verifyUpdatePID = func(state.Record) error { return nil }
			var applied container.Options
			server.updateCgroupResources = func(_ int, options container.Options, _ container.CgroupResourceUpdate) error {
				applied = options
				return nil
			}
			request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/update", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if test.wantStatus == http.StatusOK && (applied.MemorySwapMaxBytes != test.wantSwap ||
				applied.MemorySwapMaxSet != test.wantSet || applied.MemorySwapDefaulted != test.wantDefaulted) {
				t.Fatalf("updated swap options = %+v", applied)
			}
		})
	}
}

func TestDockerContainerUpdatePreservesExplicitUnlimitedSwap(t *testing.T) {
	server := setupEngineAPITest(t)
	record := createEngineAPITestContainer(t, "resource-swap-unlimited", container.Options{
		CgroupEnabled: true, MemoryLimitBytes: 64 << 20, MemorySwapUnlimited: true,
	})
	server.verifyUpdatePID = func(state.Record) error { return nil }
	var applied container.Options
	server.updateCgroupResources = func(_ int, options container.Options, _ container.CgroupResourceUpdate) error {
		applied = options
		return nil
	}
	request := httptest.NewRequest(http.MethodPost, "/containers/"+record.ID+"/update",
		strings.NewReader(`{"Memory":134217728}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !applied.MemorySwapUnlimited || applied.MemorySwapMaxSet || applied.MemorySwapMaxBytes != 0 {
		t.Fatalf("explicit unlimited swap was not preserved: %+v", applied)
	}
}

func TestDockerOptionsAcceptsUnlimitedPIDLimit(t *testing.T) {
	options, err := dockerOptions(dockerHostConfig{PidsLimit: -1})
	if err != nil {
		t.Fatalf("dockerOptions with unlimited PidsLimit: %v", err)
	}
	if options.PidsLimit != 0 {
		t.Fatalf("PidsLimit=-1 should map to unlimited cgroup setting, got %d", options.PidsLimit)
	}
}
