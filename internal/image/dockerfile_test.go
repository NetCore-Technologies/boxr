package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDockerfileSupportedInstructions(t *testing.T) {
	spec, err := ParseDockerfile([]byte("# syntax=docker/dockerfile:1\nARG APP_ROOT=/opt/demo\nFROM scratch AS runtime\nWORKDIR $APP_ROOT\nCOPY [\"server\", \"config/\", \".\"]\nENV APP_MODE=production GREETING=\"hello world\"\nLABEL org.example.component=api org.example.tier=\"edge service\"\nUSER 1000:1000\nENTRYPOINT [\"/opt/demo/server\"]\nCMD echo --serve --port 8080\n"))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Base != "scratch" {
		t.Fatalf("Base = %q", spec.Base)
	}
	if spec.Config.WorkingDir != "/opt/demo" {
		t.Errorf("WorkingDir = %q", spec.Config.WorkingDir)
	}
	if len(spec.Copy) != 2 || spec.Copy[0].Source != "server" || spec.Copy[1].Destination != "/opt/demo/config" {
		t.Errorf("COPY instructions = %+v", spec.Copy)
	}
	if strings.Join(spec.Config.Env, "|") != "APP_MODE=production|GREETING=hello world" {
		t.Errorf("Env = %#v", spec.Config.Env)
	}
	if spec.Config.Labels["org.example.tier"] != "edge service" || spec.Config.User != "1000:1000" {
		t.Errorf("Labels/User = %#v / %q", spec.Config.Labels, spec.Config.User)
	}
	if strings.Join(spec.Config.Entrypoint, " ") != "/opt/demo/server" {
		t.Errorf("Entrypoint = %#v", spec.Config.Entrypoint)
	}
	if strings.Join(spec.Config.Cmd, " ") != "/bin/sh -c echo --serve --port 8080" {
		t.Errorf("Cmd = %#v", spec.Config.Cmd)
	}
}

func TestParseDockerfileRejectsRunAndMultiStage(t *testing.T) {
	for _, input := range []string{
		"FROM scratch\nRUN apk add curl\n",
		"FROM scratch AS build\nFROM scratch AS final\n",
		"FROM scratch\nADD app /app\n",
		"FROM scratch\nCOPY --from=build /app /app\n",
	} {
		if _, err := ParseDockerfile([]byte(input)); err == nil {
			t.Errorf("unsupported Dockerfile was accepted: %q", input)
		}
	}
}

func TestBuildDockerfileUsesDockerignoreAndPersistsConfig(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "server"), []byte("app binary\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "private.secret"), []byte("do not copy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nWORKDIR /opt/demo\nCOPY . .\nENV MODE=production\nLABEL org.example.component=api\nUSER 1000:1000\nENTRYPOINT [\"/opt/demo/server\"]\nCMD [\"--serve\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("*.secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	created, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:dockerfile", "")
	if err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	if created.Reference != "demo:dockerfile" {
		t.Fatalf("created reference = %q", created.Reference)
	}
	verified, err := Verify("demo:dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if verified.RuntimeConfig.WorkingDir != "/opt/demo" || verified.RuntimeConfig.User != "1000:1000" {
		t.Errorf("runtime config = %+v", verified.RuntimeConfig)
	}
	if strings.Join(verified.RuntimeConfig.Cmd, " ") != "--serve" || verified.RuntimeConfig.Entrypoint[0] != "/opt/demo/server" {
		t.Errorf("command config = %+v", verified.RuntimeConfig)
	}
	unpacked := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:dockerfile", unpacked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "opt", "demo", "server")); err != nil {
		t.Errorf("server was not copied: %v", err)
	}
	for _, excluded := range []string{"private.secret", "Dockerfile", ".dockerignore"} {
		if _, err := os.Lstat(filepath.Join(unpacked, "opt", "demo", excluded)); !os.IsNotExist(err) {
			t.Errorf("excluded build-context file %s was copied (err=%v)", excluded, err)
		}
	}
}

func TestLoadDockerfileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Dockerfile.real")
	if err := os.WriteFile(target, []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "Dockerfile")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDockerfile(link); err == nil {
		t.Fatal("symlinked Dockerfile was accepted")
	}
}
