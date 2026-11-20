package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/manit6752025/boxr/internal/config"
	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/deploy"
	imageStore "github.com/manit6752025/boxr/internal/image"
	boxrnetwork "github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/registry"
	"github.com/manit6752025/boxr/internal/rootfs"
	"github.com/manit6752025/boxr/internal/state"
	"github.com/manit6752025/boxr/internal/support"
	"github.com/manit6752025/boxr/internal/volume"
)

// These values are populated by the release build's -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// These hooks keep live namespace connection testable without requiring
// privileged iproute2 and firewall access in the CLI unit-test suite.
var (
	joinExistingContainerNetwork   = boxrnetwork.Join
	leaveExistingContainerNetwork  = boxrnetwork.Leave
	pruneManagedNetworkAttachments = boxrnetwork.PruneStale
	verifyContainerNamespacePID    = verifyContainerRuntimePID
)

var (
	detachedReadyWriter *os.File
	detachedReadySent   bool
	launchCreatedContainer = func(executable string, args []string, stdout, stderr io.Writer) error {
		command := exec.Command(executable, args...)
		command.Stdout, command.Stderr = stdout, stderr
		return command.Run()
	}
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// The helper is PID 1 inside the newly-created namespaces. It waits for the
	// parent to attach it to its cgroup before it executes the requested command.
	if len(args) > 0 && args[0] == "__boxr-exec-launch" {
		return runExecLaunch(args[1:])
	}
	if len(args) > 0 && args[0] == "__boxr-exec" {
		return runExecChild(args[1:], stdout, stderr)
	}

	if len(args) > 0 && args[0] == "__boxr-init" {
		if err := container.Init(args[1:]); err != nil {
			var exitErr *container.ExitError
			if errors.As(err, &exitErr) {
				return exitErr.Code
			}
			fmt.Fprintf(stderr, "boxr init: %v\n", err)
			return 125
		}
		return 0
	}

	// Detached supervisors are separate Boxr processes. File descriptor 3 is
	// a one-shot startup handshake back to the launching CLI process.
	if len(args) > 0 && args[0] == "__boxr-detached-run" {
		if len(args) < 2 {
			fmt.Fprintln(stderr, "boxr detached runner: run arguments are required")
			return 125
		}
		detachedReadyWriter = os.NewFile(uintptr(3), "boxr-detached-ready")
		if detachedReadyWriter == nil {
			fmt.Fprintln(stderr, "boxr detached runner: startup handshake is unavailable")
			return 125
		}
		detachedReadySent = false
		code := runContainer(args[1:], stdout, stderr)
		if !detachedReadySent {
			_, _ = fmt.Fprintf(detachedReadyWriter, "ERROR %d\n", code)
		}
		_ = detachedReadyWriter.Close()
		detachedReadyWriter = nil
		return code
	}

	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "daemon":
		return runDaemon(args[1:], stdout, stderr)
	case "version", "--version":
		if len(args) == 2 && args[1] == "--json" {
			return writeJSON(stdout, map[string]string{"version": version, "commit": commit, "buildDate": buildDate, "goVersion": runtime.Version()}, stderr, "boxr version")
		}
		if len(args) > 1 {
			fmt.Fprintln(stderr, "Usage: boxr version [--json]")
			return 2
		}
		fmt.Fprintf(stdout, "boxr %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
		return 0
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "support-bundle":
		return runSupportBundle(args[1:], stdout, stderr)
	case "rootfs":
		return runRootFS(args[1:], stdout, stderr)
	case "image":
		return runImage(args[1:], stdout, stderr)
	case "pull":
		return runImage(append([]string{"pull"}, args[1:]...), stdout, stderr)
	case "push":
		return runImage(append([]string{"push"}, args[1:]...), stdout, stderr)
	case "features":
		return runFeatures(args[1:], stdout, stderr)
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "deploy":
		return runDeploy(args[1:], stdout, stderr)
	case "ps":
		return runPS(args[1:], stdout, stderr)
	case "stop":
		return runStop(args[1:], stdout, stderr)
	case "kill":
		return runKill(args[1:], stdout, stderr)
	case "wait":
		return runWait(args[1:], stdout, stderr)
	case "logs":
		return runLogs(args[1:], stdout, stderr)
	case "volume":
		return runVolume(args[1:], stdout, stderr)
	case "network":
		return runNetwork(args[1:], stdout, stderr)
	case "inspect":
		return runInspect(args[1:], stdout, stderr)
	case "rm":
		return runRemove(args[1:], stdout, stderr)
	case "stats":
		return runStats(args[1:], stdout, stderr)
	case "events":
		return runEvents(args[1:], stdout, stderr)
	case "info":
		return runInfo(args[1:], stdout, stderr)
	case "system":
		return runSystem(args[1:], stdout, stderr)
	case "prune":
		return runPrune(args[1:], stdout, stderr)
	case "run":
		return runContainer(args[1:], stdout, stderr)
	case "create":
		return runCreate(args[1:], stdout, stderr)
	case "start":
		return runStart(args[1:], stdout, stderr)
	case "exec":
		return runExec(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "boxr: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runContainer(args []string, stdout, stderr io.Writer) int {
	return runContainerMode(args, stdout, stderr, false)
}

func runCreate(args []string, stdout, stderr io.Writer) int {
	normalized, err := normalizeCreateArguments(args)
	if err != nil {
		fmt.Fprintf(stderr, "boxr create: %v\n", err)
		return 2
	}
	return runContainerMode(normalized, stdout, stderr, true)
}

func normalizeCreateArguments(args []string) []string {
	imageFlagProvided := false
	position := -1
	endOfOptions := false
	for index := 0; index < len(args); index++ {
		value := args[index]
		if value == "--" {
			endOfOptions = true
			if index+1 < len(args) {
				position = index + 1
			}
			break
		}
		if strings.HasPrefix(value, "--") {
			name, _, hasInlineValue := strings.Cut(value, "=")
			if name == "--image" {
				imageFlagProvided = true
			}
			if !hasInlineValue && createFlagTakesValue(name) {
				index++
			}
			continue
		}
		if strings.HasPrefix(value, "-") {
			if value == "-p" {
				index++
			}
			continue
		}
		position = index
		break
	}
	if imageFlagProvided || position < 0 || position >= len(args) {
		return args
	}
	image := args[position]
	if endOfOptions {
		result := append([]string(nil), args[:position-1]...)
		result = append(result, "--image", image, "--")
		return append(result, args[position+1:]...)
	}
	result := append([]string(nil), args[:position]...)
	result = append(result, "--image", image)
	return append(result, args[position+1:]...)
}

func createFlagTakesValue(name string) bool {
	switch name {
	case "--config", "--image", "--name", "--restart", "--restart-max-retries", "--restart-delay",
		"--hostname", "--memory", "--memory-high", "--memory-low", "--memory-min", "--memory-swap",
		"--cpus", "--cpu-weight", "--io-weight", "--pids-limit", "--workdir", "--rootfs",
		"--overlay-dir", "--mount", "--volume", "--tmpfs", "--network", "--network-name",
		"--publish", "--seccomp-profile", "--security-profile", "--resource-profile", "--cap-drop",
		"--env", "--env-file", "--label":
		return true
	default:
		return false
	}
}

func runContainerMode(args []string, stdout, stderr io.Writer, createOnly bool) int {
	var detached bool
	var opts container.Options
	var memory, memoryHigh, memoryLow, memoryMin, memorySwap string
	var restartDelay time.Duration
	var configPath string
	var imageReference string
	var configCommand []string
	var configEnv []string
	var env, envFiles, capDrop, mounts, tmpfs, labels, volumes, publish envList
	var dryRun bool
	for i, arg := range args {
		if strings.HasPrefix(arg, "--config=") {
			configPath = strings.TrimPrefix(arg, "--config=")
			break
		}
		if arg == "--config" && i+1 < len(args) {
			configPath = args[i+1]
			break
		}
	}
	if configPath != "" {
		loaded, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: config: %v\n", err)
			return 2
		}
		opts = loaded.Options
		if opts.Name == "" {
			opts.Name = loaded.Name
		}
		configCommand = loaded.Command
		configEnv = append(configEnv, loaded.Options.Env...)
		// Config metadata is the low-priority label source. Labels in the
		// options object override metadata, and explicit CLI --label flags
		// override both sources below.
		opts.Labels = mergeLabels(loaded.Metadata, opts.Labels)
	}
	restartDelay = time.Duration(opts.RestartDelayMS) * time.Millisecond
	if opts.MemoryLimitBytes > 0 {
		memory = strconv.FormatInt(opts.MemoryLimitBytes, 10)
	}
	if opts.MemoryHighBytes > 0 {
		memoryHigh = strconv.FormatInt(opts.MemoryHighBytes, 10)
	}
	if opts.MemoryLowBytes > 0 {
		memoryLow = strconv.FormatInt(opts.MemoryLowBytes, 10)
	}
	if opts.MemoryMinBytes > 0 {
		memoryMin = strconv.FormatInt(opts.MemoryMinBytes, 10)
	}
	if opts.MemorySwapMaxBytes > 0 {
		memorySwap = strconv.FormatInt(opts.MemorySwapMaxBytes, 10)
	}
	capDrop = append(capDrop, opts.DropCapabilities...)

	flagName := "run"
	if createOnly {
		flagName = "create"
	}
	flags := flag.NewFlagSet(flagName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var existingID string
	flags.StringVar(&existingID, "internal-existing-id", "", "internal Engine API existing container identity")
	flags.Usage = func() {
		if createOnly {
			fmt.Fprintln(stderr, "Usage: boxr create [options] IMAGE [COMMAND [ARG...]]")
		} else {
			fmt.Fprintln(stderr, "Usage: boxr run [options] [--] COMMAND [ARG...]")
		}
		fmt.Fprintln(stderr, "\nOptions:")
		flags.PrintDefaults()
	}
	flags.StringVar(&configPath, "config", configPath, "load versioned JSON run configuration")
	flags.StringVar(&imageReference, "image", "", "run from a verified local Boxr image (NAME:TAG)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate and print a redacted execution plan without starting a container")
	flags.BoolVar(&detached, "detach", false, "run the container in the background with persistent logs")
	flags.BoolVar(&detached, "d", false, "alias for --detach")
	flags.StringVar(&opts.Name, "name", opts.Name, "unique local container name")
	flags.Var(&labels, "label", "container metadata label (repeatable KEY=VALUE)")
	flags.BoolVar(&opts.AutoRemove, "rm", opts.AutoRemove, "remove the local record after the container exits")
	flags.StringVar(&opts.RestartPolicy, "restart", opts.RestartPolicy, "restart policy: no, on-failure, always, or unless-stopped (requires --detach)")
	flags.IntVar(&opts.RestartMaxRetries, "restart-max-retries", opts.RestartMaxRetries, "maximum on-failure restarts (0 means unlimited)")
	flags.DurationVar(&restartDelay, "restart-delay", restartDelay, "delay between restart attempts (100ms to 5m; default 1s)")
	flags.StringVar(&opts.Hostname, "hostname", opts.Hostname, "hostname visible inside the container")
	flags.StringVar(&memory, "memory", "", "hard memory limit (for example: 256m, 1g, or 536870912)")
	flags.StringVar(&memoryHigh, "memory-high", "", "memory pressure threshold (cgroup v2 memory.high)")
	flags.StringVar(&memoryLow, "memory-low", "", "best-effort memory protection (cgroup v2 memory.low)")
	flags.StringVar(&memoryMin, "memory-min", "", "hard memory protection (cgroup v2 memory.min)")
	flags.StringVar(&memorySwap, "memory-swap", "", "maximum swap usage in bytes (cgroup v2 memory.swap.max)")
	flags.Float64Var(&opts.CPUs, "cpus", opts.CPUs, "CPU quota in cores (for example: 0.5 or 2)")
	flags.IntVar(&opts.CPUWeight, "cpu-weight", opts.CPUWeight, "relative CPU weight from 1 to 10000")
	flags.IntVar(&opts.IOWeight, "io-weight", opts.IOWeight, "relative block I/O weight from 1 to 10000")
	flags.Int64Var(&opts.PidsLimit, "pids-limit", opts.PidsLimit, "maximum number of tasks in the container cgroup")
	flags.BoolVar(&opts.MemoryOOMGroup, "oom-group", opts.MemoryOOMGroup, "kill the container cgroup as a unit on memory OOM")
	flags.StringVar(&opts.Workdir, "workdir", opts.Workdir, "working directory for the command inside the container")
	flags.StringVar(&opts.RootFS, "rootfs", opts.RootFS, "directory to use as the container root filesystem")
	flags.StringVar(&opts.OverlayDir, "overlay-dir", opts.OverlayDir, "persistent OverlayFS upper/work storage (requires --rootfs)")
	flags.BoolVar(&opts.ReadOnlyRootFS, "read-only-rootfs", opts.ReadOnlyRootFS, "remount the root filesystem read-only")
	flags.BoolVar(&opts.MountProc, "mount-proc", opts.MountProc, "mount a fresh proc filesystem inside rootfs")
	flags.Var(&mounts, "mount", "bind mount SOURCE:TARGET[:ro|rw] (repeatable; requires --rootfs)")
	flags.Var(&volumes, "volume", "mount a named volume as NAME:TARGET[:ro|rw] (repeatable)")
	flags.Var(&tmpfs, "tmpfs", "mount tmpfs at TARGET[:SIZE[:MODE]] (repeatable; requires --rootfs)")
	flags.StringVar(&opts.NetworkMode, "network", opts.NetworkMode, "network namespace mode: host, none, or bridge")
	flags.StringVar(&opts.NetworkName, "network-name", opts.NetworkName, "managed bridge network to join when --network=bridge")
	flags.Var(&publish, "publish", "publish host port to a container port: [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp|udp] (repeatable)")
	flags.Var(&publish, "p", "alias for --publish (repeatable)")
	flags.BoolVar(&opts.UserNamespace, "userns", opts.UserNamespace, "map the invoking UID/GID to root inside a private user namespace")
	flags.BoolVar(&opts.NoNewPrivileges, "no-new-privileges", opts.NoNewPrivileges, "prevent execve from gaining additional privileges")
	flags.StringVar(&opts.SeccompProfile, "seccomp-profile", opts.SeccompProfile, "built-in syscall filter profile: baseline")
	flags.StringVar(&opts.SecurityProfile, "security-profile", opts.SecurityProfile, "mandatory security preset: default, baseline, restricted, or untrusted")
	flags.StringVar(&opts.ResourceProfile, "resource-profile", opts.ResourceProfile, "resource defaults: default, micro, small, medium, or large")
	flags.Var(&capDrop, "cap-drop", "drop a Linux capability by name or ALL (repeatable)")
	flags.Var(&env, "env", "environment assignment (repeatable, KEY=VALUE)")
	flags.Var(&envFiles, "env-file", "load KEY=VALUE entries from a file (repeatable)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if detached && dryRun {
		fmt.Fprintln(stderr, "boxr run: --detach cannot be combined with --dry-run")
		return 2
	}
	if createOnly && (detached || dryRun) {
		fmt.Fprintln(stderr, "boxr create: creation cannot be combined with --detach or --dry-run")
		return 2
	}
	if createOnly && existingID != "" {
		fmt.Fprintln(stderr, "boxr create: internal existing-container mode is not available")
		return 2
	}
	if createOnly && imageReference == "" {
		fmt.Fprintln(stderr, "boxr create: an image is required (use boxr pull first, then pass IMAGE)")
		flags.Usage()
		return 2
	}
	if existingID != "" {
		if !detached || dryRun {
			fmt.Fprintln(stderr, "boxr run: internal existing-ID mode requires --detach and cannot use --dry-run")
			return 2
		}
		existingRecord, getErr := state.Get(existingID)
		if getErr != nil {
			fmt.Fprintf(stderr, "boxr run: load existing container %s: %v\n", existingID, getErr)
			return 1
		}
		if existingRecord.Status != "created" && existingRecord.Status != "exited" && existingRecord.Status != "failed" {
			fmt.Fprintf(stderr, "boxr run: existing container %s must be created or stopped (state: %s)\n", existingID, existingRecord.Status)
			return 1
		}
		savedImage := existingRecord.Labels["io.boxr.image"]
		if savedImage == "" {
			fmt.Fprintln(stderr, "boxr run: existing Engine API container has no saved image reference")
			return 1
		}
		if imageReference != "" && imageReference != savedImage {
			fmt.Fprintln(stderr, "boxr run: image reference does not match the created container")
			return 1
		}
		configEnvironment := append([]string(nil), opts.Env...)
		opts = existingRecord.Options
		opts.Env = configEnvironment
		opts.Labels = existingRecord.Labels
		opts.Name = existingRecord.Name
		imageReference = savedImage
	}
	if restartDelay != 0 {
		if restartDelay%time.Millisecond != 0 {
			fmt.Fprintln(stderr, "boxr run: --restart-delay must use whole milliseconds")
			return 2
		}
		opts.RestartDelayMS = int(restartDelay / time.Millisecond)
	}
	providedCommand := flags.Args()
	if len(providedCommand) == 0 {
		providedCommand = append([]string(nil), configCommand...)
	}
	commandArgs := providedCommand
	var imageRuntimeConfig imageStore.ImageRuntimeConfig
	if imageReference != "" && opts.RootFS != "" {
		fmt.Fprintln(stderr, "boxr run: --image cannot be combined with --rootfs")
		return 2
	}
	if imageReference != "" {
		imageInfo, err := imageStore.Inspect(imageReference)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: inspect image %s: %v\n", imageReference, err)
			return 1
		}
		imageRuntimeConfig = imageInfo.RuntimeConfig
		if imageRuntimeConfig.User != "" {
			fmt.Fprintf(stderr, "boxr run: image %s declares User=%q, but image user switching is not supported yet; use a rootfs/configuration without User metadata\n", imageReference, imageRuntimeConfig.User)
			return 2
		}
		if opts.Workdir == "" {
			opts.Workdir = imageRuntimeConfig.WorkingDir
		}
		if len(imageRuntimeConfig.Entrypoint) > 0 {
			imageArguments := providedCommand
			if len(imageArguments) == 0 {
				imageArguments = imageRuntimeConfig.Cmd
			}
			commandArgs = append(append([]string(nil), imageRuntimeConfig.Entrypoint...), imageArguments...)
		} else if len(providedCommand) == 0 {
			commandArgs = append([]string(nil), imageRuntimeConfig.Cmd...)
		}
		opts.Labels = mergeLabels(imageRuntimeConfig.Labels, opts.Labels)
	}
	if len(commandArgs) == 0 {
		fmt.Fprintln(stderr, "boxr run: a command is required (provide one after the options or in the config)")
		flags.Usage()
		return 2
	}

	var imageStage string
	if imageReference != "" {
		if opts.RootFS != "" {
			fmt.Fprintln(stderr, "boxr run: --image cannot be combined with --rootfs")
			return 2
		}
		if _, err := imageStore.Verify(imageReference); err != nil {
			fmt.Fprintf(stderr, "boxr run: verify image %s: %v\n", imageReference, err)
			return 1
		}
		if dryRun {
			// A stable absolute placeholder is sufficient for option validation;
			// dry-run never creates or extracts a rootfs.
			opts.RootFS = filepath.Join(os.TempDir(), "boxr-image-dry-run")
		} else if createOnly {
			// The created record stores the verified image reference. Its
			// rootfs is unpacked only when start launches the container.
			opts.RootFS = filepath.Join(os.TempDir(), "boxr-image-create-validation")
		} else {
			var err error
			imageStage, err = os.MkdirTemp("", "boxr-run-image-*")
			if err != nil {
				fmt.Fprintf(stderr, "boxr run: create image staging directory: %v\n", err)
				return 1
			}
			defer os.RemoveAll(imageStage)
			rootPath := filepath.Join(imageStage, "rootfs")
			if _, err := imageStore.Unpack(imageReference, rootPath); err != nil {
				fmt.Fprintf(stderr, "boxr run: unpack image %s: %v\n", imageReference, err)
				return 1
			}
			opts.RootFS = rootPath
		}
	}

	if memory != "" {
		limit, err := parseMemoryLimit(memory)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid --memory: %v\n", err)
			return 2
		}
		opts.MemoryLimitBytes = limit
	}
	for _, setting := range []struct {
		name, value string
		destination *int64
	}{
		{"--memory-high", memoryHigh, &opts.MemoryHighBytes},
		{"--memory-low", memoryLow, &opts.MemoryLowBytes},
		{"--memory-min", memoryMin, &opts.MemoryMinBytes},
		{"--memory-swap", memorySwap, &opts.MemorySwapMaxBytes},
	} {
		if setting.value == "" {
			continue
		}
		limit, err := parseMemoryLimit(setting.value)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid %s: %v\n", setting.name, err)
			return 2
		}
		*setting.destination = limit
	}
	for _, value := range mounts {
		mount, err := parseBindMount(value)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid --mount: %v\n", err)
			return 2
		}
		opts.BindMounts = append(opts.BindMounts, mount)
	}
	for _, value := range volumes {
		name, mount, err := parseVolumeMount(value)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid --volume: %v\n", err)
			return 2
		}
		managed, err := volume.Get(name)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: volume %q: %v\n", name, err)
			return 2
		}
		mount.Source = managed.Path
		opts.BindMounts = append(opts.BindMounts, mount)
	}
	for _, value := range tmpfs {
		mount, err := parseTmpfsMount(value)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid --tmpfs: %v\n", err)
			return 2
		}
		opts.TmpfsMounts = append(opts.TmpfsMounts, mount)
	}
	opts.DropCapabilities = append([]string(nil), capDrop...)
	if opts.CPUs < 0 || math.IsNaN(opts.CPUs) || math.IsInf(opts.CPUs, 0) {
		fmt.Fprintln(stderr, "boxr run: --cpus must be finite and non-negative (0 disables CPU limits)")
		return 2
	}
	if opts.Hostname != "" && (len(opts.Hostname) > 64 || strings.ContainsRune(opts.Hostname, '\x00')) {
		fmt.Fprintln(stderr, "boxr run: --hostname must be at most 64 bytes and contain no NUL characters")
		return 2
	}
	if strings.ContainsRune(opts.Workdir, '\x00') {
		fmt.Fprintln(stderr, "boxr run: --workdir cannot contain a NUL character")
		return 2
	}
	for _, entry := range env {
		if err := validateEnv(entry); err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid --env: %v\n", err)
			return 2
		}
	}
	fileEnv := make([]string, 0)
	for _, path := range envFiles {
		entries, err := readEnvFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: env file %s: %v\n", path, err)
			return 2
		}
		fileEnv = append(fileEnv, entries...)
	}
	for _, value := range publish {
		mapping, err := parsePortMapping(value)
		if err != nil {
			fmt.Fprintf(stderr, "boxr run: invalid --publish: %v\n", err)
			return 2
		}
		opts.PublishedPorts = append(opts.PublishedPorts, mapping)
	}
	opts.Env = mergeEnvValues(imageRuntimeConfig.Env, configEnv, fileEnv, []string(env))
	if len(labels) > 0 {
		if opts.Labels == nil {
			opts.Labels = make(map[string]string)
		}
		for _, label := range labels {
			key, value, ok := strings.Cut(label, "=")
			if !ok || strings.TrimSpace(key) == "" {
				fmt.Fprintf(stderr, "boxr run: invalid --label %q (expected KEY=VALUE)\n", label)
				return 2
			}
			opts.Labels[key] = value
		}
	}
	if imageReference != "" {
		if opts.Labels == nil {
			opts.Labels = make(map[string]string)
		}
		opts.Labels["io.boxr.image"] = imageReference
	}
	if err := opts.ApplyResourceProfile(); err != nil {
		fmt.Fprintf(stderr, "boxr run: resource profile: %v\n", err)
		return 2
	}
	if err := opts.ApplySecurityProfile(); err != nil {
		fmt.Fprintf(stderr, "boxr run: security profile: %v\n", err)
		return 2
	}
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(stderr, "boxr run: %v\n", err)
		return 2
	}
	if opts.RestartPolicy != "" && opts.RestartPolicy != "no" &&
		!detached && detachedReadyWriter == nil && !dryRun && !createOnly {
		fmt.Fprintln(stderr, "boxr run: restart policies require --detach")
		return 2
	}
	if opts.RestartPolicy != "" && opts.RestartPolicy != "no" && opts.AutoRemove {
		fmt.Fprintln(stderr, "boxr run: restart policies cannot be combined with --rm")
		return 2
	}
	if createOnly {
		// The record keeps runtime-only mount settings, while the private
		// sidecar is a safe bootstrap config that can be loaded before the
		// image rootfs has been unpacked.
		opts.RootFS = ""
	}
	if dryRun {
		plan := buildRunPlan(commandArgs, opts)
		plan.ImageReference = imageReference
		return writeJSON(stdout, plan, stderr, "boxr run --dry-run")
	}
	if detached && detachedReadyWriter == nil {
		return runDetachedContainer(args, stdout, stderr)
	}

	var record state.Record
	var err error
	if existingID != "" {
		record, err = state.Get(existingID)
		if err == nil && record.Status != "created" && record.Status != "exited" && record.Status != "failed" {
			err = fmt.Errorf("existing container is no longer created or stopped (state: %s)", record.Status)
		}
		if err == nil && !equalStringSlices(commandArgs, record.Command) {
			err = fmt.Errorf("resolved image command does not match the created container command")
		}
	} else {
		record, err = state.Create(opts.Name, commandArgs, opts, opts.Labels)
	}
	if err != nil {
		fmt.Fprintf(stderr, "boxr run: state: %v\n", err)
		return 1
	}
	if createOnly {
		savedOptions := opts
		savedOptions.Env = append([]string(nil), opts.Env...)
		savedOptions.BindMounts = nil
		savedOptions.ReadOnlyRootFS = false
		savedOptions.MountProc = false
		savedOptions.TmpfsMounts = nil
		savedOptions.OverlayDir = ""
		runFile := config.RunFile{
			Version: 1, Name: record.Name, Command: providedCommand,
			Options: savedOptions, Metadata: record.Labels,
		}
		configData, saveErr := json.Marshal(runFile)
		if saveErr == nil {
			saveErr = state.SaveEngineConfig(record.ID, configData)
		}
		if saveErr != nil {
			_ = state.RemoveCreated(record.ID)
			fmt.Fprintf(stderr, "boxr create: persist private start configuration: %v\n", saveErr)
			return 1
		}
		fmt.Fprintf(stdout, "created %s\n", record.ID)
		return 0
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		if existingID == "" {
			_ = state.RemoveCreated(record.ID)
		}
		fmt.Fprintf(stderr, "boxr run: log setup: %v\n", err)
		return 1
	}
	defer logFile.Close()
	combinedLog := io.MultiWriter(logFile)
	startedAny := false
	var runErr error
	for {
		runErr = container.RunWithHooks(commandArgs[0], commandArgs[1:], opts, container.Hooks{
			Stdout: io.MultiWriter(stdout, combinedLog),
			Stderr: io.MultiWriter(stderr, combinedLog),
			OnStart: func(pid int) error {
				if opts.NetworkMode == "bridge" {
					if _, err := boxrnetwork.Join(opts.NetworkName, pid, record.ID, opts.PublishedPorts...); err != nil {
						return fmt.Errorf("attach bridge network %q: %w", opts.NetworkName, err)
					}
				}
				if err := state.Start(record.ID, pid); err != nil {
					_ = boxrnetwork.LeaveAll(record.ID)
					return err
				}
				startedAny = true
				return nil
			},
			OnReady: func(pid int) {
				if detachedReadyWriter != nil && !detachedReadySent {
					_, _ = fmt.Fprintf(detachedReadyWriter, "READY %s %d\n", record.ID, pid)
					detachedReadySent = true
					_ = detachedReadyWriter.Close()
					detachedReadyWriter = nil
				}
			},
			OnExit: func(_ int, executionErr error) error {
				var networkErr error
				if opts.NetworkMode == "bridge" || opts.NetworkMode == "none" {
					networkErr = boxrnetwork.LeaveAll(record.ID)
				}
				stateErr := state.Finish(record.ID, executionErr)
				if networkErr != nil && stateErr != nil {
					return fmt.Errorf("disconnect bridge network: %v; finish container record: %w", networkErr, stateErr)
				}
				if networkErr != nil {
					return fmt.Errorf("disconnect bridge network: %w", networkErr)
				}
				return stateErr
			},
		})

		// RunWithHooks can fail before its exit callback is installed (for
		// example, when clone or user-namespace setup fails). Persist that
		// attempt too, so on-failure policies can reason about the exit.
		current, stateErr := state.Get(record.ID)
		if stateErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("read container restart state: %w", stateErr))
			break
		}
		if current.Status == "created" || current.Status == "restarting" || current.Status == "running" {
			if finishErr := state.Finish(record.ID, runErr); finishErr != nil {
				runErr = errors.Join(runErr, fmt.Errorf("record container attempt exit: %w", finishErr))
				break
			}
			current, _ = state.Get(record.ID)
		}

		if !restartPolicyApplies(opts, current) {
			break
		}
		if err := state.PrepareRestart(record.ID); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("prepare container restart: %w", err))
			break
		}
		delay := time.Duration(opts.RestartDelayMS) * time.Millisecond
		if delay == 0 {
			delay = time.Second
		}
		if !waitForContainerRestart(record.ID, delay) {
			break
		}
	}

	if existingID == "" && !startedAny && (opts.RestartPolicy == "" || opts.RestartPolicy == "no") {
		_ = state.RemoveCreated(record.ID)
	}
	if opts.AutoRemove && startedAny {
		if removeErr := state.Remove(record.ID); removeErr != nil {
			fmt.Fprintf(stderr, "boxr run: auto-remove: %v\n", removeErr)
		}
	}
	if runErr != nil {
		var boxrExit *container.ExitError
		if errors.As(runErr, &boxrExit) {
			return boxrExit.Code
		}
		var execExit *exec.ExitError
		if errors.As(runErr, &execExit) && execExit.ExitCode() >= 0 {
			return execExit.ExitCode()
		}
		fmt.Fprintf(stderr, "boxr: %v\n", runErr)
		return 1
	}
	if !opts.AutoRemove {
		fmt.Fprintf(stdout, "container %s", record.ID)
		if record.Name != "" {
			fmt.Fprintf(stdout, " (%s)", record.Name)
		}
		fmt.Fprintln(stdout, " exited successfully")
	}
	return 0
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func restartPolicyApplies(options container.Options, record state.Record) bool {
	if record.StopRequested {
		return false
	}
	switch restartPolicyName(options.RestartPolicy) {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		if record.ExitCode == nil || *record.ExitCode == 0 {
			return false
		}
		return options.RestartMaxRetries == 0 || record.RestartCount < options.RestartMaxRetries
	default:
		return false
	}
}

