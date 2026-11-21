package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBuildSpec(t *testing.T, dir string, spec BuildSpec) string {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Boxrfile.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildDockerfileCopyChmodAppliesModesInsideImageRootfs(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "tools"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(contextDir, "assets", "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(contextDir, "tools", "runner")
	if err := os.WriteFile(runnerPath, []byte("#!/bin/sh\necho ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assetPath := filepath.Join(contextDir, "assets", "nested", "config.ini")
	if err := os.WriteFile(assetPath, []byte("mode=production\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	content := "FROM scratch\nCOPY --chmod=0755 tools/runner /usr/local/bin/runner\nCOPY --chmod=0640 assets/ /opt/assets/\n"
	if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:copy-chmod", ""); err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	root := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:copy-chmod", root); err != nil {
		t.Fatalf("unpack image: %v", err)
	}
	for relative, want := range map[string]os.FileMode{
		"usr/local/bin/runner":         0755,
		"opt/assets/nested/config.ini": 0640,
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("stat %s: %v", relative, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", relative, got, want)
		}
	}
	if info, err := os.Stat(runnerPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("COPY --chmod modified host source mode: info=%v error=%v", info, err)
	}
}

func TestBuildDockerfileCopiesFromVerifiedLocalExternalImage(t *testing.T) {
	useStore(t)
	externalRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(externalRoot, "usr", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(externalRoot, "etc", "toolchain"), 0755); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(externalRoot, "usr", "bin", "compiler")
	if err := os.WriteFile(toolPath, []byte("verified toolchain binary\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(externalRoot, "etc", "toolchain", "version")
	if err := os.WriteFile(configPath, []byte("toolchain-v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(externalRoot, "demo:toolchain"); err != nil {
		t.Fatalf("import external source image: %v", err)
	}

	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	content := strings.Join([]string{
		"FROM scratch AS runtime",
		"COPY --from=demo:toolchain --chmod=0755 /usr/bin/compiler /usr/local/bin/compiler",
		"COPY --from=demo:toolchain /etc/toolchain/ /opt/toolchain/",
		"ENTRYPOINT [\"/usr/local/bin/compiler\"]",
		"",
	}, "\n")
	if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	built, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:external-copy", "")
	if err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	outputRoot := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack(built.Reference, outputRoot); err != nil {
		t.Fatalf("unpack final image: %v", err)
	}
	outputTool := filepath.Join(outputRoot, "usr", "local", "bin", "compiler")
	data, err := os.ReadFile(outputTool)
	if err != nil || string(data) != "verified toolchain binary\n" {
		t.Fatalf("external compiler contents = %q error=%v", data, err)
	}
	if info, err := os.Stat(outputTool); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("external compiler mode = %v error=%v, want 0755", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(outputRoot, "opt", "toolchain", "version")); err != nil || string(data) != "toolchain-v1\n" {
		t.Fatalf("external directory copy = %q error=%v", data, err)
	}
	if info, err := os.Stat(toolPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("external source image was modified by COPY --chmod: info=%v error=%v", info, err)
	}
	images, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("external source image or intermediate stage was unexpectedly changed/published: %#v", images)
	}
}

func TestBuildDockerfileExternalCopyFromRejectsMissingAndMismatchedImages(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch AS runtime\nCOPY --from=demo:missing /tool /tool\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:missing-copy", ""); err == nil || !strings.Contains(err.Error(), "must already exist in Boxr's verified local image store") {
		t.Fatalf("missing external image error = %v", err)
	}
	if _, err := Verify("demo:missing-copy"); err == nil {
		t.Fatal("build published an image after an external source image lookup failed")
	}

	otherArchitecture := "amd64"
	if runtimeArchitecture() == "amd64" {
		otherArchitecture = "arm64"
	}
	otherRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherRoot, "tool"), []byte("foreign platform\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFSWithConfig(otherRoot, "demo:foreign-tool", "linux", otherArchitecture, "", ImageRuntimeConfig{}); err != nil {
		t.Fatalf("import foreign platform test image: %v", err)
	}
	if err := os.WriteFile(dockerfile, []byte("FROM scratch AS runtime\nCOPY --from=demo:foreign-tool /tool /tool\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:foreign-copy", ""); err == nil || !strings.Contains(err.Error(), "does not match build target") {
		t.Fatalf("foreign external image platform error = %v", err)
	}
	if _, err := Verify("demo:foreign-copy"); err == nil {
		t.Fatal("build published an image from a platform-mismatched external source")
	}
}
func TestBuildDockerfileMultiStageCopiesArtifactsFromNamedAndIndexedStages(t *testing.T) {
	useStore(t)
	baseRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(baseRoot, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "etc", "base-release"), []byte("local base image\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(baseRoot, "demo:base"); err != nil {
		t.Fatalf("import local base image: %v", err)
	}

	contextDir := t.TempDir()
	artifactPath := filepath.Join(contextDir, "artifact")
	if err := os.WriteFile(artifactPath, []byte("compiled application payload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	content := strings.Join([]string{
		"FROM demo:base AS builder",
		"WORKDIR /build",
		"COPY artifact ./app",
		"FROM builder AS intermediate",
		"COPY --from=builder /build/app /opt/app",
		"FROM intermediate AS runtime",
		"COPY --from=1 --chmod=0755 /opt/app /usr/local/bin/app",
		"CMD [\"/usr/local/bin/app\"]",
		"",
	}, "\n")
	if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	created, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:multi-stage", "")
	if err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	if created.Reference != "demo:multi-stage" {
		t.Fatalf("built reference = %q", created.Reference)
	}
	root := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack(created.Reference, root); err != nil {
		t.Fatalf("unpack final image: %v", err)
	}
	baseMarker, err := os.ReadFile(filepath.Join(root, "etc", "base-release"))
	if err != nil || string(baseMarker) != "local base image\n" {
		t.Fatalf("FROM builder did not preserve inherited base files: contents=%q error=%v", baseMarker, err)
	}
	output := filepath.Join(root, "usr", "local", "bin", "app")
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read final-stage artifact: %v", err)
	}
	if string(data) != "compiled application payload\n" {
		t.Fatalf("final-stage file contents = %q", data)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Fatalf("final-stage COPY --chmod mode = %04o, want 0755", got)
	}
	if len(created.RuntimeConfig.Cmd) != 1 || created.RuntimeConfig.Cmd[0] != "/usr/local/bin/app" {
		t.Fatalf("final stage runtime CMD was not preserved: %#v", created.RuntimeConfig.Cmd)
	}
	images, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("intermediate stages were published as images: %#v", images)
	}
	seen := map[string]bool{}
	for _, item := range images {
		seen[item.Reference] = true
	}
	if !seen["demo:base"] || !seen["demo:multi-stage"] {
		t.Fatalf("expected only the explicit base and final image to be published: %#v", images)
	}
}

func TestBuildCopiesFilesAndPersistsRuntimeConfig(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "bin", "hello"), []byte("hello\n"), 0755); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{
		Version: 1,
		Base:    "scratch",
		Copy:    []CopyInstruction{{Source: "bin/hello", Destination: "/usr/local/bin/hello"}},
		Config: ImageRuntimeConfig{
			Env:        []string{"MODE=production"},
			Entrypoint: []string{"/usr/local/bin/hello"},
			Cmd:        []string{"--once"},
			WorkingDir: "/srv/app",
			Labels:     map[string]string{"org.example.component": "api"},
		},
	})
	created, err := Build(contextDir, specPath, "demo:built")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if created.Reference != "demo:built" || created.LayerBytes <= 0 {
		t.Fatalf("unexpected image metadata: %+v", created)
	}
	unpacked := filepath.Join(t.TempDir(), "unpacked")
	if _, err := Unpack("demo:built", unpacked); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for _, path := range []string{"usr/local/bin/hello", "srv/app"} {
		if _, err := os.Lstat(filepath.Join(unpacked, filepath.FromSlash(path))); err != nil {
			t.Errorf("expected build output %s: %v", path, err)
		}
	}
	if _, err := Verify("demo:built"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Verify that runtime metadata is actually encoded in the OCI config blob.
	dir, err := StoreDir()
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	var manifestDescriptor *Descriptor
	for i := range index.Manifests {
		if index.Manifests[i].Annotations["org.opencontainers.image.ref.name"] == "demo:built" {
			manifestDescriptor = &index.Manifests[i]
			break
		}
	}
	if manifestDescriptor == nil {
		t.Fatal("built image not found in OCI index")
	}
	manifest, err := loadManifest(dir, *manifestDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := readBlob(dir, manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	var stored ImageConfig
	if err := json.Unmarshal(configBytes, &stored); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stored.Config.Env, ",") != "MODE=production" || stored.Config.Entrypoint[0] != "/usr/local/bin/hello" || stored.Config.WorkingDir != "/srv/app" || stored.Config.Labels["org.example.component"] != "api" {
		t.Fatalf("runtime config was not preserved: %+v", stored.Config)
	}
}

func TestBuildFromLocalImageBaseInheritsAndOverridesRuntimeConfig(t *testing.T) {
	useStore(t)
	baseRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(baseRoot, "usr", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "etc-os-release"), []byte("BOXR_BASE=1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "usr", "bin", "base-entrypoint"), []byte("base binary\n"), 0755); err != nil {
		t.Fatal(err)
	}
	baseConfig := ImageRuntimeConfig{
		Env:        []string{"PATH=/usr/bin", "APP_MODE=base", "BASE_ONLY=kept"},
		Entrypoint: []string{"/usr/bin/base-entrypoint"},
		Cmd:        []string{"--base"},
		WorkingDir: "/srv/base",
		User:       "1000:1000",
		Labels: map[string]string{
			"org.example.base":   "kept",
			"org.example.shared": "base",
		},
	}
	if _, err := ImportRootFSWithConfig(baseRoot, "demo:base", "linux", runtimeArchitecture(), "", baseConfig); err != nil {
		t.Fatalf("import base image: %v", err)
	}

	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "web"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "web", "server"), []byte("child application\n"), 0755); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{
		Version: 1,
		Base:    "demo:base",
		Copy:    []CopyInstruction{{Source: "web", Destination: "/opt/web"}},
		Config: ImageRuntimeConfig{
			Env:        []string{"APP_MODE=production", "CHILD_ONLY=added"},
			Cmd:        []string{"--child"},
			WorkingDir: "/srv/child",
			Labels: map[string]string{
				"org.example.shared": "child",
				"org.example.child":  "added",
			},
		},
	})
	built, err := Build(contextDir, specPath, "demo:derived")
	if err != nil {
		t.Fatalf("Build from local base: %v", err)
	}
	if built.OS != "linux" || built.Arch != runtimeArchitecture() {
		t.Fatalf("derived image platform = %s/%s, want linux/%s", built.OS, built.Arch, runtimeArchitecture())
	}

	unpacked := filepath.Join(t.TempDir(), "derived-rootfs")
	if _, err := Unpack("demo:derived", unpacked); err != nil {
		t.Fatalf("unpack derived image: %v", err)
	}
	for path, want := range map[string]string{
		"etc-os-release":          "BOXR_BASE=1\n",
		"usr/bin/base-entrypoint": "base binary\n",
		"opt/web/server":          "child application\n",
	} {
		got, err := os.ReadFile(filepath.Join(unpacked, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("read inherited/copied file %s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if info, err := os.Stat(filepath.Join(unpacked, "srv", "child")); err != nil || !info.IsDir() {
		t.Fatalf("child WorkingDir was not created: info=%v error=%v", info, err)
	}

	verified, err := Verify("demo:derived")
	if err != nil {
		t.Fatalf("verify derived image: %v", err)
	}
	config := verified.RuntimeConfig
	wantEnv := []string{"PATH=/usr/bin", "APP_MODE=production", "BASE_ONLY=kept", "CHILD_ONLY=added"}
	if strings.Join(config.Env, "\x00") != strings.Join(wantEnv, "\x00") {
		t.Fatalf("inherited environment = %v, want %v", config.Env, wantEnv)
	}
	if strings.Join(config.Entrypoint, " ") != "/usr/bin/base-entrypoint" {
		t.Fatalf("inherited Entrypoint = %v", config.Entrypoint)
	}
	if strings.Join(config.Cmd, " ") != "--child" {
		t.Fatalf("overridden Cmd = %v", config.Cmd)
	}
	if config.WorkingDir != "/srv/child" || config.User != "1000:1000" {
		t.Fatalf("inherited/overridden runtime identity = workdir %q user %q", config.WorkingDir, config.User)
	}
	if config.Labels["org.example.base"] != "kept" || config.Labels["org.example.shared"] != "child" || config.Labels["org.example.child"] != "added" {
		t.Fatalf("base/child labels were not merged: %v", config.Labels)
	}
}

func TestBuildRejectsCopyThroughSymlinkSourcePath(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret")
	if err := os.WriteFile(outsideFile, []byte("must not be copied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(contextDir, "redirect")); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{
		Version: 1,
		Base:    "scratch",
		Copy:    []CopyInstruction{{Source: "redirect/secret", Destination: "/leaked"}},
	})
	if _, err := Build(contextDir, specPath, "demo:copy-symlink-source"); err == nil || !strings.Contains(err.Error(), "traverses symlink") {
		t.Fatalf("COPY through an intermediate source symlink returned %v, want explicit rejection", err)
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "must not be copied" {
		t.Fatalf("outside source was unexpectedly modified: data=%q error=%v", data, err)
	}
	if _, err := Inspect("demo:copy-symlink-source"); err == nil {
		t.Fatal("failed symlink traversal unexpectedly published an image")
	}
}

func TestBuildRejectsCopyThroughHostAbsoluteSymlink(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	links := filepath.Join(contextDir, "links")
	if err := os.Mkdir(links, 0755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(links, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("must stay inside rootfs"), 0644); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{
		Version: 1,
		Base:    "scratch",
		Copy: []CopyInstruction{
			{Source: "links", Destination: "/opt"},
			{Source: "payload", Destination: "/opt/redirect/escaped"},
		},
	})
	if _, err := Build(contextDir, specPath, "demo:symlink-escape"); err == nil {
		t.Fatal("COPY through a host-absolute symlink unexpectedly succeeded")
	}
	if _, err := os.Lstat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("COPY created a file outside the rootfs: err=%v", err)
	}
}

func TestBuildAllowsInRootSymlinkDestinations(t *testing.T) {
	useStore(t)
	baseRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(baseRoot, "usr", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "usr", "bin", "base"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin", filepath.Join(baseRoot, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(baseRoot, "demo:symlink-base"); err != nil {
		t.Fatalf("import base: %v", err)
	}
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "tool"), []byte("child"), 0755); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{
		Version: 1,
		Base:    "demo:symlink-base",
		Copy:    []CopyInstruction{{Source: "tool", Destination: "/bin/child"}},
	})
	if _, err := Build(contextDir, specPath, "demo:symlink-child"); err != nil {
		t.Fatalf("Build through an in-root symlink: %v", err)
	}
	unpacked := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:symlink-child", unpacked); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(unpacked, "usr", "bin", "child")); err != nil || string(data) != "child" {
		t.Fatalf("in-root symlink destination not written to canonical path: data=%q err=%v", data, err)
	}
	if info, err := os.Lstat(filepath.Join(unpacked, "bin")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("base /bin symlink was not preserved: info=%v err=%v", info, err)
	}
}

