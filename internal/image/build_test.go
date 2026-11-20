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