func waitForContainerRestart(id string, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := state.Get(id)
		if err != nil || record.StopRequested || record.Status != "restarting" {
			return false
		}
		select {
		case <-timer.C:
			record, err := state.Get(id)
			return err == nil && !record.StopRequested && record.Status == "restarting"
		case <-ticker.C:
		}
	}
}

type runPlan struct {
	Mode                 string                  `json:"mode"`
	Command              string                  `json:"command"`
	ArgumentCount        int                     `json:"argumentCount"`
	ContainerName        string                  `json:"containerName,omitempty"`
	ImageReference       string                  `json:"imageReference,omitempty"`
	WorkingDirectory     string                  `json:"workingDirectory,omitempty"`
	RootFSConfigured     bool                    `json:"rootfsConfigured"`
	OverlayConfigured    bool                    `json:"overlayConfigured"`
	NetworkMode          string                  `json:"networkMode"`
	NetworkName          string                  `json:"networkName,omitempty"`
	PublishedPorts       []container.PortMapping `json:"publishedPorts,omitempty"`
	RestartPolicy        string                  `json:"restartPolicy,omitempty"`
	RestartMaxRetries    int                     `json:"restartMaxRetries,omitempty"`
	RestartDelayMS       int                     `json:"restartDelayMs,omitempty"`
	MemoryLimitBytes     int64                   `json:"memoryLimitBytes,omitempty"`
	MemoryHighBytes      int64                   `json:"memoryHighBytes,omitempty"`
	CPUs                 float64                 `json:"cpus,omitempty"`
	CPUWeight            int                     `json:"cpuWeight,omitempty"`
	IOWeight             int                     `json:"ioWeight,omitempty"`
	PIDsLimit            int64                   `json:"pidsLimit,omitempty"`
	BindMountCount       int                     `json:"bindMountCount"`
	TmpfsMountCount      int                     `json:"tmpfsMountCount"`
	EnvironmentKeys      []string                `json:"environmentKeys"`
	EnvironmentValues    string                  `json:"environmentValues"`
	LabelKeys            []string                `json:"labelKeys"`
	LabelValues          string                  `json:"labelValues"`
	DropCapabilityCount  int                     `json:"dropCapabilityCount"`
	UserNamespace        bool                    `json:"userNamespace"`
	NoNewPrivileges      bool                    `json:"noNewPrivileges"`
	SeccompProfile       string                  `json:"seccompProfile,omitempty"`
	SecurityProfile      string                  `json:"securityProfile,omitempty"`
	ResourceProfile      string                  `json:"resourceProfile,omitempty"`
	NoContainerStateMade bool                    `json:"noContainerStateCreated"`
}

func restartPolicyName(value string) string {
	if value == "" {
		return "no"
	}
	return value
}

func buildRunPlan(command []string, opts container.Options) runPlan {
	environmentKeys := make(map[string]struct{})
	for _, entry := range opts.Env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environmentKeys[key] = struct{}{}
		}
	}
	labelKeys := make([]string, 0, len(opts.Labels))
	for key := range opts.Labels {
		labelKeys = append(labelKeys, key)
	}
	envKeys := make([]string, 0, len(environmentKeys))
	for key := range environmentKeys {
		envKeys = append(envKeys, key)
	}
	sort.Strings(labelKeys)
	sort.Strings(envKeys)
	plan := runPlan{
		Mode: "dry-run", Command: command[0], ArgumentCount: len(command) - 1,
		ContainerName: opts.Name, WorkingDirectory: opts.Workdir, RootFSConfigured: opts.RootFS != "", OverlayConfigured: opts.OverlayDir != "",
		NetworkMode: func() string {
			if opts.NetworkMode == "" {
				return "host"
			}
			return opts.NetworkMode
		}(), NetworkName: opts.NetworkName, PublishedPorts: normalizePublishedPorts(opts.PublishedPorts), RestartPolicy: restartPolicyName(opts.RestartPolicy), RestartMaxRetries: opts.RestartMaxRetries, RestartDelayMS: opts.RestartDelayMS, MemoryLimitBytes: opts.MemoryLimitBytes,
		MemoryHighBytes: opts.MemoryHighBytes, CPUs: opts.CPUs, CPUWeight: opts.CPUWeight,
		IOWeight: opts.IOWeight, PIDsLimit: opts.PidsLimit, BindMountCount: len(opts.BindMounts),
		TmpfsMountCount: len(opts.TmpfsMounts), EnvironmentKeys: envKeys, EnvironmentValues: "redacted",
		LabelKeys: labelKeys, LabelValues: "redacted", DropCapabilityCount: len(opts.DropCapabilities),
		UserNamespace: opts.UserNamespace, NoNewPrivileges: opts.NoNewPrivileges,
		SeccompProfile: opts.SeccompProfile, SecurityProfile: opts.SecurityProfile, ResourceProfile: opts.ResourceProfile, NoContainerStateMade: true,
	}
	return plan
}

func runStart(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: boxr start ID|NAME")
		return 2
	}
	record, err := state.Get(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "boxr start: %v\n", err)
		return 1
	}
	if record.Status == "running" || record.Status == "restarting" {
		fmt.Fprintf(stdout, "container %s is already running\n", record.ID)
		return 0
	}
	if record.Status != "created" && record.Status != "exited" && record.Status != "failed" {
		fmt.Fprintf(stderr, "boxr start: container %s must be created or stopped (current state: %s)\n", record.ID, record.Status)
		return 1
	}
	image := record.Labels["io.boxr.image"]
	if image == "" {
		fmt.Fprintf(stderr, "boxr start: container %s has no saved image reference\n", record.ID)
		return 1
	}
	if _, err := state.ReadEngineConfig(record.ID); err != nil {
		fmt.Fprintf(stderr, "boxr start: read private start configuration: %v\n", err)
		return 1
	}
	configPath, err := state.EngineConfigPath(record.ID)
	if err != nil {
		fmt.Fprintf(stderr, "boxr start: resolve private start configuration: %v\n", err)
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "boxr start: locate executable: %v\n", err)
		return 1
	}
	startArgs := []string{
		"run", "--config", configPath, "--image", image,
		"--detach", "--internal-existing-id", record.ID,
	}
	if err := launchCreatedContainer(executable, startArgs, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "boxr start: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "started %s\n", record.ID)
	return 0
}

func runPS(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ps", flag.ContinueOnError)
	flags.SetOutput(stderr)
	all := flags.Bool("all", false, "include completed containers")
	flags.BoolVar(all, "a", false, "include completed containers")
	last := flags.Int("last", -1, "show the newest N containers, including completed records")
	quiet := flags.Bool("quiet", false, "print container IDs only")
	flags.BoolVar(quiet, "q", false, "print container IDs only")
	noTrunc := flags.Bool("no-trunc", false, "do not truncate command output in the table")
	format := flags.String("format", "table", "output format: table or json")
	var filters envList
	flags.Var(&filters, "filter", "filter by id=PREFIX, name=VALUE, status=VALUE, or label=KEY=VALUE")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *last < -1 {
		fmt.Fprintln(stderr, "boxr ps: invalid arguments")
		return 2
	}
	if *last >= 0 {
		*all = true
	}
	if *quiet && *format == "json" {
		fmt.Fprintln(stderr, "boxr ps: --quiet cannot be combined with --format json")
		return 2
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintln(stderr, "boxr ps: --format must be table or json")
		return 2
	}
	records, err := state.List(*all)
	if err != nil {
		fmt.Fprintf(stderr, "boxr ps: %v\n", err)
		return 1
	}
	for _, filter := range filters {
		key, value, ok := strings.Cut(filter, "=")
		if !ok || key == "" {
			fmt.Fprintf(stderr, "boxr ps: invalid filter %q\n", filter)
			return 2
		}
		filtered := records[:0]
		for _, record := range records {
			match := false
			switch key {
			case "id":
				match = strings.HasPrefix(record.ID, value)
			case "name":
				match = record.Name == value
			case "status":
				match = record.Status == value
			case "label":
				labelKey, labelValue, hasValue := strings.Cut(value, "=")
				if hasValue {
					match = record.Labels[labelKey] == labelValue
				} else {
					_, match = record.Labels[value]
				}
			default:
				fmt.Fprintf(stderr, "boxr ps: unsupported filter key %q\n", key)
				return 2
			}
			if match {
				filtered = append(filtered, record)
			}
		}
		records = filtered
	}
	if *last >= 0 {
		if *last == 0 {
			records = []state.Record{}
		} else if len(records) > *last {
			records = records[:*last]
		}
	}
	if *quiet {
		for _, record := range records {
			fmt.Fprintln(stdout, record.ID)
		}
		return 0
	}
	if *format == "json" {
		return writeJSON(stdout, records, stderr, "boxr ps")
	}
	fmt.Fprintln(stdout, "ID           NAME                 STATUS    PID      COMMAND")
	for _, record := range records {
		name := record.Name
		if name == "" {
			name = "-"
		}
		pid := "-"
		if record.PID > 0 {
			pid = strconv.Itoa(record.PID)
		}
		command := strings.Join(record.Command, " ")
		if !*noTrunc && len(command) > 48 {
			command = command[:45] + "..."
		}
		fmt.Fprintf(stdout, "%-12s %-20s %-9s %-8s %s\n", record.ID, name, record.Status, pid, command)
	}
	return 0
}

type inspectSize struct {
	StateBytes int64 `json:"stateBytes"`
	LogBytes   int64 `json:"logBytes"`
	TotalBytes int64 `json:"totalBytes"`
}

type inspectSizeReport struct {
	Record state.Record `json:"record"`
	Size   inspectSize  `json:"size"`
}

func runInspect(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	withSize := flags.Bool("size", false, "include container state and log file sizes")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: boxr inspect [--size] ID|NAME")
		return 2
	}
	record, err := state.Get(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "boxr inspect: %v\n", err)
		return 1
	}
	if !*withSize {
		return writeJSON(stdout, record, stderr, "boxr inspect")
	}
	stateDir, err := state.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "boxr inspect: state directory: %v\n", err)
		return 1
	}
	stateBytes, err := inspectRegularFileSize(filepath.Join(stateDir, record.ID+".json"), true)
	if err != nil {
		fmt.Fprintf(stderr, "boxr inspect: state file: %v\n", err)
		return 1
	}
	logPath, err := state.LogPath(record.ID)
	if err != nil {
		fmt.Fprintf(stderr, "boxr inspect: log path: %v\n", err)
		return 1
	}
	logBytes, err := inspectRegularFileSize(logPath, false)
	if err != nil {
		fmt.Fprintf(stderr, "boxr inspect: log file: %v\n", err)
		return 1
	}
	size := inspectSize{StateBytes: stateBytes, LogBytes: logBytes, TotalBytes: stateBytes + logBytes}
	return writeJSON(stdout, inspectSizeReport{Record: record, Size: size}, stderr, "boxr inspect")
}

// inspectRegularFileSize uses Lstat so inspect never follows a log or state
// symlink while calculating on-disk metadata.
func inspectRegularFileSize(path string, required bool) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("size target must be a regular file")
	}
	return info.Size(), nil
}

func runRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	all := flags.Bool("all", false, "remove all completed container records")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *all {
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "boxr rm: --all does not accept IDs")
			return 2
		}
		records, err := state.List(true)
		if err != nil {
			fmt.Fprintf(stderr, "boxr rm: %v\n", err)
			return 1
		}
		removed := 0
		for _, record := range records {
			if record.Status == "running" || record.Status == "created" {
				continue
			}
			if err := state.Remove(record.ID); err != nil {
				fmt.Fprintf(stderr, "boxr rm: %s: %v\n", record.ID, err)
				return 1
			}
			removed++
		}
		fmt.Fprintf(stdout, "removed %d completed container record(s)\n", removed)
		return 0
	}
	if flags.NArg() == 0 {
		fmt.Fprintln(stderr, "Usage: boxr rm ID|NAME [ID|NAME...]")
		return 2
	}
	for _, query := range flags.Args() {
		if err := state.Remove(query); err != nil {
			fmt.Fprintf(stderr, "boxr rm: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s\n", query)
	}
	return 0
}

type statsWatchSample struct {
	SampledAt time.Time   `json:"sampledAt"`
	Stats     state.Stats `json:"stats"`
}

func runStats(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("stats", flag.ContinueOnError)
	flags.SetOutput(stderr)
	watch := flags.Bool("watch", false, "stream timestamped statistics until the container exits")
	interval := flags.Duration("interval", time.Second, "sampling interval for --watch (minimum 100ms)")
	count := flags.Int("count", 0, "stop after N samples (0 means until the container exits)")
	format := flags.String("format", "json", "output format: json or table")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *count < 0 || (!*watch && *count != 0) || (*watch && *interval < 100*time.Millisecond) || (*format != "json" && *format != "table") {
		fmt.Fprintln(stderr, "Usage: boxr stats [--watch] [--interval 1s] [--count N] [--format json|table] ID|NAME")
		return 2
	}

	query := flags.Arg(0)
	record, err := state.Get(query)
	if err != nil {
		fmt.Fprintf(stderr, "boxr stats: %v\n", err)
		return 1
	}
	if !*watch {
		stats, err := state.ReadStats(record.PID)
		if err != nil {
			fmt.Fprintf(stderr, "boxr stats: %v\n", err)
			return 1
		}
		if *format == "json" {
			return writeJSON(stdout, stats, stderr, "boxr stats")
		}
		if err := writeStatsTable(stdout, time.Now().UTC(), stats); err != nil {
			fmt.Fprintf(stderr, "boxr stats: write table: %v\n", err)
			return 1
		}
		return 0
	}

	current := record
	isRunning := func() (bool, error) {
		next, err := state.Get(query)
		if err != nil {
			if strings.Contains(err.Error(), "no container matches") {
				return false, nil
			}
			return false, err
		}
		current = next
		return current.Status == "running", nil
	}
	read := func() (state.Stats, error) {
		return state.ReadStats(current.PID)
	}
	return streamStats(stdout, stderr, *interval, *count, *format, isRunning, read)
}

const statsTableHeader = "SAMPLED_AT                   PID      MEMORY_CURRENT_B MEMORY_MAX       PIDS_CURRENT CPU_USAGE_USEC CPU_USER_USEC CPU_SYSTEM_USEC"

func writeStatsTable(stdout io.Writer, sampledAt time.Time, stats state.Stats) error {
	if _, err := fmt.Fprintln(stdout, statsTableHeader); err != nil {
		return err
	}
	return writeStatsTableRow(stdout, sampledAt, stats)
}

func writeStatsTableRow(stdout io.Writer, sampledAt time.Time, stats state.Stats) error {
	memoryMax := stats.MemoryMax
	if memoryMax == "" {
		memoryMax = "-"
	}
	_, err := fmt.Fprintf(stdout, "%-27s %-8d %-17d %-16s %-12d %-14d %-13d %-15d\n",
		sampledAt.Format(time.RFC3339), stats.PID, stats.MemoryCurrent, memoryMax,
		stats.PIDsCurrent, stats.CPUUsageUsec, stats.CPUUserUsec, stats.CPUSystemUsec)
	return err
}

// streamStats emits JSON Lines by default or a compact table when requested.
func streamStats(stdout, stderr io.Writer, interval time.Duration, count int, format string, isRunning func() (bool, error), read func() (state.Stats, error)) int {
	if interval <= 0 || count < 0 || (format != "json" && format != "table") || isRunning == nil || read == nil {
		fmt.Fprintln(stderr, "boxr stats: invalid watch configuration")
		return 2
	}
	encoder := json.NewEncoder(stdout)
	headerWritten := false
	emitted := 0
	for {
		running, err := isRunning()
		if err != nil {
			fmt.Fprintf(stderr, "boxr stats: check container status: %v\n", err)
			return 1
		}
		if !running {
			if emitted == 0 {
				fmt.Fprintln(stderr, "boxr stats: container is not running")
				return 1
			}
			return 0
		}

		stats, err := read()
		if err != nil {
			running, statusErr := isRunning()
			if statusErr != nil {
				fmt.Fprintf(stderr, "boxr stats: check container status: %v\n", statusErr)
				return 1
			}
			if !running && emitted > 0 {
				return 0
			}
			fmt.Fprintf(stderr, "boxr stats: %v\n", err)
			return 1
		}
		if format == "json" {
			err = encoder.Encode(statsWatchSample{SampledAt: time.Now().UTC(), Stats: stats})
		} else {
			if !headerWritten {
				_, err = fmt.Fprintln(stdout, statsTableHeader)
				headerWritten = err == nil
			}
			if err == nil {
				err = writeStatsTableRow(stdout, time.Now().UTC(), stats)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "boxr stats: write sample: %v\n", err)
			return 1
		}
		emitted++
		if count > 0 && emitted >= count {
			return 0
		}
		time.Sleep(interval)
	}
}

func runEvents(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	limit := flags.Int("limit", 50, "maximum number of recent events (0 means all)")
	format := flags.String("format", "table", "output format: table or json")
	sinceValue := flags.String("since", "", "include events since a duration (1h) or RFC3339 timestamp")
	action := flags.String("action", "", "filter by create, start, die, remove, or prune")
	containerQuery := flags.String("container", "", "filter by exact name or container ID prefix")
	follow := flags.Bool("follow", false, "stream new events as they are recorded")
	interval := flags.Duration("interval", 500*time.Millisecond, "poll interval for --follow (minimum 100ms)")
	count := flags.Int("count", 0, "stop after N matching events (0 means continue until interrupted)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *limit < 0 || *count < 0 || (!*follow && *count != 0) || (*follow && *interval < 100*time.Millisecond) {
		fmt.Fprintln(stderr, "Usage: boxr events [--follow] [--interval 500ms] [--count N] [--limit N] [--since 1h|RFC3339] [--action ACTION] [--container ID|NAME] [--format table|json]")
		return 2
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintln(stderr, "boxr events: --format must be table or json")
		return 2
	}
	if *action != "" && !map[string]bool{"create": true, "start": true, "die": true, "remove": true, "prune": true}[*action] {
		fmt.Fprintf(stderr, "boxr events: unsupported action %q\n", *action)
		return 2
	}
	var since time.Time
	if *sinceValue != "" {
		parsed, err := parseEventsSince(*sinceValue)
		if err != nil {
			fmt.Fprintf(stderr, "boxr events: invalid --since: %v\n", err)
			return 2
		}
		since = parsed
	}
	events, err := state.Events(0)
	if err != nil {
		fmt.Fprintf(stderr, "boxr events: %v\n", err)
		return 1
	}
	matches := func(event state.Event) bool {
		if !since.IsZero() && event.Time.Before(since) {
			return false
		}
		if *action != "" && event.Action != *action {
			return false
		}
		if *containerQuery != "" && event.Name != *containerQuery && !strings.HasPrefix(event.ID, *containerQuery) {
			return false
		}
		return true
	}
	if *follow {
		initial := make([]state.Event, 0)
		if !since.IsZero() {
			for _, event := range events {
				if matches(event) {
					initial = append(initial, event)
				}
			}
			if *limit > 0 && len(initial) > *limit {
				initial = initial[len(initial)-*limit:]
			}
		}
		return streamEvents(stdout, stderr, *interval, *count, *format, initial, len(events), matches, func() ([]state.Event, error) {
			return state.Events(0)
		})
	}

	filtered := make([]state.Event, 0, len(events))
	for _, event := range events {
		if matches(event) {
			filtered = append(filtered, event)
		}
	}
	if *limit > 0 && len(filtered) > *limit {
		filtered = filtered[len(filtered)-*limit:]
	}
	if *format == "json" {
		return writeJSON(stdout, filtered, stderr, "boxr events")
	}
	fmt.Fprintln(stdout, "TIME                     ACTION  ID           NAME                 PID")
	for _, event := range filtered {
		fmt.Fprintf(stdout, "%-24s %-7s %-12s %-20s %d\n", event.Time.Format(time.RFC3339), event.Action, event.ID, event.Name, event.PID)
	}
	return 0
}

// streamEvents polls an event source and writes each matching event once. In
// follow mode JSON is newline-delimited so consumers can process events live.
func streamEvents(stdout, stderr io.Writer, interval time.Duration, count int, format string, initial []state.Event, cursor int, matches func(state.Event) bool, fetch func() ([]state.Event, error)) int {
	if interval <= 0 || count < 0 || (format != "table" && format != "json") || matches == nil || fetch == nil {
		fmt.Fprintln(stderr, "boxr events: invalid follow configuration")
		return 2
	}
	encoder := json.NewEncoder(stdout)
	headerWritten := false
	emitted := 0
	writeEvent := func(event state.Event) error {
		if format == "json" {
			return encoder.Encode(event)
		}
		if !headerWritten {
			if _, err := fmt.Fprintln(stdout, "TIME                     ACTION  ID           NAME                 PID"); err != nil {
				return err
			}
			headerWritten = true
		}
		_, err := fmt.Fprintf(stdout, "%-24s %-7s %-12s %-20s %d\n", event.Time.Format(time.RFC3339), event.Action, event.ID, event.Name, event.PID)
		return err
	}
	for _, event := range initial {
		if err := writeEvent(event); err != nil {
			fmt.Fprintf(stderr, "boxr events: write event: %v\n", err)
			return 1
		}
		emitted++
		if count > 0 && emitted >= count {
			return 0
		}
	}
	if cursor < 0 {
		cursor = 0
	}
	for {
		time.Sleep(interval)
		events, err := fetch()
		if err != nil {
			fmt.Fprintf(stderr, "boxr events: follow: %v\n", err)
			return 1
		}
		// A shorter log indicates truncation/rotation; resume from its start.
		if len(events) < cursor {
			cursor = 0
		}
		for cursor < len(events) {
			event := events[cursor]
			cursor++
			if !matches(event) {
				continue
			}
			if err := writeEvent(event); err != nil {
				fmt.Fprintf(stderr, "boxr events: write event: %v\n", err)
				return 1
			}
			emitted++
			if count > 0 && emitted >= count {
				return 0
			}
		}
	}
}

func parseEventsSince(value string) (time.Time, error) {
	if timestamp, err := time.Parse(time.RFC3339, value); err == nil {
		return timestamp, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return time.Time{}, fmt.Errorf("expected a non-negative duration or RFC3339 timestamp")
	}
	return time.Now().Add(-duration), nil
}

func runInfo(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("info", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write host information as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "boxr info: unexpected arguments")
		return 2
	}
	stateDir, err := state.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "boxr info: %v\n", err)
		return 1
	}
	kernel := "unavailable"
	if data, readErr := os.ReadFile("/proc/sys/kernel/osrelease"); readErr == nil {
		kernel = strings.TrimSpace(string(data))
	}
	_, cgroupErr := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	info := map[string]any{"version": version, "commit": commit, "buildDate": buildDate, "os": runtime.GOOS, "architecture": runtime.GOARCH, "goVersion": runtime.Version(), "kernel": kernel, "cgroupV2": cgroupErr == nil, "effectiveUID": os.Geteuid(), "stateDirectory": stateDir, "runtimeSupported": runtime.GOOS == "linux"}
	if *jsonOutput {
		return writeJSON(stdout, info, stderr, "boxr info")
	}
	for _, key := range []string{"version", "commit", "buildDate", "os", "architecture", "goVersion", "kernel", "cgroupV2", "effectiveUID", "stateDirectory", "runtimeSupported"} {
		fmt.Fprintf(stdout, "%-18s %v\n", key+":", info[key])
	}
	return 0
}

type directoryUsage struct {
	Path         string `json:"path"`
	Files        int64  `json:"files"`
	Directories  int64  `json:"directories"`
	Symlinks     int64  `json:"symlinks"`
	OtherEntries int64  `json:"otherEntries"`
	Bytes        int64  `json:"bytes"`
}

type systemDiskUsage struct {
	Images       imageStore.StoreUsage `json:"images"`
	Volumes      []volume.Usage        `json:"volumes"`
	ContainerDir directoryUsage        `json:"containerState"`
	VolumeBytes  int64                 `json:"volumeBytes"`
	TotalBytes   int64                 `json:"totalBytes"`
}