func TestBuildNormalizesContainerAbsoluteSymlinksForLaterWrites(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	for name, content := range map[string]string{
		"usrbin/base": "base",
		"tool":        "child",
	} {
		path := filepath.Join(contextDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	links := filepath.Join(contextDir, "links")
	if err := os.Mkdir(links, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin", filepath.Join(links, "bin")); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{
		Version: 1,
		Base:    "scratch",
		Copy: []CopyInstruction{
			{Source: "usrbin", Destination: "/usr/bin"},
			{Source: "links", Destination: "/"},
			{Source: "tool", Destination: "/bin/child"},
		},
	})
	if _, err := Build(contextDir, specPath, "demo:normalized-symlink"); err != nil {
		t.Fatalf("Build after normalizing container-absolute symlink: %v", err)
	}
	unpacked := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:normalized-symlink", unpacked); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(unpacked, "usr", "bin", "child")); err != nil || string(data) != "child" {
		t.Fatalf("later write did not resolve container symlink inside rootfs: data=%q err=%v", data, err)
	}
}

func TestBuildRejectsMissingLocalImageBaseWithoutPulling(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	specPath := writeBuildSpec(t, contextDir, BuildSpec{Version: 1, Base: "registry.example/base:latest"})
	_, err := Build(contextDir, specPath, "demo:missing-base")
	if err == nil || !strings.Contains(err.Error(), "base images must already exist locally") {
		t.Fatalf("Build error = %v, want explicit local-base error", err)
	}
}

