package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	boxrcontainer "github.com/manit6752025/boxr/internal/container"
)

func TestParseDockerfileSupportedInstructions(t *testing.T) {
	spec, err := ParseDockerfile([]byte("# syntax=docker/dockerfile:1\nARG APP_ROOT=/opt/demo\nFROM scratch AS runtime\nARG APP_ROOT\nWORKDIR $APP_ROOT\nCOPY [\"server\", \"config/\", \".\"]\nENV APP_MODE=production GREETING=\"hello world\"\nLABEL org.example.component=api org.example.tier=\"edge service\"\nUSER 1000:1000\nENTRYPOINT [\"/opt/demo/server\"]\nCMD echo --serve --port 8080\nEXPOSE 80 443/tcp 5353/udp 5000-5002/udp\n"))
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
	for _, port := range []string{"443/tcp", "5000/udp", "5001/udp", "5002/udp", "5353/udp", "80/tcp"} {
		if _, ok := spec.Config.ExposedPorts[port]; !ok {
			t.Errorf("EXPOSE metadata missing %q: %#v", port, spec.Config.ExposedPorts)
		}
	}
}

func TestParseDockerfileRejectsUnsupportedInstructionsAndInvalidStageCopies(t *testing.T) {
	for _, input := range []string{
		"FROM scratch\nADD https://example.invalid/app.tar /app\n",
		"FROM scratch AS runtime\nCOPY --from=build /app /app\nFROM scratch AS build\n",
		"FROM scratch\nCOPY --from=bad!image /app /app\n",
		"FROM scratch AS runtime\nCOPY --from=registry.example/tools:1.2 ../../etc/passwd /etc/passwd\n",
		"FROM scratch AS scratch\nFROM scratch AS runtime\n",
	} {
		if _, err := ParseDockerfile([]byte(input)); err == nil {
			t.Errorf("unsupported Dockerfile was accepted: %q", input)
		}
	}
}