func runSystem(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "df" {
		fmt.Fprintln(stderr, "Usage: boxr system df [--json]")
		return 2
	}
	flags := flag.NewFlagSet("system df", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write storage usage as JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: boxr system df [--json]")
		return 2
	}
	report := systemDiskUsage{Volumes: []volume.Usage{}}
	images, err := imageStore.Usage()
	if err != nil {
		fmt.Fprintf(stderr, "boxr system df: image store: %v\n", err)
		return 1
	}
	report.Images = images
	volumes, err := volume.List()
	if err != nil {
		fmt.Fprintf(stderr, "boxr system df: volumes: %v\n", err)
		return 1
	}
	for _, item := range volumes {
		usage, err := volume.Measure(item.Name)
		if err != nil {
			fmt.Fprintf(stderr, "boxr system df: volume %s: %v\n", item.Name, err)
			return 1
		}
		report.Volumes = append(report.Volumes, usage)
		report.VolumeBytes += usage.TotalBytes
	}
	stateDir, err := state.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "boxr system df: state directory: %v\n", err)
		return 1
	}
	report.ContainerDir, err = measureDirectory(stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "boxr system df: container state: %v\n", err)
		return 1
	}
	report.TotalBytes = report.Images.TotalBytes + report.VolumeBytes + report.ContainerDir.Bytes
	if *jsonOutput {
		return writeJSON(stdout, report, stderr, "boxr system df")
	}
	fmt.Fprintln(stdout, "TYPE             COUNT      SIZE")
	fmt.Fprintf(stdout, "Images           %-10d %d bytes\n", report.Images.Images, report.Images.TotalBytes)
	fmt.Fprintf(stdout, "Volumes          %-10d %d bytes\n", len(report.Volumes), report.VolumeBytes)
	fmt.Fprintf(stdout, "Container state  %-10d %d bytes\n", report.ContainerDir.Files, report.ContainerDir.Bytes)
	fmt.Fprintf(stdout, "Total                       %d bytes\n", report.TotalBytes)
	return 0
}

// measureDirectory snapshots regular-file bytes without following symlinks.
// Missing directories are treated as empty; symlinked roots are rejected.
func measureDirectory(root string) (directoryUsage, error) {
	usage := directoryUsage{Path: root}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return usage, nil
	}
	if err != nil {
		return usage, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return usage, fmt.Errorf("storage path must be a real directory")
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			usage.Directories++
			return nil
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entryInfo.Mode()&os.ModeSymlink != 0:
			usage.Symlinks++
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			usage.Bytes += int64(len(target))
		case entryInfo.Mode().IsRegular():
			usage.Files++
			usage.Bytes += entryInfo.Size()
		default:
			usage.OtherEntries++
		}
		return nil
	})
	return usage, err
}

type pruneCandidate struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type pruneReport struct {
	DryRun     bool             `json:"dryRun"`
	OlderThan  time.Duration    `json:"olderThan"`
	Before     time.Time        `json:"before"`
	Count      int              `json:"count"`
	Candidates []pruneCandidate `json:"candidates"`
	Removed    int              `json:"removed,omitempty"`
}

func runPrune(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	older := flags.Duration("older-than", 7*24*time.Hour, "remove completed records older than this duration")
	dryRun := flags.Bool("dry-run", false, "show which records would be removed without deleting them")
	jsonOutput := flags.Bool("json", false, "write prune results as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *older < 0 {
		fmt.Fprintln(stderr, "Usage: boxr prune [--older-than 168h] [--dry-run] [--json]")
		return 2
	}
	before := time.Now().Add(-*older)
	if *dryRun {
		records, err := state.List(true)
		if err != nil {
			fmt.Fprintf(stderr, "boxr prune: %v\n", err)
			return 1
		}
		candidates := make([]pruneCandidate, 0)
		for _, record := range records {
			if record.Status == "running" || record.Status == "created" || record.CreatedAt.After(before) {
				continue
			}
			candidates = append(candidates, pruneCandidate{ID: record.ID, Name: record.Name, Status: record.Status, CreatedAt: record.CreatedAt})
		}
		report := pruneReport{DryRun: true, OlderThan: *older, Before: before, Count: len(candidates), Candidates: candidates}
		if *jsonOutput {
			return writeJSON(stdout, report, stderr, "boxr prune")
		}
		fmt.Fprintf(stdout, "would prune %d completed container record(s) older than %s\n", len(candidates), older.String())
		for _, candidate := range candidates {
			fmt.Fprintf(stdout, "%-12s %-20s %s\n", candidate.ID, candidate.Name, candidate.Status)
		}
		return 0
	}
	count, err := state.Prune(before)
	if err != nil {
		fmt.Fprintf(stderr, "boxr prune: %v\n", err)
		return 1
	}
	if *jsonOutput {
		return writeJSON(stdout, pruneReport{DryRun: false, OlderThan: *older, Before: before, Count: count, Removed: count, Candidates: []pruneCandidate{}}, stderr, "boxr prune")
	}
	fmt.Fprintf(stdout, "pruned %d completed container record(s)\n", count)
	return 0
}

func writeJSON(stdout io.Writer, value any, stderr io.Writer, command string) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 1
	}
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write diagnostics as JSON")
	strict := flags.Bool("strict", false, "return a failure status when any warning is reported")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "boxr doctor: unexpected arguments")
		return 2
	}
	checks := container.Doctor()
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(checks); err != nil {
			fmt.Fprintf(stderr, "boxr doctor: %v\n", err)
			return 1
		}
		for _, check := range checks {
			if check.Status == container.DiagnosticError || (*strict && check.Status == container.DiagnosticWarn) {
				return 1
			}
		}
		return 0
	}
	failed := false
	fmt.Fprintln(stdout, "boxr doctor — read-only host checks")
	for _, check := range checks {
		marker := "[ OK ]"
		switch check.Status {
		case container.DiagnosticWarn:
			marker = "[WARN]"
			if *strict {
				failed = true
			}
		case container.DiagnosticError:
			marker = "[FAIL]"
			failed = true
		}
		fmt.Fprintf(stdout, "%-7s %-22s %s\n", marker, check.Name, check.Detail)
	}
	if failed {
		fmt.Fprintln(stderr, "boxr doctor: one or more required platform checks failed")
		return 1
	}
	fmt.Fprintln(stdout, "\nHost checks completed. Warnings describe limitations or optional capabilities.")
	return 0
}

func runSupportBundle(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "write the support archive to PATH")
	force := flags.Bool("force", false, "replace an existing regular output file")
	jsonOutput := flags.Bool("json", false, "write archive metadata as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*output) == "" {
		fmt.Fprintln(stderr, "Usage: boxr support-bundle --output PATH [--force] [--json]")
		return 2
	}
	diagnostics := container.Doctor()
	safeDiagnostics := make([]support.Diagnostic, 0, len(diagnostics))
	for _, check := range diagnostics {
		safeDiagnostics = append(safeDiagnostics, support.Diagnostic{
			Name:   check.Name,
			Status: string(check.Status),
		})
	}
	kernel := ""
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(string(data))
	}
	_, cgroupErr := os.Stat("/sys/fs/cgroup/cgroup.controllers")

	result, err := support.Create(*output, *force, support.Metadata{
		Version:     version,
		Commit:      commit,
		BuildDate:   buildDate,
		Kernel:      kernel,
		CgroupV2:    cgroupErr == nil,
		Diagnostics: safeDiagnostics,
	})
	if err != nil {
		fmt.Fprintf(stderr, "boxr support-bundle: %v\n", err)
		return 1
	}
	if *jsonOutput {
		return writeJSON(stdout, result, stderr, "boxr support-bundle")
	}
	fmt.Fprintf(stdout, "created support bundle %s (%d bytes)\nsha256: %s\n", result.Path, result.Bytes, result.SHA256)
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `boxr — lightweight Linux process isolation

Usage:
  boxr daemon [--host unix:///absolute/path/engine.sock]
  boxr pull [--platform linux/ARCH[/VARIANT]] [--authfile PATH] IMAGE
  boxr push [--authfile PATH] LOCAL:TAG [REGISTRY/IMAGE:TAG]
  boxr create [options] IMAGE [COMMAND [ARG...]]
  boxr start ID|NAME
  boxr run [options] [--detach|-d] [--dry-run] COMMAND [ARG...]
  boxr exec [options] CONTAINER COMMAND [ARG...]
  boxr version [--json]
  boxr info [--json]
  boxr system df [--json]
  boxr doctor [--json] [--strict]
  boxr support-bundle --output PATH [--force] [--json]
  boxr ps [--all] [--last N] [--quiet] [--no-trunc] [--filter KEY=VALUE]
  boxr inspect [--size] ID|NAME
  boxr stop [--time 10s] ID|NAME [...]
  boxr kill [--signal KILL] ID|NAME [...]
  boxr wait [--condition running|stopped] [--timeout 30s] [--interval 100ms] [--json] ID|NAME [...]
  boxr logs [--tail 100] [--follow] ID|NAME
  boxr exec [--workdir PATH] [--env KEY=VALUE] [--env-file PATH] ID|NAME COMMAND [ARG...]
  boxr network create --subnet CIDR [--gateway IP] [--no-nat] NAME
  boxr network ls [--json]
  boxr network inspect [--json] NAME
  boxr network connect [--publish SPEC] NETWORK CONTAINER
  boxr network disconnect NETWORK CONTAINER
  boxr network prune [--dry-run|--force]
  boxr network rm NAME
  boxr volume create [--label KEY=VALUE] NAME
  boxr volume ls [--format table|json] [--filter label=KEY=VALUE]
  boxr volume inspect NAME
  boxr volume du [--json] NAME
  boxr volume du --all [--json] [--filter name=VALUE|label=KEY[=VALUE]]
  boxr volume clone SOURCE TARGET
  boxr volume export NAME ARCHIVE.tar.gz
  boxr volume import ARCHIVE.tar.gz NAME
  boxr volume rm NAME [...]
  boxr volume prune (--force|--dry-run) [--json] [--filter name=VALUE|label=KEY[=VALUE]]
  boxr stats [--watch] [--interval 1s] [--count N] [--format json|table] ID|NAME
  boxr rm ID|NAME [...]
  boxr events [--follow] [--interval 500ms] [--count N] [--limit N] [--since 1h|RFC3339] [--action ACTION] [--container ID|NAME] [--format table|json]
  boxr prune [--older-than 168h] [--dry-run] [--json]
  boxr rootfs inspect PATH
  boxr rootfs verify PATH
  boxr rootfs audit [--json] [--strict] PATH
  boxr rootfs pack PATH ARCHIVE.tar.gz
  boxr rootfs unpack ARCHIVE.tar.gz PATH
  boxr rootfs digest PATH
  boxr rootfs diff BEFORE AFTER
  boxr image build --tag NAME:TAG [-f Dockerfile|Boxrfile.json] [CONTEXT]
  boxr image import ROOTFS NAME:TAG
  boxr image pull [--platform linux/ARCH[/VARIANT]] [--authfile PATH] REGISTRY/IMAGE[:TAG]
  boxr image push [--authfile PATH] LOCAL:TAG [REGISTRY/IMAGE:TAG]
  boxr image ls [--format table|json] [--quiet] [--filter KEY=VALUE]
  boxr image inspect NAME:TAG
  boxr image verify NAME:TAG
  boxr image audit [--json] [--strict] NAME:TAG
  boxr image du [--json]
  boxr image du --all [--json] [--filter KEY=VALUE]
  boxr image save [--format oci|docker] NAME:TAG [NAME2:TAG ...] ARCHIVE.tar[.gz]
  boxr image load ARCHIVE.tar.gz
  boxr image unpack NAME:TAG PATH
  boxr image rm NAME:TAG [...]
  boxr image prune [--dry-run|--force] [--json]
  boxr config validate FILE.json
  boxr config example
  boxr deploy validate [--profile NAME] FILE.json
  boxr deploy plan [--profile NAME] [--json] FILE.json
  boxr deploy pull [--profile NAME] [--platform linux/ARCH[/VARIANT]] [--authfile PATH] FILE.json
  boxr deploy up [--profile NAME] [--dry-run] FILE.json
  boxr deploy profiles [--json] FILE.json
  boxr deploy profiles [--json] FILE.json
  boxr deploy ps [--json] STACK
  boxr deploy logs [--tail 100] [--follow] STACK [SERVICE]
  boxr deploy wait [--condition running|stopped] [--timeout 30s] [--interval 100ms] [--json] STACK
  boxr deploy down [--time 10s] [--remove] STACK
  boxr deploy example
  boxr features [--json]
  boxr completion bash|zsh|fish|powershell
  boxr help

Run options:
  --hostname NAME   set the container hostname
  --memory LIMIT    set a cgroup v2 memory limit (for example 256m or 1g)
  --cpus NUMBER     set a cgroup v2 CPU quota in cores (for example 0.5 or 2)
  --workdir PATH    change the command's working directory
  --env KEY=VALUE   add or override an environment variable (repeatable)
  --env-file PATH   load KEY=VALUE environment entries from a file
  --config PATH     load a versioned JSON run configuration
  --image NAME:TAG  unpack and run a verified local Boxr image
  --rootfs PATH     use a root filesystem directory (Linux, privileged)
  --overlay-dir PATH keep writes in a persistent OverlayFS layer (requires --rootfs)
  --read-only-rootfs make the selected root filesystem read-only
  --mount-proc      mount a new proc filesystem inside rootfs
  --mount SRC:DST[:ro|rw] bind mount a host path (repeatable)
  --tmpfs DST[:SIZE[:MODE]] mount a bounded tmpfs (repeatable)
  --network host|none|bridge use host, isolated, or managed IPv4 bridge networking
  --network-name NAME  select a managed bridge when --network bridge
  --publish, -p SPEC  publish [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp|udp] (bridge only)
  --userns          map the invoking UID/GID to root in a private user namespace
  --cap-drop NAME   remove a Linux capability, or ALL (repeatable)
  --no-new-privileges block privilege gain through execve
  --seccomp-profile baseline deny selected high-risk syscalls (Linux)
  --security-profile default|baseline|restricted|untrusted apply mandatory security presets
  --memory-high N   set the cgroup v2 memory.high threshold
  --memory-low N    set the cgroup v2 memory.low protection
  --memory-min N    set the cgroup v2 memory.min protection
  --memory-swap N   set the cgroup v2 memory.swap.max limit
  --cpu-weight N    set CPU weight from 1 to 10000
  --io-weight N     set block I/O weight from 1 to 10000
  --pids-limit N    set the cgroup v2 task limit
  --oom-group       kill the cgroup as a unit on memory OOM

Examples:
  sudo boxr run --hostname web --memory 256m --cpus 1 -- /bin/sh
  sudo boxr run --image alpine:local --mount-proc --read-only-rootfs -- /bin/sh
  sudo boxr run --rootfs ./rootfs --mount-proc --read-only-rootfs -- /bin/sh
  sudo boxr run --userns --network none --cap-drop NET_RAW --no-new-privileges -- /bin/sh
  sudo boxr network create --subnet 172.30.10.0/24 app-net
  sudo boxr run --network bridge --network-name app-net --publish 127.0.0.1:8080:80/tcp -- /bin/sh
  sudo boxr run --tmpfs /tmp:64m:1777 --rootfs ./rootfs -- /bin/sh

Without --image or --rootfs, the host filesystem remains visible. The default network mode
is host. These controls improve isolation but do not make boxr a hardened sandbox.`)
}

func parseMemoryLimit(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, fmt.Errorf("value cannot be empty")
	}

	units := []struct {
		suffix string
		mult   uint64
	}{
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"b", 1},
	}

	var multiplier uint64 = 1
	number := value
	for _, unit := range units {
		if strings.HasSuffix(value, unit.suffix) {
			number = strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
			multiplier = unit.mult
			break
		}
	}
	amount, err := strconv.ParseUint(number, 10, 64)
	if err != nil || amount == 0 {
		return 0, fmt.Errorf("expected a positive integer with an optional B, K, M, or G suffix")
	}
	if amount > uint64(math.MaxInt64)/multiplier {
		return 0, fmt.Errorf("value is too large")
	}
	return int64(amount * multiplier), nil
}

type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }
func (e *envList) Set(value string) error {
	*e = append(*e, value)
	return nil
}

func validateEnv(value string) error {
	key, _, ok := strings.Cut(value, "=")
	if !ok || key == "" {
		return fmt.Errorf("expected KEY=VALUE")
	}
	for i, r := range key {
		valid := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')
		if !valid {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("NUL characters are not allowed")
	}
	return nil
}

func parseBindMount(value string) (container.BindMount, error) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return container.BindMount{}, fmt.Errorf("expected SOURCE:TARGET[:ro|rw]")
	}
	mount := container.BindMount{Source: parts[0], Target: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			mount.ReadOnly = true
		case "rw":
		default:
			return container.BindMount{}, fmt.Errorf("mount mode must be ro or rw")
		}
	}
	if !strings.HasPrefix(mount.Source, "/") || !strings.HasPrefix(mount.Target, "/") {
		return container.BindMount{}, fmt.Errorf("source and target must be absolute paths")
	}
	return mount, nil
}

func parseTmpfsMount(value string) (container.TmpfsMount, error) {
	parts := strings.Split(value, ":")
	if len(parts) < 1 || len(parts) > 3 || parts[0] == "" || !strings.HasPrefix(parts[0], "/") {
		return container.TmpfsMount{}, fmt.Errorf("expected TARGET[:SIZE[:MODE]] with an absolute target")
	}
	mount := container.TmpfsMount{Target: parts[0], Mode: 01777}
	if len(parts) >= 2 && parts[1] != "" {
		size, err := container.ParseTmpfsSize(parts[1])
		if err != nil {
			return container.TmpfsMount{}, err
		}
		mount.SizeBytes = size
	}
	if len(parts) == 3 && parts[2] != "" {
		mode, err := strconv.ParseUint(parts[2], 8, 32)
		if err != nil || mode > 07777 {
			return container.TmpfsMount{}, fmt.Errorf("mode must be an octal permission value no greater than 7777")
		}
		mount.Mode = uint32(mode)
	}
	return mount, nil
}

func runImage(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: boxr image build|pull|push|import|ls|inspect|verify|audit|diff|sbom|lock|verify-lock|du|save|load|unpack|rm|prune ...")
		return 2
	}
	switch args[0] {
	case "build":
		flags := flag.NewFlagSet("image build", flag.ContinueOnError)
		flags.SetOutput(stderr)
		file := flags.String("file", "", "Dockerfile or Boxrfile.json (default: CONTEXT/Dockerfile)")
		flags.StringVar(file, "f", "", "Dockerfile or JSON recipe path")
		platform := flags.String("platform", "", "target image platform (default: linux/<host architecture>)")
		buildNetwork := flags.String("network", "none", "network for Dockerfile RUN steps: none (default) or host")
		var buildArgValues []string
		flags.Func("build-arg", "set a Dockerfile ARG value as NAME=VALUE (repeatable)", func(value string) error {
			if len(buildArgValues) >= 64 || len(value) > 65536 {
				return fmt.Errorf("too many or oversized build arguments")
			}
			buildArgValues = append(buildArgValues, value)
			return nil
		})
		tag := flags.String("tag", "", "image reference to create (required)")
		jsonOutput := flags.Bool("json", false, "write image metadata as JSON")
		quiet := flags.Bool("quiet", false, "print only the resulting image reference")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if *tag == "" || flags.NArg() > 1 || (*quiet && *jsonOutput) || (*buildNetwork != "none" && *buildNetwork != "host") {
			fmt.Fprintln(stderr, "Usage: boxr image build --tag NAME:TAG [--platform linux/ARCH[/VARIANT]] [--network none|host] [--build-arg NAME=VALUE] [-f Dockerfile|Boxrfile.json] [--json|--quiet] [CONTEXT]")
			return 2
		}
		contextDir := "."
		if flags.NArg() == 1 {
			contextDir = flags.Arg(0)
		}
		buildFile := *file
		if buildFile == "" {
			dockerfilePath := filepath.Join(contextDir, "Dockerfile")
			if _, statErr := os.Lstat(dockerfilePath); statErr == nil {
				buildFile = dockerfilePath
			} else if !os.IsNotExist(statErr) {
				fmt.Fprintf(stderr, "boxr image build: inspect default Dockerfile: %v\n", statErr)
				return 1
			} else {
				buildFile = filepath.Join(contextDir, "Boxrfile.json")
			}
		}
		buildArgs := make(map[string]string, len(buildArgValues))
		for _, raw := range buildArgValues {
			key, value, ok := strings.Cut(raw, "=")
			if !ok || key == "" {
				fmt.Fprintf(stderr, "boxr image build: invalid --build-arg %q; expected NAME=VALUE\n", raw)
				return 2
			}
			buildArgs[key] = value
		}
		var created imageStore.Image
		var err error
		if strings.EqualFold(filepath.Ext(buildFile), ".json") {
			if *buildNetwork != "none" || len(buildArgs) > 0 {
				fmt.Fprintln(stderr, "boxr image build: --network and --build-arg apply only to Dockerfile builds")
				return 2
			}
			created, err = imageStore.BuildForPlatform(contextDir, buildFile, *tag, *platform)
		} else {
			created, err = imageStore.BuildDockerfileForPlatformWithOptions(contextDir, buildFile, *tag, *platform, *buildNetwork, buildArgs)
		}
		if err != nil {
			fmt.Fprintf(stderr, "boxr image build: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, created, stderr, "boxr image build")
		}
		if *quiet {
			fmt.Fprintln(stdout, created.Reference)
			return 0
		}
		fmt.Fprintf(stdout, "built %s (%s; %s/%s; %d bytes)\n", created.Reference, created.Digest, created.OS, created.Arch, created.LayerBytes)
		return 0
	case "push":
		flags := flag.NewFlagSet("image push", flag.ContinueOnError)
		flags.SetOutput(stderr)
		authFile := flags.String("authfile", "", "registry auth file (Docker config.json or Podman auth.json)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() < 1 || flags.NArg() > 2 {
			fmt.Fprintln(stderr, "Usage: boxr image push [--authfile PATH] LOCAL:TAG [REGISTRY/IMAGE:TAG]")
			return 2
		}
		target := flags.Arg(0)
		if flags.NArg() == 2 {
			target = flags.Arg(1)
		}
		bundle, err := imageStore.ExportRegistryBundle(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr image push: export local image: %v\n", err)
			return 1
		}
		defer bundle.Cleanup()
		credentials, err := registry.ResolveCredentials(target, *authFile, registry.Credentials{
			Username: os.Getenv("BOXR_REGISTRY_USERNAME"),
			Password: os.Getenv("BOXR_REGISTRY_PASSWORD"),
		})
		if err != nil {
			fmt.Fprintf(stderr, "boxr image push: registry credentials: %v\n", err)
			return 1
		}
		if err := registry.Push(context.Background(), target, registry.PushBundle{
			Manifest:  bundle.Manifest,
			Config:    bundle.Config,
			LayerPath: bundle.LayerPath,
		}, credentials); err != nil {
			fmt.Fprintf(stderr, "boxr image push: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "pushed %s\n", target)
		return 0
	case "pull":
		flags := flag.NewFlagSet("image pull", flag.ContinueOnError)
		flags.SetOutput(stderr)
		platform := flags.String("platform", "", "target platform (default: linux/<host architecture>)")
		authFile := flags.String("authfile", "", "registry auth file (Docker config.json or Podman auth.json)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr image pull [--platform linux/amd64[/VARIANT]] [--authfile PATH] REGISTRY/IMAGE[:TAG|@sha256:DIGEST]")
			return 2
		}
		credentials, err := registry.ResolveCredentials(flags.Arg(0), *authFile, registry.Credentials{
			Username: os.Getenv("BOXR_REGISTRY_USERNAME"),
			Password: os.Getenv("BOXR_REGISTRY_PASSWORD"),
		})
		if err != nil {
			fmt.Fprintf(stderr, "boxr image pull: registry credentials: %v\n", err)
			return 1
		}
		artifact, err := registry.Pull(context.Background(), flags.Arg(0), *platform, credentials)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image pull: %v\n", err)
			return 1
		}
		defer artifact.Cleanup()
		staging, err := os.MkdirTemp("", "boxr-image-pull-*")
		if err != nil {
			fmt.Fprintf(stderr, "boxr image pull: create staging directory: %v\n", err)
			return 1
		}
		defer os.RemoveAll(staging)
		rootPath := filepath.Join(staging, "rootfs")
		if err := os.Mkdir(rootPath, 0700); err != nil {
			fmt.Fprintf(stderr, "boxr image pull: create rootfs: %v\n", err)
			return 1
		}
		for index, layer := range artifact.Layers {
			if err := imageStore.ApplyLayerArchive(rootPath, layer.Path); err != nil {
				fmt.Fprintf(stderr, "boxr image pull: apply layer %d: %v\n", index+1, err)
				return 1
			}
		}
		image, err := imageStore.ImportRootFSWithConfig(rootPath, artifact.Reference, artifact.OS, artifact.Architecture, artifact.Variant, imageStore.ImageRuntimeConfig{
			Env: artifact.RuntimeConfig.Env, Entrypoint: artifact.RuntimeConfig.Entrypoint, Cmd: artifact.RuntimeConfig.Cmd,
			WorkingDir: artifact.RuntimeConfig.WorkingDir, User: artifact.RuntimeConfig.User, Labels: artifact.RuntimeConfig.Labels,
		})
		if err != nil {
			fmt.Fprintf(stderr, "boxr image pull: import into local image store: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "pulled %s (%s; %s/%s; %d bytes)\n", image.Reference, artifact.ManifestDigest, artifact.OS, artifact.Architecture, artifact.LayerBytes)
		return 0
	case "import":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr image import ROOTFS NAME:TAG")
			return 2
		}
		created, err := imageStore.ImportRootFS(args[1], args[2])
		if err != nil {
			fmt.Fprintf(stderr, "boxr image import: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "imported %s %s (%d bytes)\n", created.Reference, created.Digest, created.LayerBytes)
		return 0
	case "ls", "list":
		flags := flag.NewFlagSet("image ls", flag.ContinueOnError)
		flags.SetOutput(stderr)
		format := flags.String("format", "table", "output format: table or json")
		quiet := flags.Bool("quiet", false, "print image references only")
		var filters envList
		flags.Var(&filters, "filter", "filter by reference, os, arch, variant, digest prefix, or label")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (*format != "table" && *format != "json") || (*quiet && *format == "json") {
			fmt.Fprintln(stderr, "Usage: boxr image ls [--format table|json] [--quiet] [--filter reference=PREFIX|os=VALUE|arch=VALUE|variant=VALUE|digest=PREFIX|label=KEY[=VALUE]]")
			return 2
		}
		items, err := imageStore.List()
		if err != nil {
			fmt.Fprintf(stderr, "boxr image ls: %v\n", err)
			return 1
		}
		items, err = filterImageList(items, filters)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image ls: %v\n", err)
			return 2
		}
		if *quiet {
			for _, item := range items {
				fmt.Fprintln(stdout, item.Reference)
			}
			return 0
		}
		if *format == "json" {
			return writeJSON(stdout, items, stderr, "boxr image ls")
		}
		fmt.Fprintln(stdout, "REFERENCE             DIGEST        PLATFORM       LAYER BYTES")
		for _, item := range items {
			digest := strings.TrimPrefix(item.Digest, "sha256:")
			if len(digest) > 12 {
				digest = digest[:12]
			}
			fmt.Fprintf(stdout, "%-21s %-13s %-14s %d\n", item.Reference, digest, item.OS+"/"+item.Arch, item.LayerBytes)
		}
		return 0
	case "tag":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr image tag SOURCE:TAG TARGET:TAG")
			return 2
		}
		if err := imageStore.Tag(args[1], args[2]); err != nil {
			fmt.Fprintf(stderr, "boxr image tag: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "tagged %s as %s\n", args[1], args[2])
		return 0
	case "audit":
		flags := flag.NewFlagSet("image audit", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "emit a machine-readable audit report")
		strict := flags.Bool("strict", false, "return non-zero when any warning or high finding exists")
		maxEntries := flags.Int64("max-entries", rootfs.DefaultAuditMaxEntries, "maximum filesystem entries to inspect")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr image audit [--json] [--strict] [--max-entries N] NAME:TAG")
			return 2
		}
		report, err := imageStore.Audit(flags.Arg(0), *maxEntries)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image audit: %v\n", err)
			return 1
		}
		if *jsonOutput {
			if code := writeJSON(stdout, report, stderr, "boxr image audit"); code != 0 {
				return code
			}
		} else {
			fmt.Fprintf(stdout, "audited image %s (%s/%s): integrity verified; %d rootfs entries, %d high, %d warning, %d info\n", report.Image.Reference, report.Image.OS, report.Image.Arch, report.RootFS.EntriesScanned, report.RootFS.HighCount, report.RootFS.WarningCount, report.RootFS.InfoCount)
			for _, finding := range report.RootFS.Findings {
				fmt.Fprintf(stdout, "%-7s %-28s %s: %s\n", strings.ToUpper(finding.Severity), finding.Code, finding.Path, finding.Message)
			}
		}
		if *strict && report.HasFindings() {
			return 1
		}
		return 0
	case "diff":
		flags := flag.NewFlagSet("image diff", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "emit machine-readable filesystem changes")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 2 {
			fmt.Fprintln(stderr, "Usage: boxr image diff [--json] BEFORE:TAG AFTER:TAG")
			return 2
		}
		changes, err := imageStore.Diff(flags.Arg(0), flags.Arg(1))
		if err != nil {
			fmt.Fprintf(stderr, "boxr image diff: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, changes, stderr, "boxr image diff")
		}
		for _, change := range changes {
			fmt.Fprintf(stdout, "%-8s %s\n", change.Status, change.Path)
		}
		fmt.Fprintf(stdout, "%d change(s)\n", len(changes))
		return 0
	case "inspect", "verify":
		if len(args) != 2 {
			fmt.Fprintf(stderr, "Usage: boxr image %s NAME:TAG\n", args[0])
			return 2
		}
		item, err := imageStore.Verify(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "boxr image %s: %v\n", args[0], err)
			return 1
		}
		if args[0] == "verify" {
			fmt.Fprintf(stdout, "verified %s %s (%d layer bytes; %s/%s)\n", item.Reference, item.Digest, item.LayerBytes, item.OS, item.Arch)
			return 0
		}
		return writeJSON(stdout, item, stderr, "boxr image inspect")
	case "du":
		flags := flag.NewFlagSet("image du", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write usage as JSON")
		all := flags.Bool("all", false, "list each tagged image's rootfs layer size")
		var filters envList
		flags.Var(&filters, "filter", "filter all-image usage by reference, os, arch, variant, digest, or label")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (!*all && len(filters) != 0) {
			fmt.Fprintln(stderr, "Usage: boxr image du [--json] | boxr image du --all [--json] [--filter KEY=VALUE]")
			return 2
		}
		storeUsage, err := imageStore.Usage()
		if err != nil {
			fmt.Fprintf(stderr, "boxr image du: %v\n", err)
			return 1
		}
		if !*all {
			if *jsonOutput {
				return writeJSON(stdout, storeUsage, stderr, "boxr image du")
			}
			fmt.Fprintf(stdout, "Images: %d\nUnique blobs: %d (%d bytes)\nReachable: %d blobs (%d bytes)\nReclaimable: %d blobs (%d bytes)\n", storeUsage.Images, storeUsage.UniqueBlobs, storeUsage.TotalBytes, storeUsage.ReachableBlobs, storeUsage.ReachableBytes, storeUsage.ReclaimableBlobs, storeUsage.ReclaimableBytes)
			return 0
		}
		images, err := imageStore.List()
		if err != nil {
			fmt.Fprintf(stderr, "boxr image du: %v\n", err)
			return 1
		}
		images, err = filterImageList(images, filters)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image du: %v\n", err)
			return 2
		}
		report := imageUsageSummary{
			ImageCount: len(images),
			Images:     make([]imageUsageEntry, 0, len(images)),
			Store:      storeUsage,
		}
		for _, item := range images {
			entry := imageUsageEntry{
				Reference:    item.Reference,
				Digest:       item.Digest,
				OS:           item.OS,
				Architecture: item.Arch,
				Variant:      item.Variant,
				LayerBytes:   item.LayerBytes,
			}
			report.Images = append(report.Images, entry)
			report.ReferencedLayerBytes += item.LayerBytes
		}
		if *jsonOutput {
			return writeJSON(stdout, report, stderr, "boxr image du")
		}
		fmt.Fprintln(stdout, "REFERENCE                         PLATFORM          ROOTFS_LAYER_BYTES")
		for _, item := range report.Images {
			platform := item.OS + "/" + item.Architecture
			if item.Variant != "" {
				platform += "/" + item.Variant
			}
			fmt.Fprintf(stdout, "%-34s %-17s %d\n", item.Reference, platform, item.LayerBytes)
		}
		fmt.Fprintf(stdout, "Tagged images: %d\nReferenced rootfs layers: %d bytes (shared layers may be counted more than once)\nUnique store blobs: %d bytes\n",
			report.ImageCount, report.ReferencedLayerBytes, report.Store.TotalBytes)
		return 0
	case "sbom":
		flags := flag.NewFlagSet("image sbom", flag.ContinueOnError)
		flags.SetOutput(stderr)
		maxEntries := flags.Int64("max-entries", imageStore.DefaultSBOMMaxEntries, "maximum filesystem entries to inventory")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 || *maxEntries <= 0 {
			fmt.Fprintln(stderr, "Usage: boxr image sbom [--max-entries N] NAME:TAG")
			return 2
		}
		report, err := imageStore.GenerateSBOM(flags.Arg(0), *maxEntries)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image sbom: %v\n", err)
			return 1
		}
		return writeJSON(stdout, report, stderr, "boxr image sbom")
	case "lock":
		flags := flag.NewFlagSet("image lock", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "emit the generated lock as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 2 {
			fmt.Fprintln(stderr, "Usage: boxr image lock [--json] NAME:TAG LOCK.json")
			return 2
		}
		lock, err := imageStore.WriteLock(flags.Arg(1), flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr image lock: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, lock, stderr, "boxr image lock")
		}
		fmt.Fprintf(stdout, "locked %s at %s (%s/%s)\n", lock.Reference, lock.Digest, lock.OS, lock.Architecture)
		return 0
	case "verify-lock":
		flags := flag.NewFlagSet("image verify-lock", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "emit the verified lock as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr image verify-lock [--json] LOCK.json")
			return 2
		}
		lock, err := imageStore.VerifyLock(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr image verify-lock: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, lock, stderr, "boxr image verify-lock")
		}
		fmt.Fprintf(stdout, "verified %s at %s\n", lock.Reference, lock.Digest)
		return 0
	case "save":
		flags := flag.NewFlagSet("image save", flag.ContinueOnError)
		flags.SetOutput(stderr)
		format := flags.String("format", "oci", "archive format: oci or docker")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() < 2 || (*format != "oci" && *format != "docker") ||
			(*format == "oci" && flags.NArg() != 2) {
			fmt.Fprintln(stderr, "Usage: boxr image save [--format oci|docker] NAME:TAG [NAME2:TAG ...] ARCHIVE.tar[.gz]")
			return 2
		}
		archivePath := flags.Arg(flags.NArg() - 1)
		references := make([]string, flags.NArg()-1)
		for index := range references {
			references[index] = flags.Arg(index)
		}
		var err error
		if *format == "docker" {
			err = imageStore.SaveDockerMany(references, archivePath)
		} else {
			err = imageStore.Save(references[0], archivePath)
		}
		if err != nil {
			fmt.Fprintf(stderr, "boxr image save: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "saved %s to %s\n", strings.Join(references, ", "), archivePath)
		return 0
	case "load":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: boxr image load ARCHIVE.tar[.gz]")
			return 2
		}
		items, err := imageStore.Load(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "boxr image load: %v\n", err)
			return 1
		}
		for _, item := range items {
			fmt.Fprintf(stdout, "loaded %s %s\n", item.Reference, item.Digest)
		}
		return 0
	case "unpack":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr image unpack NAME:TAG PATH")
			return 2
		}
		manifest, err := imageStore.Unpack(args[1], args[2])
		if err != nil {
			fmt.Fprintf(stderr, "boxr image unpack: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "unpacked %s: %d file(s), %d directory(s), %d symlink(s)\n", args[1], manifest.Files, manifest.Directories, manifest.Symlinks)
		return 0
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "Usage: boxr image rm NAME:TAG [...]")
			return 2
		}
		failed := false
		for _, reference := range args[1:] {
			if err := imageStore.Remove(reference); err != nil {
				fmt.Fprintf(stderr, "boxr image rm: %s: %v\n", reference, err)
				failed = true
			} else {
				fmt.Fprintf(stdout, "untagged %s\n", reference)
			}
		}
		if failed {
			return 1
		}
		return 0
	case "prune":
		flags := flag.NewFlagSet("image prune", flag.ContinueOnError)
		flags.SetOutput(stderr)
		dryRun := flags.Bool("dry-run", false, "list unreferenced blobs without deleting them")
		force := flags.Bool("force", false, "delete unreferenced blobs")
		jsonOutput := flags.Bool("json", false, "write per-blob prune details as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (*dryRun == *force) {
			fmt.Fprintln(stderr, "Usage: boxr image prune [--dry-run|--force] [--json]")
			return 2
		}
		items, err := imageStore.PruneDetailed(!*force)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image prune: %v\n", err)
			return 1
		}
		report := imagePruneReport{DryRun: *dryRun, BlobCount: len(items), Blobs: items}
		for _, item := range items {
			report.ReclaimableBytes += item.SizeBytes
		}
		if *jsonOutput {
			return writeJSON(stdout, report, stderr, "boxr image prune")
		}
		verb := "would remove"
		if *force {
			verb = "removed"
		}
		for _, item := range items {
			fmt.Fprintf(stdout, "%s %s (%d bytes)\n", verb, item.Digest, item.SizeBytes)
		}
		fmt.Fprintf(stdout, "%d unreferenced blob(s), %d bytes %s\n", report.BlobCount, report.ReclaimableBytes, map[bool]string{true: "removed", false: "found"}[*force])
		return 0
	default:
		fmt.Fprintf(stderr, "boxr image: unknown action %q\n", args[0])
		return 2
	}
}