func TestBuildRejectsBasePlatformMismatch(t *testing.T) {
	useStore(t)
	baseRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRoot, "base-file"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFSForPlatform(baseRoot, "demo:arm-base", "linux", "arm64", "v8"); err != nil {
		t.Fatalf("import ARM base: %v", err)
	}
	contextDir := t.TempDir()
	specPath := writeBuildSpec(t, contextDir, BuildSpec{Version: 1, Base: "demo:arm-base"})
	if _, err := BuildForPlatform(contextDir, specPath, "demo:amd64-child", "linux/amd64"); err == nil || !strings.Contains(err.Error(), "does not match build target") {
		t.Fatalf("BuildForPlatform error = %v, want platform mismatch", err)
	}
}

func TestValidateBuildSpecRejectsInvalidLocalBaseReference(t *testing.T) {
	err := ValidateBuildSpec(BuildSpec{Version: 1, Base: "../outside:latest"})
	if err == nil || !strings.Contains(err.Error(), "valid local image reference") {
		t.Fatalf("ValidateBuildSpec error = %v, want invalid local reference", err)
	}
}

func TestBuildIgnorePatternsExcludeFilesAndDirectories(t *testing.T) {
	contextDir := t.TempDir()
	for name, content := range map[string]string{
		"app/main":                      "keep",
		"app/dev.env":                   "secret",
		"app/node_modules/package.json": "dependency",
		"app/cache/data":                "cache",
	} {
		path := filepath.Join(contextDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(contextDir, ".boxrignore"), []byte("# keep secrets and generated dependencies out\n*.env\nnode_modules/\ncache/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patterns, err := loadIgnorePatterns(contextDir)
	if err != nil {
		t.Fatal(err)
	}
	for path, check := range map[string]struct {
		isDir bool
		want  bool
	}{
		"app/main":                      {want: false},
		"app/dev.env":                   {want: true},
		"app/node_modules/package.json": {want: true},
		"app/cache/data":                {want: true},
		"app/node_modules":              {isDir: true, want: true},
		"app/cache":                     {isDir: true, want: true},
	} {
		if got := ignoredPath(path, check.isDir, patterns); got != check.want {
			t.Errorf("ignoredPath(%q) = %v, want %v", path, got, check.want)
		}
	}
}

func TestBuildRejectsIgnoredExplicitSource(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "secret.env"), []byte("TOKEN=secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, ".boxrignore"), []byte("*.env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{Version: 1, Base: "scratch", Copy: []CopyInstruction{{Source: "secret.env", Destination: "/secret.env"}}})
	if _, err := Build(contextDir, specPath, "demo:ignored"); err == nil || !strings.Contains(err.Error(), "excluded by .boxrignore") {
		t.Fatalf("Build error = %v, want ignore error", err)
	}
}

func TestLoadIgnorePatternsRejectsUnsafeFilesAndRules(t *testing.T) {
	contextDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(outside, []byte("*.env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(contextDir, ".boxrignore")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIgnorePatterns(contextDir); err == nil {
		t.Fatal("symlinked .boxrignore unexpectedly accepted")
	}
	if err := os.Remove(filepath.Join(contextDir, ".boxrignore")); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"../outside\n", "!important.env\n", "[invalid\n"} {
		if err := os.WriteFile(filepath.Join(contextDir, ".boxrignore"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadIgnorePatterns(contextDir); err == nil {
			t.Errorf("unsafe pattern %q unexpectedly accepted", content)
		}
	}
}

func TestBuildDirectoryCopyHonorsBoxrIgnore(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	for name, content := range map[string]string{
		"app/main":                   "keep",
		"app/.env":                   "SECRET=value",
		"app/node_modules/lib/index": "generated",
	} {
		path := filepath.Join(contextDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(contextDir, ".boxrignore"), []byte("*.env\nnode_modules/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{Version: 1, Base: "scratch", Copy: []CopyInstruction{{Source: "app", Destination: "/opt/app"}}})
	if _, err := Build(contextDir, specPath, "demo:ignore-dir"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:ignore-dir", destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "opt/app/main")); err != nil {
		t.Fatalf("included application file missing: %v", err)
	}
	for _, excluded := range []string{"opt/app/.env", "opt/app/node_modules/lib/index"} {
		if _, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(excluded))); !os.IsNotExist(err) {
			t.Errorf("excluded path %s exists or returned unexpected error: %v", excluded, err)
		}
	}
}

func TestParseBuildPlatform(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		osName    string
		arch      string
		variant   string
		wantError bool
	}{
		{name: "amd64", input: "linux/amd64", osName: "linux", arch: "amd64"},
		{name: "arm64 v8", input: "linux/arm64/v8", osName: "linux", arch: "arm64", variant: "v8"},
		{name: "arm v7", input: "linux/arm/v7", osName: "linux", arch: "arm", variant: "v7"},
		{name: "arm v6", input: "linux/arm/v6", osName: "linux", arch: "arm", variant: "v6"},
		{name: "ppc64le", input: "linux/ppc64le", osName: "linux", arch: "ppc64le"},
		{name: "wrong OS", input: "windows/amd64", wantError: true},
		{name: "unknown arch", input: "linux/mips64", wantError: true},
		{name: "invalid variant", input: "linux/arm/v8", wantError: true},
		{name: "missing arch", input: "linux/", wantError: true},
		{name: "too many parts", input: "linux/arm/v7/extra", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			osName, arch, variant, err := parseBuildPlatform(test.input)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseBuildPlatform(%q) unexpectedly succeeded", test.input)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if osName != test.osName || arch != test.arch || variant != test.variant {
				t.Fatalf("platform = %s/%s/%s, want %s/%s/%s", osName, arch, variant, test.osName, test.arch, test.variant)
			}
		})
	}
}

func TestBuildForPlatformPersistsTargetMetadata(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "hello"), []byte("target independent rootfs"), 0644); err != nil {
		t.Fatal(err)
	}
	specPath := writeBuildSpec(t, contextDir, BuildSpec{Version: 1, Base: "scratch", Copy: []CopyInstruction{{Source: "hello", Destination: "/hello"}}})
	built, err := BuildForPlatform(contextDir, specPath, "demo:arm64", "linux/arm64/v8")
	if err != nil {
		t.Fatalf("BuildForPlatform: %v", err)
	}
	if built.OS != "linux" || built.Arch != "arm64" || built.Variant != "v8" {
		t.Fatalf("target platform metadata = %s/%s/%s", built.OS, built.Arch, built.Variant)
	}
	verified, err := Verify("demo:arm64")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified.OS != "linux" || verified.Arch != "arm64" || verified.Variant != "v8" {
		t.Fatalf("verified platform metadata = %s/%s/%s", verified.OS, verified.Arch, verified.Variant)
	}
}

