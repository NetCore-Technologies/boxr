package engineapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerConfigHealthcheckInheritsOverridesAndDisables(t *testing.T) {
	imageDefault := &container.HealthcheckConfig{
		Test:     []string{"CMD", "/usr/bin/health"},
		Interval: 15 * time.Second,
		Retries:  4,
	}
	inherited, err := dockerConfigHealthcheck(nil, imageDefault)
	if err != nil {
		t.Fatalf("inherit healthcheck: %v", err)
	}
	if inherited == imageDefault || inherited.Test[1] != "/usr/bin/health" ||
		inherited.Interval != 15*time.Second || inherited.Timeout != container.DefaultHealthcheckTimeout ||
		inherited.Retries != 4 {
		t.Fatalf("inherited healthcheck = %+v", inherited)
	}
	requested := json.RawMessage(`{
		"Test":["CMD","/custom/health","--ready"],
		"Interval":10000000000,
		"Timeout":2000000000,
		"StartPeriod":3000000000,
		"StartInterval":1000000000,
		"Retries":5
	}`)
	overridden, err := dockerConfigHealthcheck(requested, imageDefault)
	if err != nil {
		t.Fatalf("override healthcheck: %v", err)
	}
	if overridden.Test[1] != "/custom/health" || overridden.Interval != 10*time.Second ||
		overridden.Timeout != 2*time.Second || overridden.StartPeriod != 3*time.Second ||
		overridden.StartInterval != time.Second || overridden.Retries != 5 {
		t.Fatalf("overridden healthcheck = %+v", overridden)
	}
	disabled, err := dockerConfigHealthcheck(json.RawMessage(`{"Test":["NONE"]}`), imageDefault)
	if err != nil || container.HealthcheckEnabled(disabled) {
		t.Fatalf("disabled healthcheck = %+v, %v", disabled, err)
	}
}

func TestDockerConfigHealthcheckRejectsMalformedAndUnknownFields(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"Test":["SHELL","echo ok"]}`),
		json.RawMessage(`{"Test":["CMD","echo ok"],"Mystery":true}`),
		json.RawMessage(`{"Test":["CMD","echo ok"]} {"Test":["NONE"]}`),
		json.RawMessage(`{"Test":["CMD-SHELL",""]}`),
	} {
		if _, err := dockerConfigHealthcheck(raw, nil); err == nil {
			t.Errorf("invalid Healthcheck payload %s was accepted", raw)
		}
	}
}

func TestDockerCreateHealthcheckOverridesImageAndAppearsInInspect(t *testing.T) {
	setupEngineAPITest(t)
	t.Setenv("BOXR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("health fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	image, err := imageStore.ImportRootFSWithConfig(root, "registry.example/team/healthcheck:v1", "linux", "amd64", "", imageStore.ImageRuntimeConfig{
		Cmd: []string{"/bin/true"},
		Healthcheck: &container.HealthcheckConfig{
			Test:     []string{"CMD", "/image/health"},
			Interval: 12 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("import image with healthcheck: %v", err)
	}
	imageConfig := dockerImageConfig(image.RuntimeConfig)
	if health, ok := imageConfig["Healthcheck"].(*container.HealthcheckConfig); !ok ||
		health.Test[1] != "/image/health" {
		t.Fatalf("image inspect Healthcheck = %#v", imageConfig["Healthcheck"])
	}
	record, err := createContainerRecord("healthcheck-api-create", dockerCreateRequest{
		Image:       image.Reference,
		Healthcheck: json.RawMessage(`{"Test":["CMD","/container/health"],"Retries":6}`),
	})
	if err != nil {
		t.Fatalf("create container with healthcheck override: %v", err)
	}
	t.Cleanup(func() {
		current, err := state.Get(record.ID)
		if err != nil {
			t.Errorf("get healthcheck container for cleanup: %v", err)
			return
		}
		if current.Status == "running" {
			if err := state.Finish(record.ID, nil); err != nil {
				t.Errorf("finish healthcheck container: %v", err)
				return
			}
			current.Status = "exited"
		}
		if current.Status == "created" {
			err = state.RemoveCreated(record.ID)
		} else {
			err = state.Remove(record.ID)
		}
		if err != nil {
			t.Errorf("remove container: %v", err)
		}
	})
	if record.Options.Healthcheck == nil || record.Options.Healthcheck.Test[1] != "/container/health" ||
		record.Options.Healthcheck.Interval != container.DefaultHealthcheckInterval || record.Options.Healthcheck.Retries != 6 {
		t.Fatalf("container healthcheck = %+v", record.Options.Healthcheck)
	}
	if err := state.Start(record.ID, os.Getpid()); err != nil {
		t.Fatalf("start healthcheck container state: %v", err)
	}
	if err := state.SetHealth(record.ID, state.HealthState{
		Status: "healthy",
		Log:    []state.HealthLog{{Start: time.Now(), End: time.Now(), ExitCode: 0, Output: "ready"}},
	}); err != nil {
		t.Fatalf("persist container health status: %v", err)
	}
	persisted, err := state.Get(record.ID)
	if err != nil || persisted.Health == nil || persisted.Health.Status != "healthy" || len(persisted.Health.Log) != 1 {
		t.Fatalf("persisted container health = %+v, %v", persisted.Health, err)
	}
}