type imagePruneReport struct {
	DryRun           bool                    `json:"dryRun"`
	BlobCount        int                     `json:"blobCount"`
	ReclaimableBytes int64                   `json:"reclaimableBytes"`
	Blobs            []imageStore.PrunedBlob `json:"blobs"`
}

type imageUsageEntry struct {
	Reference    string `json:"reference"`
	Digest       string `json:"digest"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
	LayerBytes   int64  `json:"layerBytes"`
}

type imageUsageSummary struct {
	ImageCount           int                   `json:"imageCount"`
	Images               []imageUsageEntry     `json:"images"`
	ReferencedLayerBytes int64                 `json:"referencedLayerBytes"`
	Store                imageStore.StoreUsage `json:"store"`
}

func filterImageList(items []imageStore.Image, filters []string) ([]imageStore.Image, error) {
	filtered := append([]imageStore.Image(nil), items...)
	for _, filter := range filters {
		key, value, ok := strings.Cut(filter, "=")
		if !ok || value == "" {
			return nil, fmt.Errorf("invalid filter %q (expected reference=PREFIX, os=VALUE, arch=VALUE, variant=VALUE, digest=PREFIX or label=KEY[=VALUE])", filter)
		}
		if key != "reference" && key != "os" && key != "arch" && key != "variant" && key != "digest" && key != "label" {
			return nil, fmt.Errorf("unsupported filter key %q", key)
		}
		if key == "label" && strings.TrimSpace(strings.SplitN(value, "=", 2)[0]) == "" {
			return nil, fmt.Errorf("invalid filter %q (label key cannot be empty)", filter)
		}
		next := filtered[:0]
		for _, item := range filtered {
			match := false
			switch key {
			case "reference":
				match = strings.HasPrefix(item.Reference, value)
			case "os":
				match = item.OS == value
			case "arch":
				match = item.Arch == value
			case "variant":
				match = item.Variant == value
			case "digest":
				match = strings.HasPrefix(item.Digest, value)
			case "label":
				labelKey, labelValue, hasValue := strings.Cut(value, "=")
				stored, exists := item.RuntimeConfig.Labels[labelKey]
				if hasValue {
					match = exists && stored == labelValue
				} else {
					match = exists
				}
			}
			if match {
				next = append(next, item)
			}
		}
		filtered = next
	}
	return filtered, nil
}

func runRootFS(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: boxr rootfs inspect|verify|audit|pack|unpack ...")
		return 2
	}
	switch args[0] {
	case "audit":
		flags := flag.NewFlagSet("rootfs audit", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "emit a machine-readable audit report")
		strict := flags.Bool("strict", false, "return non-zero when any warning or high finding exists")
		maxEntries := flags.Int64("max-entries", rootfs.DefaultAuditMaxEntries, "maximum filesystem entries to inspect")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr rootfs audit [--json] [--strict] [--max-entries N] PATH")
			return 2
		}
		report, err := rootfs.AuditWithLimit(flags.Arg(0), *maxEntries)
		if err != nil {
			fmt.Fprintf(stderr, "boxr rootfs audit: %v\n", err)
			return 1
		}
		if *jsonOutput {
			if code := writeJSON(stdout, report, stderr, "boxr rootfs audit"); code != 0 {
				return code
			}
		} else {
			fmt.Fprintf(stdout, "audited %d entries: %d high, %d warning, %d info\n", report.EntriesScanned, report.HighCount, report.WarningCount, report.InfoCount)
			for _, finding := range report.Findings {
				fmt.Fprintf(stdout, "%-7s %-28s %s: %s\n", strings.ToUpper(finding.Severity), finding.Code, finding.Path, finding.Message)
			}
		}
		if *strict && report.HasFindings() {
			return 1
		}
		return 0
	case "inspect", "verify":
		if len(args) < 2 || len(args) > 3 || (args[0] == "inspect" && len(args) != 2) {
			fmt.Fprintf(stderr, "Usage: boxr rootfs %s PATH [EXPECTED_SHA256]\n", args[0])
			return 2
		}
		manifest, err := rootfs.Inspect(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "boxr rootfs %s: %v\n", args[0], err)
			return 1
		}
		if args[0] == "verify" {
			if len(args) == 3 {
				expected, digestErr := rootfs.NormalizeDigest(args[2])
				if digestErr != nil {
					fmt.Fprintf(stderr, "boxr rootfs verify: %v\n", digestErr)
					return 2
				}
				actual, digestErr := rootfs.Digest(args[1])
				if digestErr != nil {
					fmt.Fprintf(stderr, "boxr rootfs verify: %v\n", digestErr)
					return 1
				}
				if actual != expected {
					fmt.Fprintf(stderr, "boxr rootfs verify: digest mismatch (expected %s, got %s)\n", expected, actual)
					return 1
				}
			}
			root := args[1]
			foundShell := false
			for _, candidate := range []string{"bin/sh", "usr/bin/sh", "bin/busybox"} {
				if info, statErr := os.Lstat(filepath.Join(root, candidate)); statErr == nil && (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
					foundShell = true
					break
				}
			}
			if !foundShell {
				fmt.Fprintln(stderr, "boxr rootfs verify: no /bin/sh, /usr/bin/sh, or /bin/busybox found")
				return 1
			}
			fmt.Fprintf(stdout, "rootfs verified: %s (%d files, %d directories, %d symlinks)\n", root, manifest.Files, manifest.Directories, manifest.Symlinks)
			return 0
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(manifest); err != nil {
			fmt.Fprintf(stderr, "boxr rootfs inspect: %v\n", err)
			return 1
		}
		return 0
	case "pack":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr rootfs pack PATH ARCHIVE.tar.gz")
			return 2
		}
		manifest, err := rootfs.PackTarGz(args[1], args[2])
		if err != nil {
			fmt.Fprintf(stderr, "boxr rootfs pack: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "packed %d files, %d directories, %d symlinks (%d bytes) to %s\n", manifest.Files, manifest.Directories, manifest.Symlinks, manifest.Bytes, args[2])
		return 0
	case "unpack":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr rootfs unpack ARCHIVE.tar.gz PATH")
			return 2
		}
		manifest, err := rootfs.ExtractTarGz(args[1], args[2], rootfs.Limits{})
		if err != nil {
			fmt.Fprintf(stderr, "boxr rootfs unpack: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "extracted %d files, %d directories, %d symlinks (%d bytes) to %s\n", manifest.Files, manifest.Directories, manifest.Symlinks, manifest.Bytes, args[2])
		return 0
	case "digest":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: boxr rootfs digest PATH")
			return 2
		}
		digest, err := rootfs.Digest(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "boxr rootfs digest: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "sha256:%s\n", digest)
		return 0
	case "diff":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr rootfs diff BEFORE AFTER")
			return 2
		}
		changes, err := rootfs.Diff(args[1], args[2])
		if err != nil {
			fmt.Fprintf(stderr, "boxr rootfs diff: %v\n", err)
			return 1
		}
		for _, change := range changes {
			fmt.Fprintf(stdout, "%-8s %s\n", change.Status, change.Path)
		}
		fmt.Fprintf(stdout, "%d change(s)\n", len(changes))
		return 0
	default:
		fmt.Fprintf(stderr, "boxr rootfs: unknown action %q\n", args[0])
		return 2
	}
}

var implementedFeatures = []string{
	"help and usage", "version metadata", "structured version fields", "JSON version output", "read-only host doctor", "JSON doctor output", "strict doctor warning mode",
	"run command with arguments", "unknown command diagnostics", "repeatable environment overrides", "environment key validation", "NUL-safe environment validation", "duplicate environment key collapse", "environment files with comments and export syntax", "single and double quoted env values",
	"hostname isolation", "working directory", "memory hard limit", "memory high threshold", "memory low protection", "memory min protection", "memory swap cap", "memory OOM group", "CPU quota", "CPU weight", "block I/O weight", "PID count limit", "cgroup v2 controller delegation checks", "cgroup rollback on failed setup", "cgroup cleanup after exit",
	"namespace startup barrier", "PID namespace init supervisor", "child process reaping", "signal forwarding", "workload exit-code propagation", "private mount propagation", "optional root filesystem", "chroot execution", "read-only root filesystem", "proc filesystem mount", "recursive bind mounts", "read-only bind mounts", "tmpfs mounts", "tmpfs size cap", "tmpfs mode",
	"private network namespace", "loopback interface activation", "host networking mode", "user namespace UID mapping", "user namespace GID mapping", "capability name parser", "drop selected Linux capabilities", "drop all Linux capabilities", "no-new-privileges",
	"rootfs archive path traversal rejection", "rootfs archive size limits", "rootfs archive entry limits", "rootfs safe symlink extraction", "rootfs absolute symlink normalization", "rootfs hard-link extraction", "rootfs special-file rejection", "rootfs atomic extraction", "rootfs pack", "rootfs inspect", "rootfs verify", "rootfs deterministic SHA-256 digest", "rootfs digest verification", "rootfs directory diff with added/modified/removed paths", "rootfs security audit without following symlinks", "rootfs audit detects escaping relative symlinks", "rootfs audit detects setuid files", "rootfs audit detects setgid files", "rootfs audit detects world-writable paths", "rootfs audit treats sticky world-writable directories as informational", "rootfs audit detects special files", "rootfs audit JSON report", "rootfs audit strict failure mode", "image audit by local tag", "image audit reuses safe rootfs extraction", "image audit JSON report", "image audit strict failure mode", "rootfs audit configurable entry limit", "rootfs audit rejects non-positive entry limits", "rootfs audit fails on limit rather than returning partial results", "image audit configurable entry limit",
	"versioned JSON run config", "strict JSON config field validation", "config size limit", "config example generator", "config name inheritance", "Go source formatting target", "race-enabled test target", "go vet target", "cross-platform CLI builds", "Linux amd64 build", "Linux arm64 build", "Windows CLI build", "macOS Intel CLI build", "macOS Apple Silicon CLI build",
	"named containers", "collision-resistant 12-character container IDs", "duplicate container-name protection", "persistent JSON container records", "private 0600 container state files", "environment-secret redaction from state", "container labels", "created lifecycle state", "running lifecycle state", "exit status persistence", "container create/start/exit/remove event journal", "bounded event-log reader", "recent event tail limit", "active container listing", "completed container history listing", "container filter by ID prefix", "container filter by name", "container filter by status", "container filter by label", "container inspect JSON", "completed-container removal", "running-container removal protection", "remove-all-completed workflow", "automatic record removal after exit", "age-based state pruning", "live cgroup memory stats", "live cgroup CPU stats", "live cgroup PID stats", "live cgroup I/O stats", "container stats JSON output", "host and runtime info report", "host info JSON output", "non-Linux stats limitation reporting",
	"combined stdout and stderr log capture", "private 0600 container log files", "bounded container log reads", "log tail by line count", "live log following",
	"container stop with graceful timeout", "container kill with named signals", "container wait for completion", "signal-aware exit code persistence", "log cleanup on container removal",
	"named persistent volumes", "volume create with metadata", "volume labels", "volume inspect JSON", "volume list table and JSON", "volume duplicate-name protection", "volume path traversal rejection", "volume directory permission hardening", "volume metadata permission hardening", "volume symlink path rejection", "volume remove with data cleanup", "volume prune with explicit force", "active volume usage detection", "run with named volume bind mounts", "read-only named volume mount", "named volume persistence across runs",
	"volume list name and label filtering", "volume prune label filtering", "volume export as safe gzip tar archive",
	"OverlayFS writable upper layer", "persistent overlay upper and work directories", "overlay path traversal and nesting validation", "overlay read-only remount compatibility", "overlay mount path option validation", "separate writable layer per caller-selected directory", "OverlayFS upper/work concurrency lock", "canonical OverlayFS storage lock path", "symlink-resistant OverlayFS lock file", "private 0600 OverlayFS lock permissions", "OverlayFS lock alias contention detection", "empty OverlayFS workdir and mountpoint validation",
	"local OCI image layout store", "OCI image index schema validation", "OCI image manifest descriptors", "OCI image config descriptors", "content-addressed SHA-256 blob storage", "custom documented Boxr rootfs layer media type", "rootfs import as image reference", "strict image reference validation", "image tag replacement without duplicate index entries", "image listing in table format", "image listing in JSON format", "quiet image reference listing", "image inspection with platform and layer metadata", "image manifest blob integrity verification", "image config blob integrity verification", "image layer blob integrity verification", "safe image unpack to new rootfs destination", "image tag removal without deleting shared blobs", "portable OCI layout archive save", "portable OCI layout archive load", "archive descriptor validation before import", "image blob garbage collection dry run", "image blob garbage collection with explicit force", "private symlink-resistant image store", "atomic OCI index updates", "local image retagging without copying blobs", "image alias removal preserves shared content", "built-in Linux seccomp baseline profile", "seccomp architecture guard in BPF filter", "seccomp denylist for namespace and kernel administration syscalls", "seccomp denylist for kernel module and BPF operations", "seccomp denylist for process tracing and cross-process memory access", "seccomp denylist for perf and userfaultfd", "seccomp automatically enables no-new-privileges", "seccomp profile name validation", "seccomp filter generation tests", "seccomp filter subprocess integration test", "OCI uncompressed layer diff_id verification", "image prune rejects unexpected blob filenames", "image store root symlink rejection", "streaming SHA-256 verification for image layers", "bounded OCI blob sizes", "streaming portable image save and load", "full image graph validation before garbage collection",
	"redacted container execution dry-run plan", "dry-run validates complete runtime options", "dry-run reports environment variable names only", "dry-run redacts environment and label values", "dry-run avoids creating container lifecycle records",
	"explicit local image integrity verification command", "image store unique blob count and byte accounting", "image store reachable content byte accounting", "image store reclaimable content byte accounting", "full image store blob digest audit",
	"OCI Distribution registry reference parser",
	"Docker Hub default registry resolution",
	"Docker Hub implicit library namespace resolution",
	"custom registry host and port references",
	"registry pull by tag",
	"registry pull by immutable SHA-256 manifest digest",
	"registry manifest-list platform selection",
	"registry platform defaulting to Linux host architecture",
	"OCI Distribution Bearer challenge handling",
	"scoped registry access-token requests",
	"registry username and password environment credentials",
	"HTTPS-only token realms for HTTPS registries",
	"registry manifest digest verification",
	"registry image config blob digest verification",
	"streaming registry rootfs layer digest verification",
	"registry descriptor size verification",
	"registry image config platform verification",
	"registry rootfs diff_id verification",
	"bounded registry manifest and config downloads",
	"bounded streaming registry layer downloads",
	"explicit unsupported registry layer media type errors",
	"temporary registry layer cleanup",
	"registry image import into local OCI layout",
	"digest-pinned pull mapped to stable local tag",
	"OCI Distribution image push by tag",
	"registry image push rejects immutable digest targets",
	"registry push requests pull and push token scope",
	"registry Bearer token reuse during upload",
	"registry blob mount before uploading content",
	"OCI monolithic registry blob upload",
	"registry relative upload Location resolution",
	"registry upload Location same-origin enforcement",
	"streamed large rootfs layer uploads from disk",
	"registry upload request replay after authentication challenge",
	"registry config and layer descriptor verification before upload",
	"registry rootfs diff_id verification before upload",
	"registry manifest publication after blobs",
	"registry published manifest digest verification",
	"verified local OCI bundle export for registry push",
	"private stable temporary layer copy for registry export",
	"registry image push CLI supports local and remote references",
	"registry credentials restricted to trusted token hosts",
	"registry host port validation",
	"registry manifest response content type validation",
	"registry image config media type validation",
	"manifest-list descriptor media type and size validation",
	"registry Content-Length validation for rootfs layer downloads",
	"registry image push supports localhost test registries",
	"registry cross-platform pulls preserve OS architecture and variant in the local OCI image config",
	"registry auth file explicit path support",
	"Docker config.json inline auth support",
	"Podman auth.json inline auth support",
	"base64 username-password registry auth decoding",
	"Docker Hub legacy auth host alias matching",
	"registry auth file symlink rejection",
	"registry auth file regular-file validation",
	"registry auth file size limit",
	"registry auth file JSON validation",
	"environment registry credentials precedence over auth files",
	"REGISTRY_AUTH_FILE credential discovery",
	"DOCKER_CONFIG credential discovery",
	"default Docker and Podman credential file discovery",
	"credential helper execution is explicitly disabled",
	"registry push authfile flag",
	"registry pull authfile flag",
	"event query by action type", "event query by exact container name", "event query by container ID prefix", "event query by relative time window", "event query by RFC3339 timestamp", "event filtering before result limiting",
	"safe volume restore from gzip-tar archive", "volume import uses rootfs archive path validation", "volume import rejects escaping symlinks and duplicate paths", "volume import stages extraction before publication", "volume import atomically publishes completed data", "volume import supports metadata labels", "volume import cleans up failed extraction", "portable volume export and import round trip",
	"volume logical disk usage report", "volume usage counts regular files and directories", "volume usage counts symlink target lengths", "volume usage never follows symlinks", "volume usage reports special entries separately", "volume usage exposes JSON and snapshot semantics",
	"volume clone by portable archive snapshot", "volume clone rejects a source in active use", "volume clone restores into a distinct target volume", "volume clone supports target metadata labels", "volume clone cleans temporary archive after completion", "volume clone uses hardened archive extraction",
	"ps quiet ID-only output", "ps newest-N selection across completed and active records", "ps non-truncated command display", "image list reference-prefix filtering", "image list operating-system filtering", "image list architecture filtering", "image list AND-combined filters", "image list rejects unsupported filters",
	"state directory symlink rejection", "state record symlink and file-type validation", "event journal symlink rejection", "container log symlink rejection on read and write",
	"image audit verifies manifest descriptor graph", "image audit verifies config descriptor digest", "image audit verifies compressed layer digest", "image audit verifies uncompressed rootfs diff ID", "image audit snapshots verified layer under store lock", "image audit rechecks snapshot digest before extraction", "image audit reports image platform metadata", "image audit reports manifest and config digests", "image audit reports layer media type and digest", "image audit combines integrity and filesystem findings in JSON", "run directly from a verified local image tag", "run image extraction is isolated in a private temporary directory", "run image temporary rootfs is removed after command completion", "run image dry-run verifies metadata without extracting the image", "run image and explicit rootfs options are mutually exclusive", "run image provenance is recorded as a container label",
	"safe non-executing JSON image build recipe", "image build strict required tag validation", "image build configurable recipe path", "image build context directory selection", "image build explicit COPY files and directories", "image build runtime environment metadata", "image build entrypoint and command metadata", "image build working-directory creation", "image build labels and user metadata", "image build JSON metadata output", "image build quiet reference output", "image build rejects unknown recipe fields and executable RUN steps",
	"image build bounded .boxrignore file", "image build .boxrignore comment and blank-line parsing", "image build .boxrignore basename glob matching", "image build .boxrignore nested path glob matching", "image build .boxrignore directory subtree exclusion", "image build refuses negated ignore patterns", "image build rejects ignored explicit COPY sources", "image build rejects symlinked ignore files", "image build rejects oversized ignore files", "image build creates parent directories for file COPY destinations",
	"image build explicit Linux target platform selection", "image build supports amd64 and arm64 target images", "image build supports ARM v5 v6 and v7 variants", "image build supports ARM64 v8 variant metadata", "image build supports 386 ppc64le riscv64 and s390x metadata", "image build rejects non-Linux and malformed platform declarations",
	"image diff compares two verified local image rootfs trees", "image diff reports added paths", "image diff reports removed paths", "image diff reports modified file contents and metadata", "image diff does not follow rootfs symlinks", "image diff supports JSON output", "image diff stages image extraction in a private temporary directory", "image diff removes staging files after comparison",
	"image lock pins a tag to its immutable manifest digest", "image lock records OS architecture and variant", "image lock uses a versioned strict JSON schema", "image lock file is written atomically", "image lock file is private mode 0600", "image lock rejects symlinked destinations and parents", "image lock rejects non-regular and oversized files", "image lock verification detects mutable tag drift", "image lock rejects unknown JSON fields and trailing data", "image lock supports JSON CLI output",
	"image SBOM inventories the verified rootfs filesystem", "image SBOM records the manifest digest and target platform", "image SBOM records regular file sizes and permission modes", "image SBOM generates per-file SHA-256 checksums", "image SBOM records symlink targets without following links", "image SBOM reports directories and symlinks separately", "image SBOM deterministic path ordering", "image SBOM configurable entry limit", "image SBOM rejects non-positive entry limits", "image SBOM fails rather than return a partial inventory", "image SBOM private temporary extraction cleanup",
	"rootfs archive normalizes entry modification timestamps", "rootfs archive normalizes access and change timestamps", "rootfs archive normalizes UID and GID metadata", "rootfs archive omits host account names", "rootfs archives are byte-identical for equivalent trees with different mtimes",
	"named runtime security profiles", "default security profile preserves explicit runtime settings", "baseline security profile enforces no-new-privileges", "baseline security profile enables the built-in seccomp filter", "baseline security profile drops NET_RAW", "restricted security profile disables container networking", "restricted security profile enables a private user namespace", "restricted security profile enforces no-new-privileges and seccomp", "restricted security profile drops high-risk Linux capabilities", "untrusted security profile requires an explicit root filesystem", "untrusted security profile enforces read-only rootfs", "security profiles apply after user flags so weaker flags cannot relax mandatory controls", "security profile is recorded in container metadata and dry-run plans", "strict JSON Schema for versioned run configuration", "config schema command emits a JSON Schema document", "config schema rejects unknown top-level properties", "config schema describes security profile options", "system storage usage summary", "system storage usage includes verified image-store bytes", "system storage usage includes named volume logical bytes", "system storage usage includes container metadata and logs", "system storage usage JSON output", "system storage scanner does not follow symlink entries", "system storage scanner rejects symlinked roots", "system storage scanner treats absent directories as empty",
	"named resource sizing profiles", "micro resource profile defaults", "small resource profile defaults", "medium resource profile defaults", "large resource profile defaults", "resource profiles fill only unset memory limits", "resource profiles fill only unset CPU quotas", "resource profiles fill only unset PID limits", "resource profiles fill CPU and I/O weights", "resource profile preserves explicit config limits", "resource profile preserves explicit command-line overrides", "resource profile name is validated", "resource profile appears in dry-run plans", "image list filter by ARM variant", "image list filter by digest prefix", "image list filter by metadata label presence", "image list filter by exact metadata label value", "image list filters combine with AND semantics across platform and metadata", "image list rejects empty metadata label filter keys",
	"privacy-preserving support bundle generation",
	"support bundle includes Boxr build and Go runtime metadata",
	"support bundle includes diagnostic names and statuses without detail strings",
	"support bundle excludes environment values and command arguments",
	"support bundle excludes container logs, state files, configuration and filesystem paths",
	"support bundle rejects symlink destinations and requires explicit overwrite confirmation",
	"support bundle archives and reports a SHA-256 digest",
	"support bundle archive and entries use private file permissions",
	"support bundle includes host kernel release metadata",
	"support bundle reports cgroup v2 availability",
	"live container statistics watch mode",
	"configurable container statistics sampling interval",
	"bounded container statistics sampling count",
	"timestamped JSON-lines container statistics output",
	"statistics watch exits cleanly when its container stops",
	"statistics watch rejects invalid intervals and counts",
	"statistics watch preserves the existing one-shot JSON interface",
	"statistics watch reports output and cgroup read errors",
	"live container event streaming",
	"event follow supports action and container filters",
	"event follow supports configurable polling intervals",
	"event follow supports bounded capture counts",
	"event follow streams compact JSON Lines or table rows",
	"event follow supports since-based historical catch-up",
	"event follow handles event log truncation by resuming",
	"event follow reports event source and output errors",
	"wait for container startup readiness",
	"wait for container process completion",
	"configurable lifecycle wait polling interval",
	"lifecycle wait enforces timeout for either condition",
	"lifecycle wait returns structured JSON results",
	"lifecycle wait reports containers that exit before becoming ready",
	"lifecycle wait supports multiple container IDs",
	"lifecycle wait preserves the default exit-code output",
	"container prune supports non-mutating dry runs",
	"prune dry run lists candidate container IDs and states",
	"prune dry run respects age thresholds",
	"prune dry run excludes running and never-started containers",
	"prune supports machine-readable JSON planning output",
	"prune dry run leaves container records and logs untouched",
	"named volume prune supports non-mutating dry runs",
	"volume prune previews unused volume names and labels",
	"volume prune reports volumes protected by active containers",
	"volume prune dry run respects name and label filters",
	"volume prune supports machine-readable JSON plans",
	"volume prune requires force for destructive cleanup",
	"container inspect optionally reports state and log disk usage",
	"inspect size report includes separate state and log byte counts",
	"inspect size report includes total bytes",
	"inspect size scanner treats an absent log as zero bytes",
	"inspect size scanner rejects symlink and special-file targets",
	"container stats supports human-readable table output",
	"container stats watch supports live table rows",
	"container stats table includes timestamp memory CPU and PID metrics",
	"container stats preserves JSON as the default output format",
	"volume du inventories every named volume",
	"volume du aggregates file and directory counts",
	"volume du aggregates logical bytes across volumes",
	"volume du all mode supports name and label filters",
	"volume du all mode supports JSON summary output",
	"image du inventories every tagged image",
	"image du reports per-tag rootfs layer bytes and platform",
	"image du all mode supports existing image metadata filters",
	"image du distinguishes referenced layer bytes from unique store bytes",
	"image du all mode supports JSON output",
	"image du preserves the existing unique-blob usage report",
	"image prune supports structured per-blob JSON reports",
	"image prune reports the size of each unreferenced blob",
	"image prune aggregates reclaimable bytes before deletion",
	"image prune dry-run preserves every unreferenced blob",
	"image prune retains explicit force confirmation for deletion",
	"standard Docker save archive import through image load",
	"plain tar and gzip-compressed Docker archive support",
	"bounded Docker archive extraction and strict path validation",
	"Docker archive manifest and config size validation",
	"Docker archive repository tag and platform validation",
	"multi-layer Docker root filesystem application in layer order",
	"Docker layer uncompressed SHA-256 diff_id verification",
	"Docker layer whiteout deletion semantics",
	"Docker opaque-directory whiteout semantics",
	"Docker image environment entrypoint command working-directory user and label preservation",
	"Docker archive multi-tag import with complete preflight validation",
	"Docker layer safe symlink and hard-link handling with special-file rejection",
	"Docker-save-compatible image archive export",
	"plain uncompressed tar output compatible with Docker load",
	"Docker image config filenames derived from SHA-256 config identities",
	"Docker archive repositories tag map for legacy-compatible consumers",
	"Docker export preserves rootfs diff IDs platform and runtime config",
	"Docker image layer is rehashed while expanding it for export",
	"format-selectable image save with the existing OCI format as default",
	"multi-image Docker save export from a single command",
	"combine tags with identical Docker image configurations into one manifest entry",
	"deduplicate shared rootfs layers across Docker image exports",
	"deterministic ordering of Docker manifest config and layer entries",
	"enforce Docker export image-count and duplicate-reference limits",
	"standard OCI gzip layer media type support",
	"standard OCI uncompressed tar layer media type support",
	"Docker distribution gzip layer media type support",
	"Docker distribution uncompressed tar layer media type support",
	"bounded multi-layer registry manifest validation",
	"per-layer compressed registry blob digest verification",
	"per-layer uncompressed rootfs diff_id verification",
	"ordered multi-layer registry filesystem application",
	"registry layer whiteout and opaque-directory semantics",
	"versioned multi-service deployment manifests",
	"strict bounded deployment manifest loading",
	"deployment service and stack name validation",
	"per-service command and runtime option validation",
	"deployment dependency reference validation",
	"duplicate and self-dependency rejection",
	"deployment dependency cycle detection",
	"deterministic topological deployment ordering",
	"deployment plan service resource summaries",
	"secret-redacted JSON deployment plans",
	"deployment plan example and validation commands",
	"local OCI image references in deployment services",
	"stable stack-scoped service container names",
	"reserved deployment identity labels",
	"attached multi-service deployment supervisor",
	"dependency-ordered service process startup",
	"service running-state startup gates",
	"fail-fast service exit handling",
	"reverse-order graceful supervisor shutdown",
	"stack-scoped deployment process status",
	"reverse-dependency-order deployment shutdown",
	"deployment stop timeout escalation",
	"safe optional deployment record and log removal",
	"deployment lifecycle dry-run planning",
	"parallel startup of dependency-independent deployment services",
	"deployment-wide unique registry image prefetch",
	"deployment pull platform selection",
	"deployment pull registry auth-file support",
	"canonical local image reference resolution before service start",
	"deployment pull validates cached image architecture and variant",
	"image-backed service startup enforces the host Linux platform",
	"deployment TCP readiness probes for loopback services",
	"deployment HTTP readiness probes for loopback services",
	"readiness targets reject non-loopback destinations and URL credentials",
	"readiness probe intervals timeouts attempts and status codes are bounded",
	"readiness retries honor supervisor cancellation and startup deadlines",
	"secret-redacted readiness metadata in deployment plans",
	"readiness validation accounts for mandatory network-isolating security profiles",
	"dependent service startup waits for configured readiness probes",
	"parallel service waves select readiness probes by service identity",
	"deployment-wide wait for all services running",
	"deployment-wide wait for all services stopped",
	"bounded stack-state polling with configurable deadlines",
	"secret-redacted JSON deployment wait results",
	"non-executing image recipes support scratch and local OCI image bases",
	"local image bases are integrity-verified before reuse",
	"image base platforms must match requested Linux build targets",
	"base filesystems are safely extracted before context COPY operations",
	"base environment variables inherit with deterministic key-based overrides",
	"base entrypoint command working directory user and labels inherit with build overrides",
	"stack-wide service-prefixed deployment logs",
	"deployment logs filter by service name",
	"follow mode streams and drains all service logs",
	"image build writes resolve in-root symlinks and reject host-root escapes",
	"container-absolute COPY symlinks are normalized inside the staged rootfs",
	"versioned deployment named persistent volume mounts",
	"named volume names and mount targets validated before startup",
	"deployment preflights every named volume before creating services",
	"named volumes reuse Boxr's secure persistent volume store",
	"volume archive exports never overwrite an existing destination",
	"volume archive exports publish atomically from a same-directory staging file",
	"volume export JSON includes SHA-256 integrity and filesystem manifest metadata",
	"versioned deployment service profiles",
	"validated lowercase service profile identifiers and duplicate rejection",
	"unprofiled baseline services remain enabled by default",
	"repeatable command-line service profile selection",
	"additive activation of services matching any selected profile",
	"profile-disabled services are omitted from deployment plans",
	"full manifest validation occurs before profile filtering",
	"unknown deployment profiles are rejected",
	"profile-filtered dependency graphs reject inactive dependencies",
	"deployment image prefetch and startup are limited to selected profiles",
	"deterministic service profile inventory",
	"profile inventory groups services by opt-in profile",
	"always-enabled services appear separately from opt-in profiles",
	"services assigned to multiple profiles appear in every matching group",
	"secret-free JSON and text profile inventory output",
	"per-service ordered environment file loading in deployments",
	"deployment environment files override service-inherited values in declaration order",
	"explicit service environment entries override environment file values",
	"environment file path traversal and duplicate path rejection",
	"deployment environment file count and path length bounds",
	"secret-free environment file metadata in deployment plans",
	"per-service startup timeout overrides the deployment-wide default",
	"zero per-service startup timeout inherits the deployment-wide default",
	"service startup timeout bounds are validated between 10 milliseconds and 10 minutes",
	"secret-free deployment plans expose per-service startup timeout metadata",
	"per-service startup timeouts govern process and readiness startup gates",
	"per-service shutdown grace overrides the stack-wide deploy down timeout",
	"zero per-service shutdown grace inherits the stack-wide command timeout",
	"service shutdown timeout bounds are validated between 10 milliseconds and 10 minutes",
	"per-service shutdown grace is persisted as reserved non-secret runtime metadata",
	"attached deployment rollback and interrupt shutdown honor service-specific grace periods",
	"deploy down applies each service-specific shutdown deadline independently",
	"only services exceeding their own shutdown grace are escalated to SIGKILL",
	"recurring per-service TCP or HTTP healthchecks run after startup gates pass",
	"healthcheck intervals are bounded from 10 milliseconds to 60 seconds",
	"healthcheck retry budgets are bounded and count consecutive failures",
	"successful healthchecks reset the consecutive failure counter",
	"healthcheck startup grace periods are bounded to 10 minutes",
	"unhealthy services fail the attached deployment supervisor and trigger stack shutdown",
	"healthcheck targets remain loopback-only and absent from deployment plans",
	"managed bridge network creation with an owner-private persistent metadata store",
	"versioned network store with bounded strict JSON decoding",
	"network metadata writes are atomically published with private file permissions",
	"IPv4 bridge subnets are canonical and bounded between /16 and /29",
	"bridge network gateways are validated as usable in-subnet addresses",
	"bridge networks reject overlapping IPv4 subnets",
	"bridge interface names are validated against Linux interface length limits",
	"bridge provisioning creates and raises a host Linux bridge interface",
	"bridge networks optionally enable scoped IPv4 forwarding and NAT",
	"network-specific firewall chains isolate Boxr forwarding and masquerade rules",
	"network deletion removes only its own firewall chains and bridge interface",
	"per-network IPv4 address allocation excludes network broadcast and gateway addresses",
	"container network attachments reserve unique addresses by container ID",
	"bridge attachments create host-side veth pairs and join container net namespaces",
	"container bridge interfaces receive an eth0 address and default gateway",
	"failed bridge attachment rolls back its veth and IPv4 allocation",
	"container exit removes its bridge veth and releases its address",
	"bridge network removal is blocked while container attachments remain",
	"network list and inspect provide sorted secret-free JSON metadata",
	"bridge mode requires an explicit managed network name",
	"bridge mode creates a private network namespace before host-side attachment",
	"container run joins and leaves bridge networks through lifecycle hooks",
	"network metadata rejects duplicate IDs names bridges and invalid allocations",
	"host TCP port mappings support IPv4 host bind addresses and container destination ports",
	"host UDP port mappings support IPv4 host bind addresses and container destination ports",
	"published port protocol defaults to TCP and accepts explicit TCP or UDP",
	"published port bindings validate host and container port ranges from 1 to 65535",
	"published port bindings normalize wildcard IPv4 host address and protocol",
	"wildcard host port mappings conflict with specific address mappings on same port",
	"host port reservations are checked across all managed bridge networks",
	"bridge port mappings install destination NAT rules for inbound traffic",
	"bridge port mappings install host OUTPUT NAT rules for host-originated connections",
	"published port forwarding allows new traffic only to mapped container addresses and ports",
	"published port firewall rules are removed when bridge container exits",
	"partial port mapping firewall setup is rolled back with veth and IPAM state",
	"published port mappings are persisted with network leases in private network metadata",
	"network store validates canonical published port mapping and cross-network reservation uniqueness",
	"run CLI supports repeatable --publish and -p mappings with IPv4 host bind and protocol syntax",
	"run config JSON schema describes strict published port mapping objects",
	"dry-run plans show normalized published port mapping values without starting container",
	"published ports are accepted only with a named managed bridge network",
	"deployment manifests validate service published port mappings through container option rules",
	"deployment service plans expose normalized TCP and UDP published port bindings",
	"deployment subprocess runs apply and release published port rules through the container lifecycle",
	"Linux network store transactions use an advisory cross-process lock for IPAM and port reservations",
	"Linux network store lock files are opened without following symlinks and restricted to owner read/write",
	"non-Linux CLI builds retain process-local network store locking without claiming runtime support",
	"published port setup checks exact PREROUTING OUTPUT and FORWARD rules before insertion",
	"published port rollback removes only firewall rules inserted by the current attachment attempt",
	"failed firewall cleanup retains the IPAM lease and host-port reservation until cleanup can be retried",
	"live network connect attaches a running Boxr container with a private network namespace to a managed bridge",
	"live network disconnect removes a running container's managed bridge interface and address lease",
	"live bridge attachment requires the container to have been started with network mode none",
	"live bridge attachment rejects host-network containers before attempting namespace entry",
	"live bridge attachment rejects user-namespace containers when host-side namespace control is unavailable",
	"live network connect verifies the target PID executable and Boxr namespace-helper command line",
	"live network connect supports repeatable TCP and UDP published host ports",
	"container exit detaches dynamically connected managed networks even when they were not present at run startup",
	"container exit releases dynamically connected published port rules veth links and IPAM reservations",
	"network detach is idempotent when a container has no remaining managed network lease",
	"managed bridge IPAM prevents a single container from acquiring conflicting eth0 attachments on multiple networks",
	"managed veth interfaces carry a network and container-specific Boxr ownership alias",
	"bridge disconnect verifies the Boxr ownership alias before deleting a host veth",
	"stale network attachment discovery filters leases against verified live Boxr helper PIDs",
	"network prune defaults to a read-only stale attachment report",
	"network prune requires an explicit force flag before changing host interfaces or firewall rules",
	"stale attachment pruning removes only Boxr-owned veths and matching published port rules",
	"stale attachment pruning releases the corresponding IPv4 IPAM lease after successful cleanup",
	"network prune retains reservations when a veth has a foreign or missing ownership alias",
	"network prune reports removed attachment IPv4 and published port counts",
	"runtime process liveness checks validate the Boxr helper command line and network namespace",
	"bounded parallel OCI and Docker registry layer downloads",
	"registry layer download worker count capped at four",
	"parallel registry layer verification preserves manifest order",
	"registry pull cancels outstanding layer downloads after the first failure",
	"parallel registry pull removes every partial temporary layer on failure",
	"concurrent registry requests share a synchronized Bearer token refresh",
	"OCI registry pulls accept Zstandard-compressed tar layers",
	"Zstandard registry layer digest verification streams decompressed bytes",
	"Zstandard decompression caps decoder memory and window at 256 MiB",
	"rootfs layer application auto-detects Zstandard-compressed tar archives",
	"pulled Zstandard layers are verified against OCI uncompressed diff IDs before application",
	"Linux detached run launches a dedicated Boxr supervisor in a new session",
	"detached run waits for a post-cgroup startup-barrier handshake before returning the container ID",
	"detached supervisor captures launch diagnostics in an owner-only state-directory log",
	"detached run reports container ID host PID and launch log path after successful startup",
	"detached supervisor uses the standard persistent container lifecycle record and log stream",
	"detached mode rejects dry-run and preserves workload arguments through the standard flag parser",
	"registry pushes convert Boxr's private rootfs layer media type to standard OCI gzip layer media type",
	"registry pushes preserve compressed layer bytes and SHA-256 digest during OCI media type normalization",
	"registry push verifies the published digest of the portable normalized OCI manifest",
	"detached container restart policies support no on-failure always and unless-stopped modes",
	"on-failure restart policy evaluates persisted non-zero container exit status",
	"on-failure restart policy supports a bounded retry count with zero meaning unlimited",
	"detached restart delay defaults to one second and is bounded from 100 milliseconds to five minutes",
	"restarted container attempts retain one persistent container ID and one log stream",
	"container state records persist restart attempt count and previous exit status",
	"manual stop intent is persisted before termination signals and suppresses restart policies",
	"boxr stop cancels a pending restart delay without signalling a non-existent process",
	"terminating boxr kill signals cancel a pending restart delay and suppress future attempts",
	"restarting containers remain visible in active container listings during backoff",
	"container removal is blocked while a restart attempt is pending",
	"restart policies require detached execution and reject automatic record removal",
	"Dockerfile parser enforces byte line and instruction-count limits",
	"Dockerfile builds support scratch and verified local single-stage FROM bases",
	"Dockerfile COPY supports local files directories and the build-context root",
	"Dockerfile WORKDIR resolves relative paths inside the container rootfs",
	"Dockerfile ENV and LABEL support quoted values and variable expansion",
	"Dockerfile USER and shell or exec CMD and ENTRYPOINT metadata are preserved",
	"Dockerfile builds apply bounded .dockerignore exclusions without mutating the context",
	"Dockerfile RUN ADD multi-stage builds and unsupported flags fail explicitly",
	"Dockerfile LABEL accepts dotted and namespaced metadata keys",
	"Dockerfile COPY supports multiple local sources with inferred destination directories",
	"Dockerfile RUN shell and exec instructions execute inside Boxr namespaces",
	"Dockerfile RUN commands operate on the staged image rootfs after preceding build steps",
	"Dockerfile RUN receives a clean host-independent environment with ARG and ENV values",
	"Dockerfile RUN defaults to an isolated network namespace",
	"Dockerfile RUN host networking requires an explicit --network host opt-in",
	"Dockerfile RUN enforces user namespaces no-new-privileges baseline seccomp and drops all capabilities",
	"Dockerfile RUN failures abort the build before the image tag is published",
	"Dockerfile RUN after a non-root USER is rejected rather than silently run as root",
	"exec runs an additional command in an existing container",
	"exec enters pinned mount UTS IPC network PID and optional user namespaces",
	"exec launcher joins the container resource cgroup before namespace entry",
	"exec inherits container environment and supports environment and env-file overrides",
	"exec reapplies configured capability no-new-privileges and seccomp restrictions",
	"exec forwards standard input output and error and propagates command exit status",
	"container records capture Linux process start-time ticks to detect PID reuse",
	"Dockerfile build accepts repeatable --build-arg KEY=VALUE overrides",
	"Dockerfile ARG defaults can be overridden by explicit build arguments",
	"pre-FROM ARG values parameterize FROM and require redeclaration inside the stage",
	"build argument values are available to RUN but are not persisted as image ENV",
	"invalid and unused Dockerfile build arguments are rejected",
	"Dockerfile COPY relative destinations resolve against the base image WORKDIR",
	"Dockerfile build API accepts explicit network and build-argument options",
	"Dockerfile ADD supports local context files and directories",
	"Dockerfile ADD automatically extracts plain tar archives",
	"Dockerfile ADD automatically extracts gzip-compressed tar archives",
	"Dockerfile ADD archive extraction applies rootfs path traversal link and special-file protections",
	"Dockerfile ADD archive extraction enforces bounded file counts and uncompressed byte limits",
	"Dockerfile ADD rejects remote URL and Git sources instead of silently fetching them",
	"deployment loader accepts a bounded single-document Docker Compose YAML file alongside strict JSON manifests",
	"Compose YAML rejects unknown fields duplicate mapping keys and multiple documents",
	"Compose services translate literal image references and safely tokenized command strings or arrays",
	"Compose environment maps and lists resolve pass-through keys and relative env_file paths",
	"Compose depends_on supports deterministic service_started dependency ordering",
	"Compose profiles labels working directories and hostnames map to Boxr deployment options",
	"Compose short-form IPv4 TCP and UDP port mappings map to Boxr published bridge ports",
	"Compose services attach only to explicit pre-created external Boxr bridge networks or explicit host/none modes",
	"unsupported Compose builds implicit volume creation implicit networks healthchecks TTY privileged mode and restart policies fail explicitly",
	"exec supports explicit -i stdin forwarding and closes stdin by default",
	"exec -t allocates a pseudo-terminal with a controlling session and combines terminal output",
	"exec TTY mode propagates SIGWINCH terminal size changes and restores host terminal settings on exit",
	"Compose external named volumes map to pre-created Boxr-managed volumes",
	"Compose relative host bind paths resolve from the Compose file directory",
	"Compose bind sources reject missing paths symlink components and special files",
	"Compose bind and external volume mounts support explicit read-only and read-write modes",
	"image-backed deployments defer bind mounts until after image rootfs extraction",
	"Compose-created volumes drivers options labels long-form mounts and propagation flags are rejected",
	"Compose read_only maps image-backed services to a read-only root filesystem",
	"Compose cap_drop validates and applies supported Linux capability drops",
	"daemon serves the initial Docker Engine API through a local Unix domain socket only",
	"daemon refuses TCP exposure symlinked ancestors shared writable socket directories and non-socket path replacement",
	"daemon detects active and stale sockets uses owner-only 0600 permissions and removes only its own socket",
	"Engine API implements Docker ping and negotiated version information within API v1.24 to v1.45",
	"Engine API exposes runtime-backed info and filtered container list metadata",
	"Engine API exposes container inspect state mounts network port bindings and restart metadata",
	"Engine API streams bounded tail and follow container logs in Docker multiplexed stdout frames",
	"Engine API exposes cgroup v2 CPU memory and PID stats with snapshot and streaming responses",
	"Engine API streams persisted container lifecycle events and lists OCI image tags",
	"unsupported Docker Engine restart attach image volume network mutation and TCP listener operations are explicitly outside the API contract",
	"Engine API creates containers from verified local images using a strict supported Docker create subset",
	"Engine API starts created containers through the native Boxr detached supervisor",
	"Engine API gracefully stops containers with a bounded timeout and SIGKILL fallback",
	"Engine API sends validated Docker kill signals only to process-identity-verified running containers",
	"Engine API removes created and stopped containers through Docker DELETE with safe conflict handling",
	"Engine API inspects verified local OCI images using Docker-compatible image metadata and uncompressed layer DiffIDs",
	"Engine API removes unused local image tags and rejects removal while containers reference them",
	"Engine API pulls and verifies OCI/Docker registry images using Docker X-Registry-Auth and Linux platform selection",
	"Engine API pushes verified local single-layer OCI images using Docker X-Registry-Auth and registry upload verification",
	"Engine API creates Docker image tag aliases from verified OCI content and protects tags referenced by containers",
	"top-level pull command reuses verified OCI and Docker registry image downloads",
	"top-level push command reuses verified OCI and Docker registry image uploads",
	"native create command records a verified image-backed container without starting it",
	"native start command launches a created image-backed container through the detached supervisor",
	"native start command restarts stopped image-backed containers through the detached supervisor"
}

func printFeatures(stdout io.Writer) {
	fmt.Fprintf(stdout, "boxr implemented capabilities (%d)\n", len(implementedFeatures))
	for i, feature := range implementedFeatures {
		fmt.Fprintf(stdout, "%03d. %s\n", i+1, feature)
	}
}

func runFeatures(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("features", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write the feature inventory as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: boxr features [--json]")
		return 2
	}
	if *jsonOutput {
		return writeJSON(stdout, map[string]any{
			"schemaVersion": 1,
			"count":         len(implementedFeatures),
			"features":      implementedFeatures,
		}, stderr, "boxr features")
	}
	printFeatures(stdout)
	return 0
}

func mergeLabels(metadata, configured map[string]string) map[string]string {
	if len(metadata)+len(configured) == 0 {
		return nil
	}
	merged := make(map[string]string, len(metadata)+len(configured))
	for key, value := range metadata {
		merged[key] = value
	}
	for key, value := range configured {
		merged[key] = value
	}
	return merged
}

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "Usage: boxr completion bash|zsh|fish|powershell")
		return 2
	}
	commands := "daemon pull push create start run exec ps inspect stop kill wait logs network volume image stats rm events prune info system doctor support-bundle rootfs config deploy features version help completion"
	switch args[0] {
	case "bash":
		fmt.Fprintf(stdout, `_boxr_complete() { COMPREPLY=($(compgen -W "%s" -- "${COMP_WORDS[COMP_CWORD]}")); }
complete -F _boxr_complete boxr
`, commands)
	case "zsh":
		fmt.Fprintf(stdout, `#compdef boxr
_arguments '1:command:(%s)' '*::argument:->args'
`, strings.ReplaceAll(commands, " ", " "))
	case "fish":
		for _, command := range strings.Fields(commands) {
			fmt.Fprintf(stdout, "complete -c boxr -f -a %s\n", command)
		}
	case "powershell":
		fmt.Fprintf(stdout, `Register-ArgumentCompleter -CommandName boxr -ScriptBlock {
  param($wordToComplete)
  @(%s) | Where-Object { $_ -like "$wordToComplete*" }
}
`, strings.Join(strings.Fields(commands), ", "))
	default:
		fmt.Fprintf(stderr, "boxr completion: unsupported shell %q\n", args[0])
		return 2
	}
	return 0
}

func mergeEnvValues(groups ...[]string) []string {
	merged := make([]string, 0)
	for _, group := range groups {
		merged = container.MergeEnvironment(merged, group)
	}
	return merged
}

func readEnvFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > config.MaxConfigBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", config.MaxConfigBytes)
	}
	entries := make([]string, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if err := validateEnv(line); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		key, value, _ := strings.Cut(line, "=")
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		entries = append(entries, key+"="+value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: boxr config validate FILE.json | example | schema")
		return 2
	}
	switch args[0] {
	case "schema":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "Usage: boxr config schema")
			return 2
		}
		fmt.Fprintln(stdout, config.JSONSchema())
		return 0
	case "example":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "Usage: boxr config example")
			return 2
		}
		fmt.Fprint(stdout, config.Example())
		return 0
	case "validate":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: boxr config validate FILE.json")
			return 2
		}
		loaded, err := config.Load(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "boxr config: %v\n", err)
			return 1
		}
		command := "not specified (pass a command after --config)"
		if len(loaded.Command) > 0 {
			command = strings.Join(loaded.Command, " ")
		}
		fmt.Fprintf(stdout, "valid Boxr config v%d\nname: %s\ncommand: %s\nnetwork: %s\nrootfs: %s\n", loaded.Version, loaded.Name, command, loaded.Options.NetworkMode, loaded.Options.RootFS)
		return 0
	default:
		fmt.Fprintf(stderr, "boxr config: unknown action %q\n", args[0])
		return 2
	}
}

func runDeploy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: boxr deploy validate [--profile NAME] FILE.json|FILE.yaml|FILE.yml | plan [--profile NAME] [--json] FILE.json|FILE.yaml|FILE.yml | pull [--profile NAME] [--platform ...] [--authfile PATH] FILE.json|FILE.yaml|FILE.yml | up [--profile NAME] [--dry-run] FILE.json|FILE.yaml|FILE.yml | ps [--json] STACK | logs [--tail N] [--follow] STACK [SERVICE] | wait [--condition running|stopped] STACK | down [--time 10s] [--remove] STACK | profiles [--json] FILE.json|FILE.yaml|FILE.yml | example")
		return 2
	}
	switch args[0] {
	case "example":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "Usage: boxr deploy example")
			return 2
		}
		fmt.Fprint(stdout, deploy.Example())
		return 0
	case "profiles":
		flags := flag.NewFlagSet("deploy profiles", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write profile inventory as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr deploy profiles [--json] FILE.json|FILE.yaml|FILE.yml")
			return 2
		}
		file, err := deploy.Load(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy profiles: %v\n", err)
			return 1
		}
		inventory := deploy.ListProfiles(file)
		selected := make([]string, 0, len(inventory.Profiles))
		for _, profile := range inventory.Profiles {
			selected = append(selected, profile.Name)
		}
		if _, err := deploy.BuildPlanWithProfiles(file, selected); err != nil {
			fmt.Fprintf(stderr, "boxr deploy profiles: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, inventory, stderr, "boxr deploy profiles")
		}
		fmt.Fprintln(stdout, "DEFAULT SERVICES")
		if len(inventory.DefaultServices) == 0 {
			fmt.Fprintln(stdout, "(none)")
		} else {
			for _, service := range inventory.DefaultServices {
				fmt.Fprintln(stdout, service)
			}
		}
		fmt.Fprintln(stdout, "\nSERVICE PROFILES")
		if len(inventory.Profiles) == 0 {
			fmt.Fprintln(stdout, "No service profiles are defined.")
		} else {
			for _, profile := range inventory.Profiles {
				fmt.Fprintf(stdout, "%s\t%s\n", profile.Name, strings.Join(profile.Services, ", "))
			}
		}
		return 0
	case "validate", "plan":
		action := args[0]
		flags := flag.NewFlagSet("deploy "+action, flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write a secret-redacted JSON plan")
		profiles := []string{}
		flags.Func("profile", "activate a deployment service profile (repeatable)", func(value string) error {
			profiles = append(profiles, value)
			return nil
		})
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintf(stderr, "Usage: boxr deploy %s [--profile NAME] [--json] FILE.json|FILE.yaml|FILE.yml\n", action)
			return 2
		}
		file, err := deploy.Load(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy: %v\n", err)
			return 1
		}
		plan, err := deploy.BuildPlanWithProfiles(file, profiles)
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, plan, stderr, "boxr deploy")
		}
		if action == "validate" {
			fmt.Fprintf(stdout, "valid Boxr deployment v%d (%d services)\n", plan.Version, len(plan.Services))
			fmt.Fprintf(stdout, "execution order: %s\n", strings.Join(plan.ExecutionOrder, " -> "))
			fmt.Fprintf(stdout, "parallel start groups: %s\n", formatDeploymentStartGroups(plan.StartGroups))
			return 0
		}
		fmt.Fprintf(stdout, "Deployment: %s\nServices: %d\nExecution order: %s\n\n", plan.Name, len(plan.Services), strings.Join(plan.ExecutionOrder, " -> "))
		fmt.Fprintln(stdout, "SERVICE\tCONTAINER\tIMAGE\tDEPENDS ON\tREADINESS\tVOLUMES\tNETWORK\tSECURITY\tROOTFS")
		for _, service := range plan.Services {
			rootfsConfigured := "no"
			if service.RootFSConfigured {
				rootfsConfigured = "yes"
			}
			volumeSummary := make([]string, 0, len(service.Volumes))
			for _, mount := range service.Volumes {
				mode := "rw"
				if mount.ReadOnly {
					mode = "ro"
				}
				volumeSummary = append(volumeSummary, mount.Name+":"+mount.Target+":"+mode)
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				service.Name,
				service.ContainerName,
				service.Image,
				strings.Join(service.DependsOn, ","),
				service.ReadinessType,
				strings.Join(volumeSummary, ","),
				service.NetworkMode,
				service.SecurityProfile,
				rootfsConfigured,
			)
		}
		fmt.Fprintln(stdout, "\nPlan only: no container was created and no process was started.")
		return 0
	case "pull":
		flags := flag.NewFlagSet("deploy pull", flag.ContinueOnError)
		flags.SetOutput(stderr)
		profiles := []string{}
		flags.Func("profile", "activate a deployment service profile (repeatable)", func(value string) error {
			profiles = append(profiles, value)
			return nil
		})
		platform := flags.String("platform", "", "target image platform (default: linux/<host architecture>)")
		authFile := flags.String("authfile", "", "registry auth file (Docker config.json or Podman auth.json)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr deploy pull [--profile NAME] [--platform linux/ARCH[/VARIANT]] [--authfile PATH] FILE.json|FILE.yaml|FILE.yml")
			return 2
		}
		file, plan, code := loadDeploymentPlan(flags.Arg(0), profiles, stderr)
		if code != 0 {
			return code
		}
		return runDeploymentPull(file, plan, *platform, *authFile, stdout, stderr)
	case "up":
		flags := flag.NewFlagSet("deploy up", flag.ContinueOnError)
		flags.SetOutput(stderr)
		dryRun := flags.Bool("dry-run", false, "show the deployment plan without starting services")
		startTimeout := flags.Duration("start-timeout", 30*time.Second, "maximum time to wait for each service to enter running state")
		stopTimeout := flags.Duration("stop-timeout", 10*time.Second, "grace period for service shutdown")
		profiles := []string{}
		flags.Func("profile", "activate a deployment service profile (repeatable)", func(value string) error {
			profiles = append(profiles, value)
			return nil
		})
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 || *startTimeout <= 0 || *stopTimeout < 0 {
			fmt.Fprintln(stderr, "Usage: boxr deploy up [--profile NAME] [--dry-run] [--start-timeout 30s] [--stop-timeout 10s] FILE.json|FILE.yaml|FILE.yml")
			return 2
		}
		file, plan, code := loadDeploymentPlan(flags.Arg(0), profiles, stderr)
		if code != 0 {
			return code
		}
		if plan.Name == "" {
			fmt.Fprintln(stderr, "boxr deploy up: deployment manifest must define a name")
			return 2
		}
		if *dryRun {
			fmt.Fprintf(stdout, "Deployment %s: %d services\n", plan.Name, len(plan.Services))
			fmt.Fprintf(stdout, "Execution order: %s\n", strings.Join(plan.ExecutionOrder, " -> "))
			fmt.Fprintf(stdout, "Parallel start groups: %s\n", formatDeploymentStartGroups(plan.StartGroups))
			fmt.Fprintln(stdout, "Dry run: no image was unpacked and no service was started.")
			return 0
		}
		return runDeploymentUp(file, plan, *startTimeout, *stopTimeout, stdout, stderr)
	case "ps":
		flags := flag.NewFlagSet("deploy ps", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write service status as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
			fmt.Fprintln(stderr, "Usage: boxr deploy ps [--json] STACK")
			return 2
		}
		records, err := deploymentRecords(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy ps: %v\n", err)
			return 1
		}
		statuses := deploymentStatuses(records)
		if *jsonOutput {
			return writeJSON(stdout, statuses, stderr, "boxr deploy ps")
		}
		fmt.Fprintf(stdout, "Deployment: %s\n", flags.Arg(0))
		fmt.Fprintln(stdout, "SERVICE\tCONTAINER\tSTATUS\tPID\tEXIT")
		for _, item := range statuses {
			exit := "-"
			if item.ExitCode != nil {
				exit = strconv.Itoa(*item.ExitCode)
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\t%s\n", item.Service, item.ContainerName, item.Status, item.PID, exit)
		}
		if len(statuses) == 0 {
			fmt.Fprintln(stdout, "No services found.")
		}
		return 0
	case "logs":
		flags := flag.NewFlagSet("deploy logs", flag.ContinueOnError)
		flags.SetOutput(stderr)
		tail := flags.Int("tail", 100, "number of trailing lines per service (0 means all)")
		follow := flags.Bool("follow", false, "continue streaming output while services run")
		interval := flags.Duration("interval", 200*time.Millisecond, "follow polling interval (10ms to 1m)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if (flags.NArg() != 1 && flags.NArg() != 2) || *tail < 0 || *interval < 10*time.Millisecond || *interval > time.Minute {
			fmt.Fprintln(stderr, "Usage: boxr deploy logs [--tail 100] [--follow] [--interval 200ms] STACK [SERVICE]")
			return 2
		}
		stack := flags.Arg(0)
		service := ""
		if flags.NArg() == 2 {
			service = flags.Arg(1)
		}
		return runDeploymentLogs(stack, service, *tail, *follow, *interval, stdout, stderr)
	case "wait":
		flags := flag.NewFlagSet("deploy wait", flag.ContinueOnError)
		flags.SetOutput(stderr)
		condition := flags.String("condition", "running", "stack condition: running or stopped")
		timeout := flags.Duration("timeout", 30*time.Second, "maximum wait duration (0 means no timeout)")
		interval := flags.Duration("interval", 100*time.Millisecond, "poll interval (10ms to 1m)")
		jsonOutput := flags.Bool("json", false, "write stack status as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 || *timeout < 0 || *interval < 10*time.Millisecond || *interval > time.Minute || (*condition != "running" && *condition != "stopped") {
			fmt.Fprintln(stderr, "Usage: boxr deploy wait [--condition running|stopped] [--timeout 30s] [--interval 100ms] [--json] STACK")
			return 2
		}
		result, err := waitForDeploymentCondition(flags.Arg(0), *condition, *timeout, *interval)
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy wait: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, result, stderr, "boxr deploy wait")
		}
		fmt.Fprintf(stdout, "deployment %s reached %s state (%d service(s))\n", result.Stack, result.Condition, len(result.Services))
		return 0
	case "down":
		flags := flag.NewFlagSet("deploy down", flag.ContinueOnError)
		flags.SetOutput(stderr)
		timeout := flags.Duration("time", 10*time.Second, "grace period before SIGKILL")
		remove := flags.Bool("remove", false, "remove stopped container records and logs")
		dryRun := flags.Bool("dry-run", false, "show reverse-order shutdown without signaling services")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" || *timeout < 0 {
			fmt.Fprintln(stderr, "Usage: boxr deploy down [--time 10s] [--remove] [--dry-run] STACK")
			return 2
		}
		return runDeploymentDown(flags.Arg(0), *timeout, *remove, *dryRun, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "boxr deploy: unknown action %q\n", args[0])
		return 2
	}
}

func formatDeploymentStartGroups(groups [][]string) string {
	labels := make([]string, 0, len(groups))
	for _, group := range groups {
		labels = append(labels, strings.Join(group, ", "))
	}
	return strings.Join(labels, " | ")
}

func loadDeploymentPlan(path string, profiles []string, stderr io.Writer) (deploy.File, deploy.Plan, int) {
	file, err := deploy.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "boxr deploy: %v\n", err)
		return deploy.File{}, deploy.Plan{}, 1
	}
	plan, err := deploy.BuildPlanWithProfiles(file, profiles)
	if err != nil {
		fmt.Fprintf(stderr, "boxr deploy: %v\n", err)
		return deploy.File{}, deploy.Plan{}, 1
	}
	return file, plan, 0
}

type deploymentProcess struct {
	service     string
	name        string
	configDir   string
	stopTimeout time.Duration
	command     *exec.Cmd
}

type deploymentProcessExit struct {
	service string
	err     error
}

type deploymentHealthFailure struct {
	service string
	err     error
}

type deploymentStatus struct {
	Service       string     `json:"service"`
	ContainerName string     `json:"containerName"`
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	PID           int        `json:"pid,omitempty"`
	ExitCode      *int       `json:"exitCode,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

func deploymentServiceStartTimeout(service deploy.Service, fallback time.Duration) time.Duration {
	if service.StartTimeoutMS > 0 {
		return time.Duration(service.StartTimeoutMS) * time.Millisecond
	}
	return fallback
}

func deploymentContainerOptions(service deploy.Service) (container.Options, error) {
	options := service.Options
	if len(service.EnvFiles) > 0 {
		fileValues := make([]string, 0)
		for index, path := range service.EnvFiles {
			entries, err := readEnvFile(path)
			if err != nil {
				return container.Options{}, fmt.Errorf("environment file %d could not be read: %w", index+1, err)
			}
			fileValues = append(fileValues, entries...)
		}
		// Manifest options are explicit overrides and take precedence over
		// values loaded from the service's environment files.
		options.Env = mergeEnvValues(fileValues, options.Env)
	}
	for index, mount := range service.Volumes {
		managed, err := volume.Get(mount.Name)
		if err != nil {
			return container.Options{}, fmt.Errorf("named volume %q for mount %d: %w", mount.Name, index+1, err)
		}
		options.BindMounts = append(options.BindMounts, container.BindMount{
			Source:   managed.Path,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		})
	}
	validationOptions := options
	if service.Image != "" && validationOptions.RootFS == "" {
		// Image-backed deployments unpack their rootfs just before boxr run
		// parses these bind mounts. Validate the deferred rootfs requirement here
		// without persisting a fake host path in the run configuration.
		validationOptions.RootFS = "/boxr-image-rootfs-validation"
	}
	if err := validationOptions.Validate(); err != nil {
		return container.Options{}, fmt.Errorf("invalid prepared container options: %w", err)
	}
	return options, nil
}

func deferImageServiceBindMounts(image string, options *container.Options) ([]string, error) {
	if image == "" || len(options.BindMounts) == 0 {
		return nil, nil
	}
	var args []string
	for index, mount := range options.BindMounts {
		if strings.ContainsAny(mount.Source+mount.Target, ":\\\x00") {
			return nil, fmt.Errorf("mount %d paths cannot contain colons, backslashes, or NUL bytes", index+1)
		}
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		args = append(args, "--mount", mount.Source+":"+mount.Target+":"+mode)
	}
	// config.Load validates options before --image unpacks its rootfs. Pass
	// mounts as run flags so the CLI applies them after image extraction.
	options.BindMounts = nil
	return args, nil
}

func startDeploymentProcess(executable, stack string, order int, serviceName string, service deploy.Service, stdout, stderr io.Writer) (*deploymentProcess, error) {
	options, err := deploymentContainerOptions(service)
	if err != nil {
		return nil, fmt.Errorf("prepare service %q configuration: %w", serviceName, err)
	}
	options.Name = deploy.ContainerName(stack, serviceName)
	options.AutoRemove = false
	internalLabels := map[string]string{
		deploy.StackLabel:   stack,
		deploy.ServiceLabel: serviceName,
		deploy.OrderLabel:   strconv.Itoa(order),
	}
	if service.StopTimeoutMS > 0 {
		internalLabels[deploy.StopTimeoutLabel] = strconv.Itoa(service.StopTimeoutMS)
	}
	options.Labels = mergeLabels(options.Labels, internalLabels)
	deferredMountArgs, err := deferImageServiceBindMounts(service.Image, &options)
	if err != nil {
		return nil, fmt.Errorf("prepare image service mounts: %w", err)
	}
	runFile := config.RunFile{
		Version: 1,
		Name:    options.Name,
		Command: append([]string(nil), service.Command...),
		Options: options,
	}
	data, err := json.Marshal(runFile)
	if err != nil {
		return nil, fmt.Errorf("encode service %q run config: %w", serviceName, err)
	}
	configDir, err := os.MkdirTemp("", "boxr-deploy-*")
	if err != nil {
		return nil, fmt.Errorf("create private service config directory: %w", err)
	}
	configPath := filepath.Join(configDir, "run.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		_ = os.RemoveAll(configDir)
		return nil, fmt.Errorf("write private service config: %w", err)
	}
	args := []string{"run", "--config", configPath}
	if service.Image != "" {
		args = append(args, "--image", service.Image)
	}
	args = append(args, deferredMountArgs...)
	command := exec.Command(executable, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(configDir)
		return nil, fmt.Errorf("start service %q: %w", serviceName, err)
	}
	return &deploymentProcess{
		service:     serviceName,
		name:        options.Name,
		configDir:   configDir,
		stopTimeout: time.Duration(service.StopTimeoutMS) * time.Millisecond,
		command:     command,
	}, nil
}

func waitForDeploymentStart(ctx context.Context, name string, timeout time.Duration, probe *deploy.ReadinessProbe, exits <-chan deploymentProcessExit) (state.Record, *deploymentProcessExit, error) {
	var empty state.Record
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case exited := <-exits:
			return empty, &exited, fmt.Errorf("service %q exited while another service was starting", exited.service)
		case <-startCtx.Done():
			if ctx.Err() != nil {
				return empty, nil, ctx.Err()
			}
			if probe != nil {
				return empty, nil, fmt.Errorf("timed out waiting for service %q readiness", name)
			}
			return empty, nil, fmt.Errorf("timed out waiting for service %q to start", name)
		case <-ticker.C:
			record, err := state.Get(name)
			if err != nil {
				continue
			}
			switch record.Status {
			case "running":
				unexpectedExit, err := waitForDeploymentReadiness(startCtx, name, probe, exits)
				if unexpectedExit != nil {
					return empty, unexpectedExit, err
				}
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						return empty, nil, fmt.Errorf("timed out waiting for service %q readiness: %w", name, err)
					}
					return empty, nil, fmt.Errorf("service %q readiness failed: %w", name, err)
				}
				return record, nil, nil
			case "exited", "failed":
				return record, nil, fmt.Errorf("service %q exited before entering the running state", name)
			}
		}
	}
}