func TestParseDockerfileCapturesExternalImageCopyFromInSingleStage(t *testing.T) {
	spec, err := ParseDockerfile([]byte("FROM scratch AS runtime\nCOPY --from=registry.example/tools:1.2 /usr/bin/tool /usr/local/bin/tool\n"))
	if err != nil {
		t.Fatalf("ParseDockerfile external COPY --from: %v", err)
	}
	if len(spec.dockerfileStages) != 1 {
		t.Fatalf("stage executor captured %d stages, want one", len(spec.dockerfileStages))
	}
	if len(spec.dockerfileStages[0].steps) != 1 || spec.dockerfileStages[0].steps[0].copyFrom != "registry.example/tools:1.2" {
		t.Fatalf("external image COPY --from step = %#v", spec.dockerfileStages[0].steps)
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
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nWORKDIR /opt/demo\nCOPY . .\nENV MODE=production\nLABEL org.example.component=api\nUSER 1000:1000\nENTRYPOINT [\"/opt/demo/server\"]\nCMD [\"--serve\"]\nEXPOSE 8080/tcp\n"), 0600); err != nil {
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
	if _, ok := verified.RuntimeConfig.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("image EXPOSE metadata = %#v", verified.RuntimeConfig.ExposedPorts)
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

func TestParseDockerfileCapturesRunCommandsAndEnvironment(t *testing.T) {
	spec, err := ParseDockerfile([]byte("ARG BUILD_FLAVOR=debug\nFROM scratch\nARG BUILD_FLAVOR\nWORKDIR /src\nENV APP_MODE=release\nRUN echo \"$APP_MODE:$BUILD_FLAVOR\"\nRUN [\"/bin/tool\", \"$APP_MODE\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.dockerfileSteps) != 3 {
		t.Fatalf("captured %d Dockerfile steps, want WORKDIR and two RUNs", len(spec.dockerfileSteps))
	}
	var runs []dockerfileStep
	for _, step := range spec.dockerfileSteps {
		if step.kind == "run" {
			runs = append(runs, step)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("captured %d RUN steps, want 2", len(runs))
	}
	if len(runs[0].command) != 3 || runs[0].command[0] != "/bin/sh" || runs[0].command[1] != "-c" || !strings.Contains(runs[0].command[2], "$APP_MODE:$BUILD_FLAVOR") {
		t.Errorf("shell RUN = %#v", runs[0].command)
	}
	if !containsBuildEnv(runs[0].env, "APP_MODE", "release") || !containsBuildEnv(runs[0].env, "BUILD_FLAVOR", "debug") {
		t.Errorf("RUN environment did not capture ARG and ENV values: %#v", runs[0].env)
	}
	if strings.Join(runs[1].command, "|") != "/bin/tool|$APP_MODE" {
		t.Errorf("exec-form RUN = %#v", runs[1].command)
	}
}

func TestBuildDockerfileRunsInsideHardenedStagedRootfs(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "source.txt"), []byte("copy me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	content := "FROM scratch\nWORKDIR /build\nCOPY source.txt .\nENV BUILD_MODE=release\nARG TARGET=demo\nRUN echo \"$BUILD_MODE:$TARGET\"\n"
	if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	previous := runDockerfileCommand
	defer func() { runDockerfileCommand = previous }()

	var calls int
	runDockerfileCommand = func(command string, args []string, options boxrcontainer.Options) error {
		calls++
		if command != "/bin/sh" || len(args) != 2 || args[0] != "-c" {
			t.Errorf("RUN command = %q %#v", command, args)
		}
		if options.RootFS == "" || options.Workdir != "/build" {
			t.Errorf("RUN rootfs/workdir = %q / %q", options.RootFS, options.Workdir)
		}
		if options.NetworkMode != "none" || !options.UserNamespace || !options.MountProc || !options.NoNewPrivileges || options.SeccompProfile != "baseline" || !options.ClearEnv {
			t.Errorf("RUN isolation options = %+v", options)
		}
		if len(options.DropCapabilities) != 1 || options.DropCapabilities[0] != "ALL" {
			t.Errorf("RUN capabilities = %#v", options.DropCapabilities)
		}
		if containsBuildEnv(options.Env, "PATH", "/host/private/bin") || containsBuildEnv(options.Env, "HOST_SECRET", "must-not-leak") {
			t.Errorf("host environment leaked into RUN: %#v", options.Env)
		}
		if !containsBuildEnv(options.Env, "BUILD_MODE", "release") || !containsBuildEnv(options.Env, "TARGET", "demo") {
			t.Errorf("Dockerfile build environment missing: %#v", options.Env)
		}
		if err := os.WriteFile(filepath.Join(options.RootFS, "build", "result.txt"), []byte("built in Boxr\n"), 0644); err != nil {
			return err
		}
		return nil
	}

	t.Setenv("PATH", "/host/private/bin")
	t.Setenv("HOST_SECRET", "must-not-leak")
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:run", ""); err != nil {
		t.Fatalf("BuildDockerfileForPlatform: %v", err)
	}
	if calls != 1 {
		t.Fatalf("RUN invoked %d times, want 1", calls)
	}
	root := filepath.Join(t.TempDir(), "rootfs")
	if _, err := Unpack("demo:run", root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "build", "result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "built in Boxr\n" {
		t.Fatalf("build output = %q", got)
	}
}

func TestBuildDockerfileNetworkRequiresExplicitHostOptIn(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nRUN echo ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := runDockerfileCommand
	defer func() { runDockerfileCommand = previous }()
	var gotNetwork string
	runDockerfileCommand = func(_ string, _ []string, options boxrcontainer.Options) error {
		gotNetwork = options.NetworkMode
		return nil
	}
	if _, err := BuildDockerfileForPlatformWithNetwork(contextDir, dockerfile, "demo:hostnet", "", "host"); err != nil {
		t.Fatal(err)
	}
	if gotNetwork != "host" {
		t.Fatalf("explicit build network = %q, want host", gotNetwork)
	}
	if _, err := BuildDockerfileForPlatformWithNetwork(contextDir, dockerfile, "demo:badnet", "", "bridge"); err == nil {
		t.Fatal("unsupported build network mode was accepted")
	}
}

func TestBuildDockerfileDoesNotPublishImageWhenRunFails(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nRUN false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := runDockerfileCommand
	defer func() { runDockerfileCommand = previous }()
	runDockerfileCommand = func(_ string, _ []string, _ boxrcontainer.Options) error {
		return errors.New("simulated build command failure")
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:failed-run", ""); err == nil || !strings.Contains(err.Error(), "RUN 1") {
		t.Fatalf("failed RUN error = %v", err)
	}
	if _, err := Verify("demo:failed-run"); err == nil {
		t.Fatal("image was published even though RUN failed")
	}
}

func containsBuildEnv(values []string, key, want string) bool {
	for _, value := range values {
		name, actual, ok := strings.Cut(value, "=")
		if ok && name == key && actual == want {
			return true
		}
	}
	return false
}

func TestBuildDockerfileWithBuildArgsPassesOverridesOnlyToRun(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nARG BUILD_VERSION=default\nRUN echo \"$BUILD_VERSION\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := runDockerfileCommand
	defer func() { runDockerfileCommand = previous }()
	runDockerfileCommand = func(_ string, _ []string, options boxrcontainer.Options) error {
		if !containsBuildEnv(options.Env, "BUILD_VERSION", "1.4.2") {
			t.Errorf("RUN did not receive --build-arg override: %#v", options.Env)
		}
		return nil
	}
	if _, err := BuildDockerfileForPlatformWithOptions(contextDir, dockerfile, "demo:build-arg", "", "none", map[string]string{"BUILD_VERSION": "1.4.2"}); err != nil {
		t.Fatalf("Dockerfile build with build args: %v", err)
	}
	image, err := Verify("demo:build-arg")
	if err != nil {
		t.Fatal(err)
	}
	if containsBuildEnv(image.RuntimeConfig.Env, "BUILD_VERSION", "1.4.2") {
		t.Fatalf("build argument persisted in image environment: %#v", image.RuntimeConfig.Env)
	}
}

func TestParseDockerfileCopyChmodSupportsBoundedOctalModes(t *testing.T) {
	spec, err := ParseDockerfile([]byte("FROM scratch\nCOPY --chmod=0755 runner /usr/local/bin/runner\nCOPY --chmod=640 config /etc/app.conf\n"))
	if err != nil {
		t.Fatalf("ParseDockerfile with COPY --chmod: %v", err)
	}
	if len(spec.dockerfileSteps) != 2 {
		t.Fatalf("captured %d build steps, want 2", len(spec.dockerfileSteps))
	}
	if !spec.dockerfileSteps[0].hasCopyMode || spec.dockerfileSteps[0].copyMode != 0755 {
		t.Fatalf("first COPY mode = %04o (set=%t), want 0755", spec.dockerfileSteps[0].copyMode, spec.dockerfileSteps[0].hasCopyMode)
	}
	if !spec.dockerfileSteps[1].hasCopyMode || spec.dockerfileSteps[1].copyMode != 0640 {
		t.Fatalf("second COPY mode = %04o (set=%t), want 0640", spec.dockerfileSteps[1].copyMode, spec.dockerfileSteps[1].hasCopyMode)
	}
	jsonSpec, err := ParseDockerfile([]byte("FROM scratch\nCOPY --chmod=0755 [\"runner\", \"/usr/bin/runner\"]\n"))
	if err != nil || len(jsonSpec.dockerfileSteps) != 1 || jsonSpec.dockerfileSteps[0].copyMode != 0755 {
		t.Fatalf("JSON-form COPY --chmod parse = %#v, error=%v", jsonSpec.dockerfileSteps, err)
	}
	for _, mode := range []string{"7", "75", "888", "10000", "4755", "u+x", ""} {
		if _, err := ParseDockerfile([]byte("FROM scratch\nCOPY --chmod=" + mode + " runner /runner\n")); err == nil {
			t.Errorf("unsafe or malformed chmod mode %q was accepted", mode)
		}
	}
	for _, instruction := range []string{
		"COPY --chown=0:0 runner /runner",
		"ADD --chmod=0755 runner /runner",
		"COPY --chmod=0755 --chmod=0644 runner /runner",
	} {
		if _, err := ParseDockerfile([]byte("FROM scratch\n" + instruction + "\n")); err == nil {
			t.Errorf("unsupported COPY/ADD options were accepted: %s", instruction)
		}
	}
}

func TestParseDockerfileUsesGlobalBuildArgsAcrossStages(t *testing.T) {
	spec, err := ParseDockerfile([]byte("ARG BASE=scratch\nFROM ${BASE} AS builder\nFROM ${BASE} AS runtime\nCOPY --from=0 /app /app\n"))
	if err != nil {
		t.Fatalf("ParseDockerfile with global FROM ARG: %v", err)
	}
	if len(spec.dockerfileStages) != 2 || spec.dockerfileStages[0].base != "scratch" || spec.dockerfileStages[1].base != "scratch" {
		t.Fatalf("global FROM ARG was not expanded in each stage: %#v", spec.dockerfileStages)
	}
}

func TestParseDockerfileSupportsNamedAndIndexedStages(t *testing.T) {
	spec, err := ParseDockerfile([]byte("FROM scratch AS builder\nCOPY artifact /out/artifact\nFROM scratch AS runtime\nCOPY --from=builder /out/artifact /app/artifact\n"))
	if err != nil {
		t.Fatalf("ParseDockerfile multi-stage: %v", err)
	}
	if len(spec.dockerfileStages) != 2 || spec.dockerfileStages[0].name != "builder" || spec.dockerfileStages[1].name != "runtime" {
		t.Fatalf("parsed stages = %#v", spec.dockerfileStages)
	}
	last := spec.dockerfileStages[1].steps
	if len(last) != 1 || last[0].copyFrom != "builder" || last[0].source != "out/artifact" {
		t.Fatalf("COPY --from stage step = %#v", last)
	}
	indexed, err := ParseDockerfile([]byte("FROM scratch\nCOPY artifact /out/artifact\nFROM scratch\nCOPY --from=0 /out/artifact /app/artifact\n"))
	if err != nil || len(indexed.dockerfileStages) != 2 || indexed.dockerfileStages[1].steps[0].copyFrom != "0" {
		t.Fatalf("indexed COPY --from parsed %#v, error=%v", indexed.dockerfileStages, err)
	}
	for _, invalid := range []string{
		"FROM scratch AS bad\nCOPY --from=missing /file /file\n",
		"FROM scratch AS first\nCOPY --from=second /file /file\nFROM scratch AS second\n",
		"FROM scratch AS first\nFROM scratch AS first\n",
		"FROM scratch AS 0\n",
	} {
		if _, err := ParseDockerfile([]byte(invalid)); err == nil {
			t.Errorf("invalid multi-stage Dockerfile was accepted: %q", invalid)
		}
	}
}

func TestParseDockerfileSupportsLocalADDAndRejectsRemoteSources(t *testing.T) {
	spec, err := ParseDockerfile([]byte("FROM scratch\nADD payload.tar /opt/app/\nADD config.txt /etc/app.conf\n"))
	if err != nil {
		t.Fatal(err)
	}
	var addCount int
	for _, step := range spec.dockerfileSteps {
		if step.kind == "add" {
			addCount++
		}
	}
	if addCount != 2 {
		t.Fatalf("captured %d ADD steps, want 2: %#v", addCount, spec.dockerfileSteps)
	}
	for _, remote := range []string{
		"ADD https://example.invalid/app.tar /app\n",
		"ADD http://example.invalid/app.tar /app\n",
		"ADD git@github.com:team/project.git /src\n",
	} {
		if _, err := ParseDockerfile([]byte("FROM scratch\n" + remote)); err == nil || !strings.Contains(err.Error(), "remote ADD sources are not supported") {
			t.Errorf("remote ADD source %q was not rejected explicitly: %v", remote, err)
		}
	}
}

func TestBuildDockerfileADDExtractsPlainAndGzipTarArchives(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "plain"
		archiveName := "payload.tar"
		if compressed {
			name = "gzip"
			archiveName = "payload.tar.gz"
		}
		t.Run(name, func(t *testing.T) {
			useStore(t)
			contextDir := t.TempDir()
			archivePath := filepath.Join(contextDir, archiveName)
			writeDockerfileADDArchive(t, archivePath, compressed, []struct {
				name string
				data string
				mode int64
			}{
				{name: "bin/server", data: "binary payload\n", mode: 0755},
				{name: "etc/app.conf", data: "mode=production\n", mode: 0644},
			})
			if err := os.WriteFile(filepath.Join(contextDir, "plain.txt"), []byte("normal ADD file\n"), 0600); err != nil {
				t.Fatal(err)
			}
			dockerfile := filepath.Join(contextDir, "Dockerfile")
			content := "FROM scratch\nWORKDIR /opt/service\nADD " + archiveName + " .\nADD plain.txt /etc/plain.txt\n"
			if err := os.WriteFile(dockerfile, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:add-"+name, ""); err != nil {
				t.Fatalf("BuildDockerfileForPlatform: %v", err)
			}
			root := filepath.Join(t.TempDir(), "rootfs")
			if _, err := Unpack("demo:add-"+name, root); err != nil {
				t.Fatal(err)
			}
			for relative, expected := range map[string]string{
				"opt/service/bin/server":   "binary payload\n",
				"opt/service/etc/app.conf": "mode=production\n",
				"etc/plain.txt":            "normal ADD file\n",
			} {
				actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
				if err != nil {
					t.Errorf("read ADD result %s: %v", relative, err)
					continue
				}
				if string(actual) != expected {
					t.Errorf("%s = %q, want %q", relative, actual, expected)
				}
			}
		})
	}
}

func TestBuildDockerfileADDRejectsTarTraversalBeforePublishing(t *testing.T) {
	useStore(t)
	contextDir := t.TempDir()
	archivePath := filepath.Join(contextDir, "escape.tar")
	writeDockerfileADDArchive(t, archivePath, false, []struct {
		name string
		data string
		mode int64
	}{{name: "../escape.txt", data: "outside\n", mode: 0644}})
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nADD escape.tar /opt/app/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDockerfileForPlatform(contextDir, dockerfile, "demo:add-escape", ""); err == nil || !strings.Contains(err.Error(), "safely extract ADD archive") {
		t.Fatalf("path-traversing ADD archive error = %v", err)
	}
	if _, err := Verify("demo:add-escape"); err == nil {
		t.Fatal("ADD published an image after unsafe archive extraction")
	}
	if _, err := os.Lstat(filepath.Join(contextDir, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("ADD archive wrote outside the staged rootfs (error %v)", err)
	}
}

func writeDockerfileADDArchive(t *testing.T, filename string, compressed bool, entries []struct {
	name string
	data string
	mode int64
}) {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Typeflag: tar.TypeReg, Mode: entry.mode, Size: int64(len(entry.data)),
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	if compressed {
		var buffer bytes.Buffer
		compressor := gzip.NewWriter(&buffer)
		if _, err := compressor.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := compressor.Close(); err != nil {
			t.Fatal(err)
		}
		data = buffer.Bytes()
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
}
