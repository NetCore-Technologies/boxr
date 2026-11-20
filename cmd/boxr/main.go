package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/config"
	"github.com/manit6752025/boxr/internal/container"
	imageStore "github.com/manit6752025/boxr/internal/image"
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// The helper is PID 1 inside the newly-created namespaces. It waits for the
	// parent to attach it to its cgroup before it executes the requested command.
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

	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
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
	case "features":
		return runFeatures(args[1:], stdout, stderr)
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
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
	default:
		fmt.Fprintf(stderr, "boxr: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runContainer(args []string, stdout, stderr io.Writer) int {
	var opts container.Options
	var memory, memoryHigh, memoryLow, memoryMin, memorySwap string
	var configPath string
	var imageReference string
	var configCommand []string
	var configEnv []string
	var env, envFiles, capDrop, mounts, tmpfs, labels, volumes envList
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

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: boxr run [options] [--] COMMAND [ARG...]")
		fmt.Fprintln(stderr, "\nOptions:")
		flags.PrintDefaults()
	}
	flags.StringVar(&configPath, "config", configPath, "load versioned JSON run configuration")
	flags.StringVar(&imageReference, "image", "", "run from a verified local Boxr image (NAME:TAG)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate and print a redacted execution plan without starting a container")
	flags.StringVar(&opts.Name, "name", opts.Name, "unique local container name")
	flags.Var(&labels, "label", "container metadata label (repeatable KEY=VALUE)")
	flags.BoolVar(&opts.AutoRemove, "rm", opts.AutoRemove, "remove the local record after the container exits")
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
	flags.StringVar(&opts.NetworkMode, "network", opts.NetworkMode, "network namespace mode: host or none")
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
	if dryRun {
		plan := buildRunPlan(commandArgs, opts)
		plan.ImageReference = imageReference
		return writeJSON(stdout, plan, stderr, "boxr run --dry-run")
	}

	record, err := state.Create(opts.Name, commandArgs, opts, opts.Labels)
	if err != nil {
		fmt.Fprintf(stderr, "boxr run: state: %v\n", err)
		return 1
	}
	logFile, err := state.OpenLog(record.ID)
	if err != nil {
		_ = state.RemoveCreated(record.ID)
		fmt.Fprintf(stderr, "boxr run: log setup: %v\n", err)
		return 1
	}
	defer logFile.Close()
	combinedLog := io.MultiWriter(logFile)
	started := false
	runErr := container.RunWithHooks(commandArgs[0], commandArgs[1:], opts, container.Hooks{
		Stdout: io.MultiWriter(stdout, combinedLog),
		Stderr: io.MultiWriter(stderr, combinedLog),
		OnStart: func(pid int) error {
			if err := state.Start(record.ID, pid); err != nil {
				return err
			}
			started = true
			return nil
		},
		OnExit: func(_ int, executionErr error) error {
			return state.Finish(record.ID, executionErr)
		},
	})
	if !started {
		_ = state.RemoveCreated(record.ID)
	}
	if opts.AutoRemove && started {
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

type runPlan struct {
	Mode                 string   `json:"mode"`
	Command              string   `json:"command"`
	ArgumentCount        int      `json:"argumentCount"`
	ContainerName        string   `json:"containerName,omitempty"`
	ImageReference       string   `json:"imageReference,omitempty"`
	WorkingDirectory     string   `json:"workingDirectory,omitempty"`
	RootFSConfigured     bool     `json:"rootfsConfigured"`
	OverlayConfigured    bool     `json:"overlayConfigured"`
	NetworkMode          string   `json:"networkMode"`
	MemoryLimitBytes     int64    `json:"memoryLimitBytes,omitempty"`
	MemoryHighBytes      int64    `json:"memoryHighBytes,omitempty"`
	CPUs                 float64  `json:"cpus,omitempty"`
	CPUWeight            int      `json:"cpuWeight,omitempty"`
	IOWeight             int      `json:"ioWeight,omitempty"`
	PIDsLimit            int64    `json:"pidsLimit,omitempty"`
	BindMountCount       int      `json:"bindMountCount"`
	TmpfsMountCount      int      `json:"tmpfsMountCount"`
	EnvironmentKeys      []string `json:"environmentKeys"`
	EnvironmentValues    string   `json:"environmentValues"`
	LabelKeys            []string `json:"labelKeys"`
	LabelValues          string   `json:"labelValues"`
	DropCapabilityCount  int      `json:"dropCapabilityCount"`
	UserNamespace        bool     `json:"userNamespace"`
	NoNewPrivileges      bool     `json:"noNewPrivileges"`
	SeccompProfile       string   `json:"seccompProfile,omitempty"`
	SecurityProfile      string   `json:"securityProfile,omitempty"`
	ResourceProfile      string   `json:"resourceProfile,omitempty"`
	NoContainerStateMade bool     `json:"noContainerStateCreated"`
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
		}(), MemoryLimitBytes: opts.MemoryLimitBytes,
		MemoryHighBytes: opts.MemoryHighBytes, CPUs: opts.CPUs, CPUWeight: opts.CPUWeight,
		IOWeight: opts.IOWeight, PIDsLimit: opts.PidsLimit, BindMountCount: len(opts.BindMounts),
		TmpfsMountCount: len(opts.TmpfsMounts), EnvironmentKeys: envKeys, EnvironmentValues: "redacted",
		LabelKeys: labelKeys, LabelValues: "redacted", DropCapabilityCount: len(opts.DropCapabilities),
		UserNamespace: opts.UserNamespace, NoNewPrivileges: opts.NoNewPrivileges,
		SeccompProfile: opts.SeccompProfile, SecurityProfile: opts.SecurityProfile, ResourceProfile: opts.ResourceProfile, NoContainerStateMade: true,
	}
	return plan
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
  boxr run [options] [--dry-run] COMMAND [ARG...]
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
  boxr volume create [--label KEY=VALUE] NAME
  boxr volume ls [--format table|json] [--filter label=KEY=VALUE]
  boxr volume inspect NAME
  boxr volume du [--json] NAME
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
  boxr image build --tag NAME:TAG [--file Boxrfile.json] [CONTEXT]
  boxr image import ROOTFS NAME:TAG
  boxr image pull [--platform linux/ARCH[/VARIANT]] [--authfile PATH] REGISTRY/IMAGE[:TAG]
  boxr image push [--authfile PATH] LOCAL:TAG [REGISTRY/IMAGE:TAG]
  boxr image ls [--format table|json] [--quiet] [--filter KEY=VALUE]
  boxr image inspect NAME:TAG
  boxr image verify NAME:TAG
  boxr image audit [--json] [--strict] NAME:TAG
  boxr image du [--json]
  boxr image save NAME:TAG ARCHIVE.tar.gz
  boxr image load ARCHIVE.tar.gz
  boxr image unpack NAME:TAG PATH
  boxr image rm NAME:TAG [...]
  boxr image prune [--dry-run|--force]
  boxr config validate FILE.json
  boxr config example
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
  --network host|none use host networking or a private network namespace
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
		file := flags.String("file", "", "JSON build recipe (default: CONTEXT/Boxrfile.json; non-executing)")
		platform := flags.String("platform", "", "target image platform (default: linux/<host architecture>)")
		tag := flags.String("tag", "", "image reference to create (required)")
		jsonOutput := flags.Bool("json", false, "write image metadata as JSON")
		quiet := flags.Bool("quiet", false, "print only the resulting image reference")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if *tag == "" || flags.NArg() > 1 || (*quiet && *jsonOutput) {
			fmt.Fprintln(stderr, "Usage: boxr image build --tag NAME:TAG [--platform linux/ARCH[/VARIANT]] [--file Boxrfile.json] [--json|--quiet] [CONTEXT]")
			return 2
		}
		contextDir := "."
		if flags.NArg() == 1 {
			contextDir = flags.Arg(0)
		}
		buildFile := *file
		if buildFile == "" {
			buildFile = filepath.Join(contextDir, "Boxrfile.json")
		}
		created, err := imageStore.BuildForPlatform(contextDir, buildFile, *tag, *platform)
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
		if _, err := rootfs.ExtractTarGz(artifact.LayerPath, rootPath, rootfs.Limits{}); err != nil {
			fmt.Fprintf(stderr, "boxr image pull: extract rootfs: %v\n", err)
			return 1
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
		if len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
			fmt.Fprintln(stderr, "Usage: boxr image du [--json]")
			return 2
		}
		usage, err := imageStore.Usage()
		if err != nil {
			fmt.Fprintf(stderr, "boxr image du: %v\n", err)
			return 1
		}
		if len(args) == 2 {
			return writeJSON(stdout, usage, stderr, "boxr image du")
		}
		fmt.Fprintf(stdout, "Images: %d\nUnique blobs: %d (%d bytes)\nReachable: %d blobs (%d bytes)\nReclaimable: %d blobs (%d bytes)\n", usage.Images, usage.UniqueBlobs, usage.TotalBytes, usage.ReachableBlobs, usage.ReachableBytes, usage.ReclaimableBlobs, usage.ReclaimableBytes)
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
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr image save NAME:TAG ARCHIVE.tar.gz")
			return 2
		}
		if err := imageStore.Save(args[1], args[2]); err != nil {
			fmt.Fprintf(stderr, "boxr image save: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "saved %s to %s\n", args[1], args[2])
		return 0
	case "load":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "Usage: boxr image load ARCHIVE.tar.gz")
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
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 || (*dryRun == *force) {
			fmt.Fprintln(stderr, "Usage: boxr image prune [--dry-run|--force]")
			return 2
		}
		items, err := imageStore.Prune(!*force)
		if err != nil {
			fmt.Fprintf(stderr, "boxr image prune: %v\n", err)
			return 1
		}
		verb := "would remove"
		if *force {
			verb = "removed"
		}
		for _, digest := range items {
			fmt.Fprintf(stdout, "%s %s\n", verb, digest)
		}
		fmt.Fprintf(stdout, "%d unreferenced blob(s) %s\n", len(items), map[bool]string{true: "removed", false: "found"}[*force])
		return 0
	default:
		fmt.Fprintf(stderr, "boxr image: unknown action %q\n", args[0])
		return 2
	}
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
	commands := "run ps inspect stop kill wait logs volume image stats rm events prune info system doctor support-bundle rootfs config features version help completion"
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
		if record.Status != "running" {
			fmt.Fprintf(stdout, "%s is already %s\n", record.ID, record.Status)
			continue
		}
		if err := state.Signal(record.ID, "TERM"); err != nil {
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
		if err == nil && record.Status != "running" {
			err = fmt.Errorf("container is %s, not running", record.Status)
		}
		if err == nil {
			err = state.Signal(record.ID, *signalName)
		}
		if err != nil {
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
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "Usage: boxr volume du [--json] NAME")
			return 2
		}
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
		if len(args) != 3 {
			fmt.Fprintln(stderr, "Usage: boxr volume export NAME ARCHIVE.tar.gz")
			return 2
		}
		item, err := volume.Get(args[1])
		if err == nil {
			err = ensureVolumeUnused(item.Path)
		}
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume export: %v\n", err)
			return 1
		}
		manifest, err := rootfs.PackTarGz(item.Path, args[2])
		if err != nil {
			fmt.Fprintf(stderr, "boxr volume export: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "exported %s: %d file(s), %d directory(s), %d symlink(s), %d bytes\n", item.Name, manifest.Files, manifest.Directories, manifest.Symlinks, manifest.Bytes)
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
				report.InUse = append(report.InUse, volumePruneBlocked{Name: item.Name, Reason: err.Error()})
				continue
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

func ensureVolumeUnused(path string) error {
	records, err := state.List(false)
	if err != nil {
		return fmt.Errorf("inspect active containers: %w", err)
	}
	for _, record := range records {
		for _, mount := range record.Options.BindMounts {
			if mount.Source == path {
				return fmt.Errorf("volume is mounted by active container %s", record.ID)
			}
		}
	}
	return nil
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