// waitForDeploymentReadiness races the probe against supervisor cancellation
// and service exits so a failed sibling cannot remain hidden behind a slow probe.
func deploymentReadinessForProcess(file deploy.File, process *deploymentProcess) (*deploy.ReadinessProbe, error) {
	if process == nil {
		return nil, fmt.Errorf("deployment process is required")
	}
	service, exists := file.Services[process.service]
	if !exists {
		return nil, fmt.Errorf("deployment service %q does not exist", process.service)
	}
	return service.Readiness, nil
}

func waitForDeploymentReadiness(ctx context.Context, name string, probe *deploy.ReadinessProbe, exits <-chan deploymentProcessExit) (*deploymentProcessExit, error) {
	if probe == nil {
		return nil, nil
	}
	result := make(chan error, 1)
	go func() {
		result <- deploy.WaitForReadiness(ctx, probe)
	}()
	for {
		select {
		case err := <-result:
			return nil, err
		case exited := <-exits:
			return &exited, fmt.Errorf("service %q exited while waiting for service %q readiness", exited.service, name)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func deploymentTargetPlatform(platform string) (string, string, string, error) {
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}
	if strings.ContainsAny(platform, "\\\x00\r\n ") {
		return "", "", "", fmt.Errorf("invalid target platform %q", platform)
	}
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
		return "", "", "", fmt.Errorf("platform must be linux/ARCH[/VARIANT]")
	}
	variant := ""
	if len(parts) == 3 {
		variant = parts[2]
	}
	return parts[0], parts[1], variant, nil
}

func resolveDeploymentImageReference(reference, platform string) (string, error) {
	targetOS, targetArch, targetVariant, err := deploymentTargetPlatform(platform)
	if err != nil {
		return "", err
	}
	verifyPlatform := func(image imageStore.Image) error {
		if image.OS != targetOS || image.Arch != targetArch {
			return fmt.Errorf("local image platform is %s/%s; requested %s/%s", image.OS, image.Arch, targetOS, targetArch)
		}
		if targetVariant != "" && image.Variant != targetVariant {
			return fmt.Errorf("local image variant is %q; requested %q", image.Variant, targetVariant)
		}
		return nil
	}
	if image, err := imageStore.Verify(reference); err == nil {
		if err := verifyPlatform(image); err != nil {
			return "", err
		}
		return reference, nil
	}
	parsed, err := registry.ParseReference(reference)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}
	localReference := parsed.LocalReference()
	image, err := imageStore.Verify(localReference)
	if err != nil {
		return "", fmt.Errorf("image is not present in the local store: %w", err)
	}
	if err := verifyPlatform(image); err != nil {
		return "", err
	}
	return localReference, nil
}