func TestDiffImagesReportsAddedModifiedAndRemovedPaths(t *testing.T) {
	useStore(t)
	before := t.TempDir()
	if err := os.MkdirAll(filepath.Join(before, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(before, "etc", "config"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(before, "remove-me"), []byte("gone"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(before, "demo:before"); err != nil {
		t.Fatal(err)
	}
	after := t.TempDir()
	if err := os.MkdirAll(filepath.Join(after, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "etc", "config"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(after, "added"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRootFS(after, "demo:after"); err != nil {
		t.Fatal(err)
	}
	changes, err := Diff("demo:before", "demo:after")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := make(map[string]string, len(changes))
	for _, change := range changes {
		got[change.Path] = change.Status
	}
	for path, want := range map[string]string{"etc/config": "modified", "remove-me": "removed", "added": "added"} {
		if got[path] != want {
			t.Errorf("change %q = %q, want %q (all: %#v)", path, got[path], want, changes)
		}
	}
}

func TestDiffImagesRejectsMissingAndInvalidReferences(t *testing.T) {
	useStore(t)
	if _, err := Diff("../escape:latest", "demo:after"); err == nil {
		t.Fatal("invalid image reference unexpectedly accepted")
	}
	if _, err := Diff("demo:missing", "demo:after"); err == nil {
		t.Fatal("missing image unexpectedly accepted")
	}
}