func runDeploymentPull(file deploy.File, plan deploy.Plan, platform, authFile string, stdout, stderr io.Writer) int {
	seen := make(map[string]string, len(plan.Services))
	pulled := 0
	for _, service := range plan.Services {
		if service.Image == "" {
			continue
		}
		localReference, resolveErr := resolveDeploymentImageReference(service.Image, platform)
		deduplicationKey := ""
		if resolveErr == nil {
			deduplicationKey = localReference
		} else {
			parsed, err := registry.ParseReference(service.Image)
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy pull: service %s has an invalid image reference: %v\n", service.Name, err)
				return 1
			}
			deduplicationKey = parsed.LocalReference()
		}
		if previous, exists := seen[deduplicationKey]; exists {
			fmt.Fprintf(stdout, "service %s reuses image %s (already handled for %s)\n", service.Name, service.Image, previous)
			continue
		}
		seen[deduplicationKey] = service.Name
		if resolveErr == nil {
			fmt.Fprintf(stdout, "image %s already present locally as %s\n", service.Image, localReference)
			continue
		}

		fmt.Fprintf(stdout, "pulling image %s for service %s\n", service.Image, service.Name)
		pullArgs := []string{"pull"}
		if platform != "" {
			pullArgs = append(pullArgs, "--platform", platform)
		}
		if authFile != "" {
			pullArgs = append(pullArgs, "--authfile", authFile)
		}
		pullArgs = append(pullArgs, service.Image)
		if code := runImage(pullArgs, stdout, stderr); code != 0 {
			fmt.Fprintf(stderr, "boxr deploy pull: image pull failed for service %s\n", service.Name)
			return code
		}
		localReference, err := resolveDeploymentImageReference(service.Image, platform)
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy pull: downloaded image for service %s could not be verified locally: %v\n", service.Name, err)
			return 1
		}
		fmt.Fprintf(stdout, "verified image %s as %s\n", service.Image, localReference)
		pulled++
	}
	fmt.Fprintf(stdout, "deployment %s: all %d image reference(s) are available locally (%d pulled)\n", file.Name, len(seen), pulled)
	return 0
}

func runDeploymentUp(file deploy.File, plan deploy.Plan, startTimeout, stopTimeout time.Duration, stdout, stderr io.Writer) int {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(stderr, "boxr deploy up: service execution requires Linux")
		return 2
	}
	if file.Name == "" {
		fmt.Fprintln(stderr, "boxr deploy up: deployment name is required")
		return 2
	}
	existing, err := state.List(true)
	if err != nil {
		fmt.Fprintf(stderr, "boxr deploy up: inspect existing containers: %v\n", err)
		return 1
	}
	existingNames := make(map[string]bool, len(existing))
	for _, record := range existing {
		existingNames[record.Name] = true
	}
	resolvedImages := make(map[string]string, len(plan.Services))
	for _, service := range plan.Services {
		if existingNames[service.ContainerName] {
			fmt.Fprintf(stderr, "boxr deploy up: container %q already exists; use deploy down --remove %s before retrying\n", service.ContainerName, file.Name)
			return 1
		}
		if service.Image != "" {
			localReference, err := resolveDeploymentImageReference(service.Image, "")
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy up: image %q for service %q is not ready: %v (run boxr deploy pull first)\n", service.Image, service.Name, err)
				return 1
			}
			resolvedImages[service.Name] = localReference
		}
		if _, err := deploymentContainerOptions(file.Services[service.Name]); err != nil {
			fmt.Fprintf(stderr, "boxr deploy up: service %s configuration preflight failed: %v\n", service.Name, err)
			return 1
		}
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "boxr deploy up: locate Boxr executable: %v\n", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exits := make(chan deploymentProcessExit, len(plan.Services))
	processes := make([]*deploymentProcess, 0, len(plan.Services))
	exited := make(map[string]deploymentProcessExit, len(plan.Services))
	defer func() {
		for _, process := range processes {
			_ = os.RemoveAll(process.configDir)
		}
	}()

	plannedByName := make(map[string]deploy.ServicePlan, len(plan.Services))
	orderIndex := make(map[string]int, len(plan.Services))
	for index, planned := range plan.Services {
		plannedByName[planned.Name] = planned
		orderIndex[planned.Name] = index
	}
	startGroups := plan.StartGroups
	if len(startGroups) == 0 {
		startGroups = [][]string{plan.ExecutionOrder}
	}
	for _, group := range startGroups {
		groupProcesses := make([]*deploymentProcess, 0, len(group))
		for _, serviceName := range group {
			if _, exists := plannedByName[serviceName]; !exists {
				fmt.Fprintf(stderr, "boxr deploy up: start plan references unknown service %q\n", serviceName)
				stopDeploymentProcesses(processes, exited, exits, stopTimeout, stdout, stderr)
				return 1
			}
			service := file.Services[serviceName]
			if resolvedImage, exists := resolvedImages[serviceName]; exists {
				service.Image = resolvedImage
			}
			process, err := startDeploymentProcess(executable, file.Name, orderIndex[serviceName], serviceName, service, stdout, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy up: %v\n", err)
				stopDeploymentProcesses(processes, exited, exits, stopTimeout, stdout, stderr)
				return 1
			}
			processes = append(processes, process)
			groupProcesses = append(groupProcesses, process)
			go func(serviceName string, command *exec.Cmd) {
				exits <- deploymentProcessExit{service: serviceName, err: command.Wait()}
			}(process.service, process.command)
			fmt.Fprintf(stdout, "starting service %s (%s)\n", process.service, process.name)
		}
		// Launch all services in a dependency wave before waiting for any one.
		for _, process := range groupProcesses {
			probe, probeErr := deploymentReadinessForProcess(file, process)
			if probeErr != nil {
				fmt.Fprintf(stderr, "boxr deploy up: %v\n", probeErr)
				stopDeploymentProcesses(processes, exited, exits, stopTimeout, stdout, stderr)
				return 1
			}
			service := file.Services[process.service]
			serviceTimeout := deploymentServiceStartTimeout(service, startTimeout)
			_, unexpectedExit, err := waitForDeploymentStart(ctx, process.name, serviceTimeout, probe, exits)
			if unexpectedExit != nil {
				exited[unexpectedExit.service] = *unexpectedExit
			}
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy up: %v\n", err)
				stopDeploymentProcesses(processes, exited, exits, stopTimeout, stdout, stderr)
				if errors.Is(err, context.Canceled) {
					return 130
				}
				return 1
			}
			fmt.Fprintf(stdout, "started service %s\n", process.service)
		}
	}
	healthCtx, cancelHealth := context.WithCancel(ctx)
	defer cancelHealth()
	healthFailures := make(chan deploymentHealthFailure, len(processes))
	for _, process := range processes {
		healthcheck := file.Services[process.service].Healthcheck
		if healthcheck == nil {
			continue
		}
		go func(service string, health *deploy.Healthcheck) {
			if err := deploy.MonitorHealth(healthCtx, health); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case healthFailures <- deploymentHealthFailure{service: service, err: err}:
				case <-healthCtx.Done():
				}
			}
		}(process.service, healthcheck)
	}

	fmt.Fprintf(stdout, "deployment %s is running; press Ctrl+C to stop all services\n", file.Name)
	var terminalExit *deploymentProcessExit
	var terminalHealthFailure *deploymentHealthFailure
	interrupted := false
	select {
	case exitedService := <-exits:
		exited[exitedService.service] = exitedService
		terminalExit = &exitedService
		if exitedService.err != nil {
			fmt.Fprintf(stderr, "boxr deploy up: service %s exited: %v\n", exitedService.service, exitedService.err)
		} else {
			fmt.Fprintf(stdout, "boxr deploy up: service %s completed\n", exitedService.service)
		}
	case failure := <-healthFailures:
		terminalHealthFailure = &failure
		fmt.Fprintf(stderr, "boxr deploy up: service %s is unhealthy: %v\n", failure.service, failure.err)
	case <-ctx.Done():
		interrupted = true
		fmt.Fprintln(stdout, "stopping deployment after interrupt")
	}
	cancelHealth()
	stopDeploymentProcesses(processes, exited, exits, stopTimeout, stdout, stderr)
	if terminalExit != nil {
		return deploymentExitCode(terminalExit.err)
	}
	if terminalHealthFailure != nil {
		return 1
	}
	if interrupted {
		return 130
	}
	return 0
}

func stopDeploymentProcesses(processes []*deploymentProcess, exited map[string]deploymentProcessExit, exits <-chan deploymentProcessExit, timeout time.Duration, stdout, stderr io.Writer) {
	type stopState struct {
		graceDeadline time.Time
		killDeadline  time.Time
		killed        bool
	}
	pending := make(map[string]stopState, len(processes))
	now := time.Now()
	for index := len(processes) - 1; index >= 0; index-- {
		process := processes[index]
		if _, finished := exited[process.service]; finished || process.command.Process == nil {
			continue
		}
		grace := timeout
		if process.stopTimeout > 0 {
			grace = process.stopTimeout
		}
		pending[process.service] = stopState{graceDeadline: now.Add(grace)}
		if err := process.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintf(stderr, "boxr deploy: signal service %s: %v\n", process.service, err)
		}
	}

	for len(pending) > 0 {
		now = time.Now()
		for index := len(processes) - 1; index >= 0; index-- {
			process := processes[index]
			state, exists := pending[process.service]
			if !exists {
				continue
			}
			if !state.killed && !now.Before(state.graceDeadline) {
				if process.command.Process != nil {
					if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
						fmt.Fprintf(stderr, "boxr deploy: kill service %s: %v\n", process.service, err)
					}
				}
				state.killed = true
				state.killDeadline = now.Add(2 * time.Second)
				pending[process.service] = state
				continue
			}
			if state.killed && !now.Before(state.killDeadline) {
				fmt.Fprintf(stderr, "boxr deploy: service %s did not exit after SIGKILL\n", process.service)
				delete(pending, process.service)
				continue
			}
		}
		if len(pending) == 0 {
			return
		}

		var nextDeadline time.Time
		for _, state := range pending {
			deadline := state.graceDeadline
			if state.killed {
				deadline = state.killDeadline
			}
			if nextDeadline.IsZero() || deadline.Before(nextDeadline) {
				nextDeadline = deadline
			}
		}
		delay := time.Until(nextDeadline)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case exitedService := <-exits:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if _, seen := exited[exitedService.service]; !seen {
				exited[exitedService.service] = exitedService
				delete(pending, exitedService.service)
				fmt.Fprintf(stdout, "stopped service %s\n", exitedService.service)
			}
		case <-timer.C:
		}
	}
}

func deploymentStopTimeout(labels map[string]string, fallback time.Duration) (time.Duration, error) {
	value := labels[deploy.StopTimeoutLabel]
	if value == "" {
		return fallback, nil
	}
	milliseconds, err := strconv.Atoi(value)
	if err != nil || milliseconds < 10 || milliseconds > 600000 {
		return 0, fmt.Errorf("invalid persisted service stop timeout")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func deploymentExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	return 1
}

func deploymentRecords(stack string) ([]state.Record, error) {
	records, err := state.List(true)
	if err != nil {
		return nil, err
	}
	filtered := make([]state.Record, 0)
	for _, record := range records {
		if record.Labels[deploy.StackLabel] == stack {
			filtered = append(filtered, record)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		left, _ := strconv.Atoi(filtered[i].Labels[deploy.OrderLabel])
		right, _ := strconv.Atoi(filtered[j].Labels[deploy.OrderLabel])
		if left != right {
			return left < right
		}
		return filtered[i].Labels[deploy.ServiceLabel] < filtered[j].Labels[deploy.ServiceLabel]
	})
	return filtered, nil
}

func deploymentStatuses(records []state.Record) []deploymentStatus {
	statuses := make([]deploymentStatus, 0, len(records))
	for _, record := range records {
		statuses = append(statuses, deploymentStatus{
			Service:       record.Labels[deploy.ServiceLabel],
			ContainerName: record.Name,
			ID:            record.ID,
			Status:        record.Status,
			PID:           record.PID,
			ExitCode:      record.ExitCode,
			CreatedAt:     record.CreatedAt,
			StartedAt:     record.StartedAt,
			FinishedAt:    record.FinishedAt,
		})
	}
	return statuses
}

type deploymentLogPrefixWriter struct {
	service string
	dst     io.Writer
	pending []byte
}

func (w *deploymentLogPrefixWriter) Write(data []byte) (int, error) {
	original := len(data)
	w.pending = append(w.pending, data...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		line := w.pending[:index+1]
		if _, err := fmt.Fprintf(w.dst, "[%s] ", w.service); err != nil {
			return 0, err
		}
		written, err := w.dst.Write(line)
		if err != nil {
			return 0, err
		}
		if written != len(line) {
			return 0, io.ErrShortWrite
		}
		w.pending = w.pending[index+1:]
	}
	return original, nil
}

func (w *deploymentLogPrefixWriter) Flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w.dst, "[%s] ", w.service); err != nil {
		return err
	}
	written, err := w.dst.Write(w.pending)
	if err != nil {
		return err
	}
	if written != len(w.pending) {
		return io.ErrShortWrite
	}
	w.pending = nil
	return nil
}

func runDeploymentLogs(stack, service string, tail int, follow bool, interval time.Duration, stdout, stderr io.Writer) int {
	if tail < 0 || interval < 10*time.Millisecond || interval > time.Minute {
		fmt.Fprintln(stderr, "boxr deploy logs: invalid tail or interval")
		return 2
	}
	records, err := deploymentRecords(stack)
	if err != nil {
		fmt.Fprintf(stderr, "boxr deploy logs: %v\n", err)
		return 1
	}
	if service != "" {
		filtered := make([]state.Record, 0, 1)
		for _, record := range records {
			if record.Labels[deploy.ServiceLabel] == service {
				filtered = append(filtered, record)
			}
		}
		records = filtered
	}
	if len(records) == 0 {
		if service != "" {
			fmt.Fprintf(stderr, "boxr deploy logs: no service %q found in deployment %q\n", service, stack)
		} else {
			fmt.Fprintf(stderr, "boxr deploy logs: deployment %q has no recorded services\n", stack)
		}
		return 1
	}
	writers := make(map[string]*deploymentLogPrefixWriter, len(records))
	offsets := make(map[string]int64, len(records))
	completed := make(map[string]bool, len(records))
	for _, record := range records {
		name := record.Labels[deploy.ServiceLabel]
		writer := &deploymentLogPrefixWriter{service: name, dst: stdout}
		writers[record.ID] = writer
		data, offset, err := state.ReadLogsSnapshot(record.ID, tail)
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy logs: read service %s: %v\n", name, err)
			return 1
		}
		if _, err := writer.Write(data); err != nil {
			fmt.Fprintf(stderr, "boxr deploy logs: write service %s: %v\n", name, err)
			return 1
		}
		offsets[record.ID] = offset
		if !follow || (record.Status != "running" && record.Status != "created") {
			if err := writer.Flush(); err != nil {
				fmt.Fprintf(stderr, "boxr deploy logs: flush service %s: %v\n", name, err)
				return 1
			}
			completed[record.ID] = true
		}
	}
	if !follow {
		return 0
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	flushPending := func() error {
		for _, writer := range writers {
			if err := writer.Flush(); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		active := false
		for _, record := range records {
			if completed[record.ID] {
				continue
			}
			current, err := state.Get(record.ID)
			if err != nil {
				if flushErr := writers[record.ID].Flush(); flushErr != nil {
					fmt.Fprintf(stderr, "boxr deploy logs: flush service %s: %v\n", record.Labels[deploy.ServiceLabel], flushErr)
					return 1
				}
				completed[record.ID] = true
				continue
			}
			var chunk bytes.Buffer
			next, err := state.CopyLogFrom(record.ID, offsets[record.ID], &chunk)
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy logs: follow service %s: %v\n", record.Labels[deploy.ServiceLabel], err)
				return 1
			}
			offsets[record.ID] = next
			if _, err := writers[record.ID].Write(chunk.Bytes()); err != nil {
				fmt.Fprintf(stderr, "boxr deploy logs: write service %s: %v\n", record.Labels[deploy.ServiceLabel], err)
				return 1
			}
			if current.Status == "running" || current.Status == "created" {
				active = true
				continue
			}
			if err := writers[record.ID].Flush(); err != nil {
				fmt.Fprintf(stderr, "boxr deploy logs: flush service %s: %v\n", record.Labels[deploy.ServiceLabel], err)
				return 1
			}
			completed[record.ID] = true
		}
		if !active {
			return 0
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if err := flushPending(); err != nil {
				fmt.Fprintf(stderr, "boxr deploy logs: flush remaining output: %v\n", err)
				return 1
			}
			return 130
		case <-timer.C:
		}
	}
}

type deploymentWaitResult struct {
	Stack     string             `json:"stack"`
	Condition string             `json:"condition"`
	WaitedMS  int64              `json:"waitedMs"`
	Services  []deploymentStatus `json:"services"`
}

func deploymentWaitCondition(records []state.Record, condition string) (bool, error) {
	if condition != "running" && condition != "stopped" {
		return false, fmt.Errorf("unsupported deployment condition %q", condition)
	}
	if len(records) == 0 {
		if condition == "stopped" {
			return true, nil
		}
		return false, fmt.Errorf("deployment has no recorded services")
	}
	switch condition {
	case "running":
		for _, record := range records {
			if record.Status == "exited" || record.Status == "failed" {
				return false, fmt.Errorf("service %s reached %s before the deployment became running", record.Labels[deploy.ServiceLabel], record.Status)
			}
			if record.Status != "running" {
				return false, nil
			}
		}
		return true, nil
	case "stopped":
		for _, record := range records {
			if record.Status == "running" {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported deployment condition %q", condition)
	}
}

func waitForDeploymentCondition(stack, condition string, timeout, interval time.Duration) (deploymentWaitResult, error) {
	result := deploymentWaitResult{Stack: stack, Condition: condition}
	if timeout < 0 {
		return result, fmt.Errorf("timeout cannot be negative")
	}
	if interval < 10*time.Millisecond || interval > time.Minute {
		return result, fmt.Errorf("poll interval must be between 10ms and 1m")
	}
	started := time.Now()
	for {
		records, err := deploymentRecords(stack)
		if err != nil {
			return result, err
		}
		satisfied, err := deploymentWaitCondition(records, condition)
		if err != nil {
			return result, err
		}
		if satisfied {
			result.WaitedMS = time.Since(started).Milliseconds()
			result.Services = deploymentStatuses(records)
			if result.Services == nil {
				result.Services = []deploymentStatus{}
			}
			return result, nil
		}
		if timeout > 0 {
			remaining := timeout - time.Since(started)
			if remaining <= 0 {
				return result, fmt.Errorf("timed out waiting for deployment %s to reach %s", stack, condition)
			}
			if remaining < interval {
				time.Sleep(remaining)
				continue
			}
		}
		time.Sleep(interval)
	}
}

func runDeploymentDown(stack string, timeout time.Duration, remove, dryRun bool, stdout, stderr io.Writer) int {
	records, err := deploymentRecords(stack)
	if err != nil {
		fmt.Fprintf(stderr, "boxr deploy down: %v\n", err)
		return 1
	}
	if len(records) == 0 {
		fmt.Fprintf(stdout, "deployment %s has no recorded services\n", stack)
		return 0
	}
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		service := record.Labels[deploy.ServiceLabel]
		serviceTimeout, err := deploymentStopTimeout(record.Labels, timeout)
		if err != nil {
			fmt.Fprintf(stderr, "boxr deploy down: service %s: %v\n", service, err)
			return 1
		}
		if dryRun {
			action := "stop"
			if remove {
				action = "stop and remove"
			}
			fmt.Fprintf(stdout, "would %s service %s (%s) with %s grace\n", action, service, record.Name, serviceTimeout)
			continue
		}
		if record.Status == "running" {
			if runtime.GOOS != "linux" {
				fmt.Fprintln(stderr, "boxr deploy down: stopping services requires Linux")
				return 2
			}
			if err := state.Signal(record.ID, "TERM"); err != nil {
				fmt.Fprintf(stderr, "boxr deploy down: stop service %s: %v\n", service, err)
				return 1
			}
			if !waitForStopped(record.ID, record.PID, time.Now().Add(serviceTimeout)) {
				if err := state.Signal(record.ID, "KILL"); err != nil {
					fmt.Fprintf(stderr, "boxr deploy down: force-stop service %s: %v\n", service, err)
					return 1
				}
				if !waitForStopped(record.ID, record.PID, time.Now().Add(3*time.Second)) {
					fmt.Fprintf(stderr, "boxr deploy down: service %s did not stop\n", service)
					return 1
				}
			}
		}
		if remove {
			current, err := state.Get(record.ID)
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy down: refresh service %s: %v\n", service, err)
				return 1
			}
			if current.Status == "created" {
				err = state.RemoveCreated(current.ID)
			} else {
				err = state.Remove(current.ID)
			}
			if err != nil {
				fmt.Fprintf(stderr, "boxr deploy down: remove service %s: %v\n", service, err)
				return 1
			}
		} else {
			fmt.Fprintf(stdout, "stopped service %s (%s)\n", service, record.Name)
		}
	}
	if remove {
		fmt.Fprintf(stdout, "removed deployment %s\n", stack)
	}
	return 0
}

func runStop(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	timeout := flags.Duration("time", 10*time.Second, "grace period before SIGKILL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() == 0 || *timeout < 0 {
		fmt.Fprintln(stderr, "Usage: boxr stop [--time 10s] ID|NAME [...]")
		return 2
	}
	failed := false
	for _, query := range flags.Args() {
		record, err := state.Get(query)
		if err != nil {
			fmt.Fprintf(stderr, "boxr stop: %s: %v\n", query, err)
			failed = true
			continue
		}
		if record.Status == "restarting" {
			if err := state.RequestStop(record.ID); err != nil {
				fmt.Fprintf(stderr, "boxr stop: %s: %v\n", record.ID, err)
				failed = true
				continue
			}
			fmt.Fprintf(stdout, "cancelled pending restart for %s\n", record.ID)
			continue
		}
		if record.Status != "running" {
			fmt.Fprintf(stdout, "%s is already %s\n", record.ID, record.Status)
			continue
		}
		if err := state.RequestStop(record.ID); err != nil {
			fmt.Fprintf(stderr, "boxr stop: %s: %v\n", record.ID, err)
			failed = true
			continue
		}
		if err := state.Signal(record.ID, "TERM"); err != nil {
			_ = state.CancelStopRequest(record.ID)
			fmt.Fprintf(stderr, "boxr stop: %s: %v\n", record.ID, err)
			failed = true
			continue
		}
		deadline := time.Now().Add(*timeout)
		stopped := waitForStopped(record.ID, record.PID, deadline)
		if !stopped {
			if err := state.Signal(record.ID, "KILL"); err != nil {
				fmt.Fprintf(stderr, "boxr stop: %s: timed out and SIGKILL failed: %v\n", record.ID, err)
				failed = true
				continue
			}
			stopped = waitForStopped(record.ID, record.PID, time.Now().Add(3*time.Second))
		}
		if !stopped {
			fmt.Fprintf(stderr, "boxr stop: %s: process did not stop after SIGKILL; check state with boxr inspect\n", record.ID)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "stopped %s\n", record.ID)
	}
	if failed {
		return 1
	}
	return 0
}

func waitForStopped(query string, pid int, deadline time.Time) bool {
	for {
		record, err := state.Get(query)
		if err != nil || record.Status != "running" {
			return true
		}
		if !state.ProcessAlive(pid) {
			// The foreground owner records the final status after Wait returns.
			// Give it a short interval to persist the exit event.
			if time.Now().After(deadline) {
				return false
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func runKill(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("kill", flag.ContinueOnError)
	flags.SetOutput(stderr)
	signalName := flags.String("signal", "KILL", "signal name: KILL, TERM, INT, HUP, QUIT, USR1, USR2, STOP, or CONT")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() == 0 {
		fmt.Fprintln(stderr, "Usage: boxr kill [--signal KILL] ID|NAME [...]")
		return 2
	}
	failed := false
	for _, query := range flags.Args() {
		record, err := state.Get(query)
		terminating := isTerminatingSignal(*signalName)
		stopRequested := false
		if err == nil && record.Status == "restarting" && terminating {
			err = state.RequestStop(record.ID)
			if err == nil {
				fmt.Fprintf(stdout, "cancelled pending restart for %s\n", record.ID)
				continue
			}
		}
		if err == nil && record.Status != "running" {
			err = fmt.Errorf("container is %s, not running", record.Status)
		}
		if err == nil && terminating {
			err = state.RequestStop(record.ID)
			stopRequested = err == nil
		}
		if err == nil {
			err = state.Signal(record.ID, *signalName)
		}
		if err != nil {
			if stopRequested {
				_ = state.CancelStopRequest(record.ID)
			}
			fmt.Fprintf(stderr, "boxr kill: %s: %v\n", query, err)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "sent %s to %s\n", strings.ToUpper(*signalName), record.ID)
	}
	if failed {
		return 1
	}
	return 0
}

type waitResult struct {
	ID        string `json:"id"`
	Condition string `json:"condition"`
	Status    string `json:"status"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	WaitedMS  int64  `json:"waitedMs"`
}

func isTerminatingSignal(value string) bool {
	switch strings.ToUpper(strings.TrimPrefix(value, "SIG")) {
	case "HUP", "INT", "QUIT", "TERM", "KILL":
		return true
	default:
		return false
	}
}

func runWait(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(stderr)
	timeout := flags.Duration("timeout", 0, "maximum wait duration (0 means no timeout)")
	condition := flags.String("condition", "stopped", "condition to wait for: running or stopped")
	interval := flags.Duration("interval", 100*time.Millisecond, "poll interval (minimum 10ms)")
	jsonOutput := flags.Bool("json", false, "write wait results as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() == 0 || *timeout < 0 || *interval < 10*time.Millisecond || (*condition != "running" && *condition != "stopped") {
		fmt.Fprintln(stderr, "Usage: boxr wait [--condition running|stopped] [--timeout 30s] [--interval 100ms] [--json] ID|NAME [...]")
		return 2
	}

	results := make([]waitResult, 0, flags.NArg())
	failed := false
	for _, query := range flags.Args() {
		result, err := waitForCondition(query, *condition, *timeout, *interval)
		if err != nil {
			fmt.Fprintf(stderr, "boxr wait: %s: %v\n", query, err)
			failed = true
			continue
		}
		results = append(results, result)
		if !*jsonOutput {
			if *condition == "stopped" && result.ExitCode != nil {
				fmt.Fprintf(stdout, "%d\n", *result.ExitCode)
			} else {
				fmt.Fprintln(stdout, result.ID)
			}
		}
	}
	if *jsonOutput {
		if code := writeJSON(stdout, results, stderr, "boxr wait"); code != 0 {
			return code
		}
	}
	if failed {
		return 1
	}
	return 0
}

// waitForCondition polls a persisted container record until it reaches the
// requested lifecycle state, times out, or terminates before becoming ready.
func waitForCondition(query, condition string, timeout, interval time.Duration) (waitResult, error) {
	var result waitResult
	started := time.Now()
	record, err := state.Get(query)
	if err != nil {
		return result, err
	}
	for {
		switch condition {
		case "running":
			if record.Status == "running" {
				return waitResult{ID: record.ID, Condition: condition, Status: record.Status, ExitCode: record.ExitCode, WaitedMS: time.Since(started).Milliseconds()}, nil
			}
			if record.Status == "exited" || record.Status == "failed" {
				return result, fmt.Errorf("container reached %s before becoming running", record.Status)
			}
		case "stopped":
			if record.Status == "exited" || record.Status == "failed" {
				return waitResult{ID: record.ID, Condition: condition, Status: record.Status, ExitCode: record.ExitCode, WaitedMS: time.Since(started).Milliseconds()}, nil
			}
			if record.Status == "created" {
				return result, fmt.Errorf("container %s has not started", record.ID)
			}
		default:
			return result, fmt.Errorf("unsupported condition %q", condition)
		}
		if timeout > 0 && time.Since(started) >= timeout {
			return result, fmt.Errorf("timed out waiting for condition %s", condition)
		}
		time.Sleep(interval)
		record, err = state.Get(record.ID)
		if err != nil {
			return result, err
		}
	}
}

func runLogs(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tail := flags.Int("tail", 100, "number of trailing lines (0 means all)")
	follow := flags.Bool("follow", false, "continue streaming output while the container is running")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *tail < 0 {
		fmt.Fprintln(stderr, "Usage: boxr logs [--tail 100] [--follow] ID|NAME")
		return 2
	}
	query := flags.Arg(0)
	data, offset, err := state.ReadLogsSnapshot(query, *tail)
	if err != nil {
		fmt.Fprintf(stderr, "boxr logs: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "boxr logs: write output: %v\n", err)
		return 1
	}
	if !*follow {
		return 0
	}
	path, err := state.LogPath(query)
	if err != nil {
		fmt.Fprintf(stderr, "boxr logs: %v\n", err)
		return 1
	}
	if _, err := os.Stat(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "boxr logs: inspect log: %v\n", err)
		return 1
	}
	for {
		offset, err = state.CopyLogFrom(query, offset, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "boxr logs: follow: %v\n", err)
			return 1
		}
		record, getErr := state.Get(query)
		if getErr != nil {
			fmt.Fprintf(stderr, "boxr logs: %v\n", getErr)
			return 1
		}
		if record.Status != "running" {
			// Drain one final time in case the last write raced with process exit.
			offset, err = state.CopyLogFrom(query, offset, stdout)
			if err != nil {
				fmt.Fprintf(stderr, "boxr logs: follow: %v\n", err)
				return 1
			}
			return 0
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func parsePortMapping(value string) (container.PortMapping, error) {
	var mapping container.PortMapping
	if strings.TrimSpace(value) != value || value == "" || strings.ContainsAny(value, " \t\r\n") {
		return mapping, fmt.Errorf("expected [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp|udp]")
	}
	portPart := value
	if strings.Count(value, "/") > 1 {
		return mapping, fmt.Errorf("expected at most one /protocol suffix")
	}
	if strings.Contains(value, "/") {
		var ok bool
		portPart, mapping.Protocol, ok = strings.Cut(value, "/")
		if !ok || mapping.Protocol == "" {
			return mapping, fmt.Errorf("protocol suffix must be tcp or udp")
		}
	}
	parts := strings.Split(portPart, ":")
	if len(parts) == 2 {
		mapping.HostIP = "0.0.0.0"
	} else if len(parts) == 3 {
		mapping.HostIP = parts[0]
	} else {
		return mapping, fmt.Errorf("expected [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp|udp]")
	}
	parsePort := func(label, text string) (uint16, error) {
		value, err := strconv.ParseUint(text, 10, 16)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("%s must be a port from 1 to 65535", label)
		}
		return uint16(value), nil
	}
	var err error
	if mapping.HostPort, err = parsePort("host port", parts[len(parts)-2]); err != nil {
		return mapping, err
	}
	if mapping.ContainerPort, err = parsePort("container port", parts[len(parts)-1]); err != nil {
		return mapping, err
	}
	return mapping.Normalize()
}

func normalizePublishedPorts(mappings []container.PortMapping) []container.PortMapping {
	result := make([]container.PortMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if normalized, err := mapping.Normalize(); err == nil {
			result = append(result, normalized)
		}
	}
	return result
}

func parseVolumeMount(value string) (string, container.BindMount, error) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || strings.TrimSpace(parts[0]) == "" {
		return "", container.BindMount{}, fmt.Errorf("expected NAME:/absolute/target[:ro|rw]")
	}
	name := parts[0]
	if err := volume.ValidateName(name); err != nil {
		return "", container.BindMount{}, err
	}
	mount := container.BindMount{Target: parts[1]}
	if !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) == "/" || strings.Contains(mount.Target, "\\") {
		return "", container.BindMount{}, fmt.Errorf("volume target must be an absolute container path other than /")
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			mount.ReadOnly = true
		case "rw":
		default:
			return "", container.BindMount{}, fmt.Errorf("mount mode must be ro or rw")
		}
	}
	return name, mount, nil
}

func runNetwork(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: boxr network create --subnet CIDR [--gateway IP] [--bridge IFACE] [--no-nat] NAME | ls [--json] | inspect [--json] NAME | connect NETWORK CONTAINER | disconnect NETWORK CONTAINER | prune [--dry-run|--force] | rm NAME")
		return 2
	}
	switch args[0] {
	case "prune":
		flags := flag.NewFlagSet("network prune", flag.ContinueOnError)
		flags.SetOutput(stderr)
		dryRun := flags.Bool("dry-run", false, "show stale attachments without changing the host")
		force := flags.Bool("force", false, "remove stale Boxr-owned veth links, port rules and IPAM leases")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (*dryRun && *force) {
			fmt.Fprintln(stderr, "Usage: boxr network prune [--dry-run|--force]")
			return 2
		}
		records, err := state.List(true)
		if err != nil {
			fmt.Fprintf(stderr, "boxr network prune: list containers: %v\n", err)
			return 1
		}
		live := make(map[string]struct{})
		for _, record := range records {
			if record.Status == "running" && containerNamespaceHelperAlive(record.PID) {
				live[record.ID] = struct{}{}
			}
		}
		stale, err := boxrnetwork.StaleAttachments(live)
		if err != nil {
			fmt.Fprintf(stderr, "boxr network prune: inspect network leases: %v\n", err)
			return 1
		}
		if !*force {
			fmt.Fprintln(stdout, "DRY RUN: no host network changes will be made")
			for _, attachment := range stale {
				fmt.Fprintf(stdout, "would remove %s/%s (%s, %d published port(s))\n",
					attachment.Network, attachment.ContainerID, attachment.Address, len(attachment.PublishedPorts))
			}
			fmt.Fprintf(stdout, "stale attachments: %d\n", len(stale))
			fmt.Fprintln(stdout, "Run 'boxr network prune --force' to remove verified Boxr-owned attachments.")
			return 0
		}
		report, err := pruneManagedNetworkAttachments(live)
		fmt.Fprintf(stdout, "removed attachments: %d\nreleased IPv4 addresses: %d\nremoved published ports: %d\n",
			report.AttachmentsRemoved, report.AddressesReleased, report.PublishedPortsRemoved)
		if err != nil {
			fmt.Fprintf(stderr, "boxr network prune: %v\n", err)
			return 1
		}
		return 0
	case "create":
		flags := flag.NewFlagSet("network create", flag.ContinueOnError)
		flags.SetOutput(stderr)
		subnet := flags.String("subnet", "", "non-overlapping IPv4 subnet in canonical CIDR form (/16 to /29)")
		gateway := flags.String("gateway", "", "IPv4 gateway address (default: first usable address)")
		bridge := flags.String("bridge", "", "host bridge interface name (default: generated)")
		noNAT := flags.Bool("no-nat", false, "disable outbound NAT (host routing policy still applies)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 || *subnet == "" {
			fmt.Fprintln(stderr, "Usage: boxr network create --subnet CIDR [--gateway IP] [--bridge IFACE] [--no-nat] NAME")
			return 2
		}
		created, err := boxrnetwork.Create(flags.Arg(0), boxrnetwork.CreateOptions{
			Subnet: *subnet, Gateway: *gateway, Bridge: *bridge, NAT: !*noNAT,
		})
		if err != nil {
			fmt.Fprintf(stderr, "boxr network create: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "created bridge network %s (%s)\n", created.Name, created.ID)
		fmt.Fprintf(stdout, "bridge: %s\nsubnet: %s\ngateway: %s\nNAT: %t\n", created.Bridge, created.Subnet, created.Gateway, created.NAT)
		return 0
	case "ls":
		flags := flag.NewFlagSet("network ls", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write network list as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "Usage: boxr network ls [--json]")
			return 2
		}
		items, err := boxrnetwork.List()
		if err != nil {
			fmt.Fprintf(stderr, "boxr network ls: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, items, stderr, "boxr network ls")
		}
		fmt.Fprintln(stdout, "NAME\tDRIVER\tBRIDGE\tSUBNET\tGATEWAY\tNAT\tCONTAINERS")
		for _, item := range items {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%t\t%d\n", item.Name, item.Driver, item.Bridge, item.Subnet, item.Gateway, item.NAT, item.ContainerCount)
		}
		if len(items) == 0 {
			fmt.Fprintln(stdout, "No networks found.")
		}
		return 0
	case "inspect":
		flags := flag.NewFlagSet("network inspect", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write network metadata as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr network inspect [--json] NAME")
			return 2
		}
		item, err := boxrnetwork.Inspect(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "boxr network inspect: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, item, stderr, "boxr network inspect")
		}
		fmt.Fprintf(stdout, "Network: %s\nID: %s\nDriver: %s\nBridge: %s\nSubnet: %s\nGateway: %s\nNAT: %t\nContainers: %d\n",
			item.Name, item.ID, item.Driver, item.Bridge, item.Subnet, item.Gateway, item.NAT, item.ContainerCount)
		return 0
	case "connect":
		flags := flag.NewFlagSet("network connect", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var publish envList
		flags.Var(&publish, "publish", "publish [HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp|udp] (repeatable)")
		flags.Var(&publish, "p", "alias for --publish (repeatable)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 2 {
			fmt.Fprintln(stderr, "Usage: boxr network connect [--publish SPEC] NETWORK CONTAINER")
			return 2
		}
		networkName, containerQuery := flags.Arg(0), flags.Arg(1)
		record, err := state.Get(containerQuery)
		if err != nil {
			fmt.Fprintf(stderr, "boxr network connect: %v\n", err)
			return 1
		}
		if record.Status != "running" || record.PID <= 1 {
			fmt.Fprintf(stderr, "boxr network connect: container %s must be running\n", record.ID)
			return 1
		}
		if record.Options.NetworkMode != "none" || record.Options.UserNamespace {
			fmt.Fprintln(stderr, "boxr network connect: container must have been started with --network none without --userns so its network namespace is safely manageable")
			return 1
		}
		if err := verifyContainerNamespacePID(record.PID); err != nil {
			fmt.Fprintf(stderr, "boxr network connect: refusing to enter container PID %d: %v\n", record.PID, err)
			return 1
		}
		ports := make([]container.PortMapping, 0, len(publish))
		for _, value := range publish {
			mapping, err := parsePortMapping(value)
			if err != nil {
				fmt.Fprintf(stderr, "boxr network connect: invalid --publish: %v\n", err)
				return 2
			}
			ports = append(ports, mapping)
		}
		if err := (container.Options{NetworkMode: "bridge", NetworkName: networkName, PublishedPorts: ports}).Validate(); err != nil {
			fmt.Fprintf(stderr, "boxr network connect: %v\n", err)
			return 2
		}
		address, err := joinExistingContainerNetwork(networkName, record.PID, record.ID, ports...)
		if err != nil {
			fmt.Fprintf(stderr, "boxr network connect: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "connected container %s to network %s with IPv4 %s\n", record.ID, networkName, address)
		return 0
	case "disconnect":
		if len(args) != 3 || strings.TrimSpace(args[1]) == "" || strings.TrimSpace(args[2]) == "" {
			fmt.Fprintln(stderr, "Usage: boxr network disconnect NETWORK CONTAINER")
			return 2
		}
		record, err := state.Get(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "boxr network disconnect: %v\n", err)
			return 1
		}
		if record.Options.NetworkMode != "none" || record.Options.UserNamespace {
			fmt.Fprintln(stderr, "boxr network disconnect: only containers started with --network none without --userns support live network attachment")
			return 1
		}
		if err := leaveExistingContainerNetwork(args[1], record.ID); err != nil {
			fmt.Fprintf(stderr, "boxr network disconnect: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "disconnected container %s from network %s\n", record.ID, args[1])
		return 0
	case "rm":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintln(stderr, "Usage: boxr network rm NAME")
			return 2
		}
		if err := boxrnetwork.Remove(args[1]); err != nil {
			fmt.Fprintf(stderr, "boxr network rm: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed network %s\n", args[1])
		return 0
	default:
		fmt.Fprintf(stderr, "boxr network: unknown action %q\n", args[0])
		return 2
	}
}

func runDetachedContainer(args []string, stdout, stderr io.Writer) int {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(stderr, "boxr run --detach: detached container execution is supported on Linux only")
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "boxr run --detach: resolve executable: %v\n", err)
		return 1
	}
	stateDir, err := state.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "boxr run --detach: resolve state directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		fmt.Fprintf(stderr, "boxr run --detach: create state directory: %v\n", err)
		return 1
	}
	info, err := os.Lstat(stateDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		fmt.Fprintln(stderr, "boxr run --detach: state directory must be a real directory")
		return 1
	}
	if err := os.Chmod(stateDir, 0700); err != nil {
		fmt.Fprintf(stderr, "boxr run --detach: secure state directory: %v\n", err)
		return 1
	}
	logFile, err := os.CreateTemp(stateDir, "detached-launch-*.log")
	if err != nil {
		fmt.Fprintf(stderr, "boxr run --detach: create launch log: %v\n", err)
		return 1
	}
	logPath := logFile.Name()
	if err := logFile.Chmod(0600); err != nil {
		_ = logFile.Close()
		_ = os.Remove(logPath)
		fmt.Fprintf(stderr, "boxr run --detach: secure launch log: %v\n", err)
		return 1
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		_ = os.Remove(logPath)
		fmt.Fprintf(stderr, "boxr run --detach: create startup handshake: %v\n", err)
		return 1
	}
	childArgs := append([]string{"__boxr-detached-run"}, args...)
	command := exec.Command(executable, childArgs...)
	command.Stdout = logFile
	command.Stderr = logFile
	command.ExtraFiles = []*os.File{writer}
	command.SysProcAttr = detachedProcessSysProcAttr()
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = logFile.Close()
		_ = os.Remove(logPath)
		fmt.Fprintf(stderr, "boxr run --detach: start supervisor: %v\n", err)
		return 1
	}
	_ = writer.Close()
	_ = logFile.Close()

	type handshake struct {
		line string
		err  error
	}
	result := make(chan handshake, 1)
	go func() {
		line, err := bufio.NewReader(reader).ReadString('\n')
		result <- handshake{line: strings.TrimSpace(line), err: err}
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	defer reader.Close()
	select {
	case received := <-result:
		if received.err != nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
			fmt.Fprintf(stderr, "boxr run --detach: supervisor exited before startup (launch log: %s): %v\n", logPath, received.err)
			return 1
		}
		fields := strings.Fields(received.line)
		if len(fields) == 3 && fields[0] == "READY" {
			if err := command.Process.Release(); err != nil {
				fmt.Fprintf(stderr, "boxr run --detach: release supervisor handle: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "container %s started detached (PID %s)\n", fields[1], fields[2])
			fmt.Fprintf(stdout, "launch log: %s\n", logPath)
			fmt.Fprintf(stdout, "use 'boxr logs %s' to follow container output\n", fields[1])
			return 0
		}
		_ = command.Wait()
		fields = strings.Fields(received.line)
		if len(fields) == 2 && fields[0] == "ERROR" {
			if code, err := strconv.Atoi(fields[1]); err == nil && code != 0 {
				fmt.Fprintf(stderr, "boxr run --detach: startup failed with exit status %d (launch log: %s)\n", code, logPath)
				return code
			}
		}
		fmt.Fprintf(stderr, "boxr run --detach: supervisor did not start the container: %s (launch log: %s)\n", received.line, logPath)
		return 1
	case <-timer.C:
		// Do not kill a supervisor on a handshake timeout: it may already have
		// created a state record or released the namespace startup barrier.
		// Leave it discoverable so the operator can inspect/stop it safely.
		if err := command.Process.Release(); err != nil {
			fmt.Fprintf(stderr, "boxr run --detach: startup is not confirmed and supervisor handle release failed: %v (check 'boxr ps --all'; launch log: %s)\n", err, logPath)
			return 1
		}
		fmt.Fprintf(stderr, "boxr run --detach: startup is not confirmed after 30s; supervisor remains detached (check 'boxr ps --all'; launch log: %s)\n", logPath)
		return 1
	}
}

func containerNamespaceHelperAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	commandLine, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return false
	}
	arguments := strings.Split(string(commandLine), "\x00")
	if len(arguments) < 3 || arguments[1] != "__boxr-init" {
		return false
	}
	if _, err := os.Stat(filepath.Join(procDir, "ns", "net")); err != nil {
		return false
	}
	return true
}

func verifyContainerRuntimePID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("host PID must be greater than 1")
	}
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	currentExecutable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Boxr executable: %w", err)
	}
	currentInfo, err := os.Stat(currentExecutable)
	if err != nil {
		return fmt.Errorf("inspect Boxr executable: %w", err)
	}
	processExecutable := filepath.Join(procDir, "exe")
	processInfo, err := os.Stat(processExecutable)
	if err != nil {
		return fmt.Errorf("inspect process executable: %w", err)
	}
	if !os.SameFile(currentInfo, processInfo) {
		return fmt.Errorf("PID does not belong to this Boxr executable")
	}
	commandLine, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return fmt.Errorf("read process command line: %w", err)
	}
	arguments := strings.Split(string(commandLine), "\x00")
	if len(arguments) < 3 || arguments[1] != "__boxr-init" {
		return fmt.Errorf("PID is not a Boxr container namespace helper")
	}
	if _, err := os.Stat(filepath.Join(procDir, "ns", "net")); err != nil {
		return fmt.Errorf("inspect container network namespace: %w", err)
	}
	return nil
}

func runVolume(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: boxr volume create|ls|inspect|rm|prune ...")
		return 2
	}
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("volume create", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var labels envList
		flags.Var(&labels, "label", "volume metadata label (repeatable KEY=VALUE)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr volume create [--label KEY=VALUE] NAME")
			return 2
		}
		parsed, err := parseLabels(labels)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume create: %v\n", err)
			return 2
		}
		created, err := volume.Create(flags.Arg(0), parsed)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume create: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, created.Name)
		return 0
	case "import":
		flags := flag.NewFlagSet("volume import", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var labels envList
		flags.Var(&labels, "label", "volume metadata label (repeatable KEY=VALUE)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 2 {
			fmt.Fprintln(stderr, "Usage: boxr volume import [--label KEY=VALUE] ARCHIVE.tar.gz NAME")
			return 2
		}
		parsed, err := parseLabels(labels)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume import: %v\n", err)
			return 2
		}
		created, manifest, err := volume.ImportArchive(flags.Arg(1), flags.Arg(0), parsed)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume import: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "imported %s: %d file(s), %d directory(s), %d symlink(s)\n", created.Name, manifest.Files, manifest.Directories, manifest.Symlinks)
		return 0
	case "ls", "list":
		flags := flag.NewFlagSet("volume ls", flag.ContinueOnError)
		flags.SetOutput(stderr)
		format := flags.String("format", "table", "output format: table or json")
		quiet := flags.Bool("quiet", false, "print volume names only")
		var filters envList
		flags.Var(&filters, "filter", "filter by name=VALUE or label=KEY[=VALUE]")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (*format != "table" && *format != "json") {
			fmt.Fprintln(stderr, "Usage: boxr volume ls [--format table|json] [--quiet]")
			return 2
		}
		items, err := volume.List()
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume ls: %v\n", err)
			return 1
		}
		items, err = filterVolumeList(items, filters)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume ls: %v\n", err)
			return 2
		}
		if *quiet {
			for _, item := range items {
				fmt.Fprintln(stdout, item.Name)
			}
			return 0
		}
		if *format == "json" {
			return writeJSON(stdout, items, stderr, "boxr volume ls")
		}
		fmt.Fprintln(stdout, "NAME                 PATH")
		for _, item := range items {
			fmt.Fprintf(stdout, "%-20s %s\n", item.Name, item.Path)
		}
		return 0
	case "du":
		flags := flag.NewFlagSet("volume du", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write volume usage as JSON")
		all := flags.Bool("all", false, "measure every named volume")
		var filters envList
		flags.Var(&filters, "filter", "filter all-volume usage by name=VALUE or label=KEY[=VALUE]")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if (*all && flags.NArg() != 0) || (!*all && (flags.NArg() != 1 || len(filters) != 0)) {
			fmt.Fprintln(stderr, "Usage: boxr volume du [--json] NAME | boxr volume du --all [--json] [--filter name=VALUE|label=KEY[=VALUE]]")
			return 2
		}
		if !*all {
			usage, err := volume.Measure(flags.Arg(0))
			if err != nil {
				fmt.Fprintf(stderr, "boxr volume du: %v\n", err)
				return 1
			}
			if *jsonOutput {
				return writeJSON(stdout, usage, stderr, "boxr volume du")
			}
			fmt.Fprintf(stdout, "%s: %d file(s), %d directory(s), %d symlink(s), %d other, %d bytes (snapshot)\n", usage.Name, usage.Files, usage.Directories, usage.Symlinks, usage.OtherEntries, usage.TotalBytes)
			return 0
		}
		items, err := volume.List()
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume du: %v\n", err)
			return 1
		}
		items, err = filterVolumeList(items, filters)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume du: %v\n", err)
			return 2
		}
		report := volumeUsageSummary{Volumes: make([]volume.Usage, 0, len(items))}
		for _, item := range items {
			usage, err := volume.Measure(item.Name)
			if err != nil {
				fmt.Fprintf(stderr, "boxr volume du: %s: %v\n", item.Name, err)
				return 1
			}
			report.Volumes = append(report.Volumes, usage)
			report.Files += usage.Files
			report.Directories += usage.Directories
			report.Symlinks += usage.Symlinks
			report.OtherEntries += usage.OtherEntries
			report.TotalBytes += usage.TotalBytes
		}
		report.VolumeCount = len(report.Volumes)
		if *jsonOutput {
			return writeJSON(stdout, report, stderr, "boxr volume du")
		}
		fmt.Fprintln(stdout, "NAME                 FILES      DIRS       SYMLINKS   OTHER      SIZE")
		for _, usage := range report.Volumes {
			fmt.Fprintf(stdout, "%-20s %-10d %-10d %-10d %-10d %d bytes\n", usage.Name, usage.Files, usage.Directories, usage.Symlinks, usage.OtherEntries, usage.TotalBytes)
		}
		fmt.Fprintf(stdout, "TOTAL (%d volume(s))  %d file(s), %d directory(s), %d symlink(s), %d other, %d bytes\n",
			report.VolumeCount, report.Files, report.Directories, report.Symlinks, report.OtherEntries, report.TotalBytes)
		return 0
	case "inspect":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: boxr volume inspect NAME")
			return 2
		}
		item, err := volume.Get(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume inspect: %v\n", err)
			return 1
		}
		return writeJSON(stdout, item, stderr, "boxr volume inspect")
	case "clone":
		flags := flag.NewFlagSet("volume clone", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var labels envList
		flags.Var(&labels, "label", "metadata label for the new volume (repeatable KEY=VALUE)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 2 {
			fmt.Fprintln(stderr, "Usage: boxr volume clone [--label KEY=VALUE] SOURCE TARGET")
			return 2
		}
		parsed, err := parseLabels(labels)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume clone: %v\n", err)
			return 2
		}
		source, err := volume.Get(flags.Arg(0))
		if err == nil {
			err = ensureVolumeUnused(source.Path)
		}
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume clone: %v\n", err)
			return 1
		}
		archive, err := os.CreateTemp("", "boxr-volume-clone-*.tar.gz")
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume clone: %v\n", err)
			return 1
		}
		archivePath := archive.Name()
		if err := archive.Close(); err != nil {
			_ = os.Remove(archivePath)
			fmt.Fprintf(stderr, "boxr volume clone: %v\n", err)
			return 1
		}
		defer os.Remove(archivePath)
		manifest, err := rootfs.PackTarGz(source.Path, archivePath)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume clone: snapshot source: %v\n", err)
			return 1
		}
		cloned, _, err := volume.ImportArchive(flags.Arg(1), archivePath, parsed)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume clone: restore target: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "cloned %s to %s: %d file(s), %d directory(s), %d symlink(s)\n", source.Name, cloned.Name, manifest.Files, manifest.Directories, manifest.Symlinks)
		return 0
	case "export":
		flags := flag.NewFlagSet("volume export", flag.ContinueOnError)
		flags.SetOutput(stderr)
		jsonOutput := flags.Bool("json", false, "write archive path, SHA-256 digest and manifest as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 2 {
			fmt.Fprintln(stderr, "Usage: boxr volume export [--json] NAME ARCHIVE.tar.gz")
			return 2
		}
		item, err := volume.Get(flags.Arg(0))
		if err == nil {
			err = ensureVolumeUnused(item.Path)
		}
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume export: %v\n", err)
			return 1
		}
		archive, err := volume.ExportArchive(item.Name, flags.Arg(1))
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume export: %v\n", err)
			return 1
		}
		if *jsonOutput {
			return writeJSON(stdout, archive, stderr, "boxr volume export")
		}
		fmt.Fprintf(stdout, "exported %s to %s (%s; %d bytes; %d file(s), %d directory(s), %d symlink(s))\n",
			archive.Volume, archive.Path, archive.Digest, archive.SizeBytes,
			archive.Manifest.Files, archive.Manifest.Directories, archive.Manifest.Symlinks)
		return 0
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "Usage: boxr volume rm NAME [...]")
			return 2
		}
		failed := false
		for _, name := range args[1:] {
			item, err := volume.Get(name)
			if err == nil {
				err = ensureVolumeUnused(item.Path)
			}
			if err == nil {
				err = volume.Remove(name)
			}
			if err != nil {
				fmt.Fprintf(stderr, "boxr volume rm: %s: %v\n", name, err)
				failed = true
			} else {
				fmt.Fprintf(stdout, "removed %s\n", name)
			}
		}
		if failed {
			return 1
		}
		return 0
	case "prune":
		flags := flag.NewFlagSet("volume prune", flag.ContinueOnError)
		flags.SetOutput(stderr)
		force := flags.Bool("force", false, "confirm deletion of all unused volumes and their contents")
		dryRun := flags.Bool("dry-run", false, "preview unused volumes without deleting them")
		jsonOutput := flags.Bool("json", false, "write prune results as JSON")
		var filters envList
		flags.Var(&filters, "filter", "limit prune to name=VALUE or label=KEY[=VALUE]")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (!*force && !*dryRun) || (*force && *dryRun) {
			fmt.Fprintln(stderr, "Usage: boxr volume prune (--force|--dry-run) [--json] [--filter name=VALUE|label=KEY[=VALUE]]")
			return 2
		}
		items, err := volume.List()
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume prune: %v\n", err)
			return 1
		}
		items, err = filterVolumeList(items, filters)
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume prune: %v\n", err)
			return 2
		}
		report := volumePruneReport{
			DryRun:     *dryRun,
			Candidates: make([]volumePruneCandidate, 0),
			InUse:      make([]volumePruneBlocked, 0),
			Removed:    make([]string, 0),
		}
		for _, item := range items {
			if err := ensureVolumeUnused(item.Path); err != nil {
				var inUse *volumeInUseError
				if errors.As(err, &inUse) {
					report.InUse = append(report.InUse, volumePruneBlocked{Name: item.Name, Reason: err.Error()})
					continue
				}
				fmt.Fprintf(stderr, "boxr volume prune: inspect %s: %v\n", item.Name, err)
				return 1
			}
			if *dryRun {
				report.Candidates = append(report.Candidates, volumePruneCandidate{Name: item.Name, Labels: item.Labels, CreatedAt: item.CreatedAt})
				continue
			}
			if err := volume.Remove(item.Name); err != nil {
				fmt.Fprintf(stderr, "boxr volume prune: %s: %v\n", item.Name, err)
				return 1
			}
			report.Removed = append(report.Removed, item.Name)
		}
		if *jsonOutput {
			report.RemovedCount = len(report.Removed)
			return writeJSON(stdout, report, stderr, "boxr volume prune")
		}
		if *dryRun {
			fmt.Fprintf(stdout, "would remove %d unused volume(s); protected %d volume(s)\n", len(report.Candidates), len(report.InUse))
			for _, item := range report.Candidates {
				fmt.Fprintf(stdout, "  %s\n", item.Name)
			}
			return 0
		}
		fmt.Fprintf(stdout, "removed %d unused volume(s)\n", len(report.Removed))
		return 0
	default:
		fmt.Fprintf(stderr, "boxr volume: unknown action %q\n", args[0])
		return 2
	}
}

type volumePruneCandidate struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
}

type volumePruneBlocked struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type volumePruneReport struct {
	DryRun       bool                   `json:"dryRun"`
	Candidates   []volumePruneCandidate `json:"candidates"`
	InUse        []volumePruneBlocked   `json:"inUse"`
	Removed      []string               `json:"removed,omitempty"`
	RemovedCount int                    `json:"removedCount"`
}

func parseLabels(values []string) (map[string]string, error) {
	labels := make(map[string]string)
	for _, value := range values {
		key, labelValue, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid label %q (expected KEY=VALUE)", value)
		}
		labels[key] = labelValue
	}
	return labels, nil
}

type volumeInUseError struct {
	ContainerID string
}

func (e *volumeInUseError) Error() string {
	return fmt.Sprintf("volume is mounted by active container %s", e.ContainerID)
}

func ensureVolumeUnused(path string) error {
	records, err := state.List(false)
	if err != nil {
		return fmt.Errorf("inspect active containers: %w", err)
	}
	for _, record := range records {
		for _, mount := range record.Options.BindMounts {
			if mount.Source == path {
				return &volumeInUseError{ContainerID: record.ID}
			}
		}
	}
	return nil
}

type volumeUsageSummary struct {
	VolumeCount  int            `json:"volumeCount"`
	Volumes      []volume.Usage `json:"volumes"`
	Files        int64          `json:"files"`
	Directories  int64          `json:"directories"`
	Symlinks     int64          `json:"symlinks"`
	OtherEntries int64          `json:"otherEntries"`
	TotalBytes   int64          `json:"totalBytes"`
}

func filterVolumeList(items []volume.Volume, filters []string) ([]volume.Volume, error) {
	filtered := append([]volume.Volume(nil), items...)
	for _, filter := range filters {
		key, value, ok := strings.Cut(filter, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid filter %q (expected name=VALUE or label=KEY[=VALUE])", filter)
		}
		if key != "name" && key != "label" {
			return nil, fmt.Errorf("unsupported filter key %q", key)
		}
		if key == "name" && value == "" || key == "label" && strings.TrimSpace(strings.SplitN(value, "=", 2)[0]) == "" {
			return nil, fmt.Errorf("invalid filter %q", filter)
		}
		next := filtered[:0]
		for _, item := range filtered {
			match := false
			switch key {
			case "name":
				match = item.Name == value
			case "label":
				labelKey, labelValue, hasValue := strings.Cut(value, "=")
				stored, exists := item.Labels[labelKey]
				if hasValue {
					match = exists && stored == labelValue
				} else {
					match = exists
				}
			default:
				return nil, fmt.Errorf("unsupported filter key %q", key)
			}
			if match {
				next = append(next, item)
			}
		}
		filtered = next
	}
	return filtered, nil
}
