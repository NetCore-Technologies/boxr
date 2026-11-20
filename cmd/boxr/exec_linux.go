//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
	"golang.org/x/term"
)

const maxExecEnvironmentBytes = 1 << 20

type execHelperConfig struct {
	RootFS           bool     `json:"rootFS"`
	Workdir          string   `json:"workdir,omitempty"`
	Command          []string `json:"command"`
	DropCapabilities []string `json:"dropCapabilities,omitempty"`
	NoNewPrivileges  bool     `json:"noNewPrivileges,omitempty"`
	SeccompProfile   string   `json:"seccompProfile,omitempty"`
}

// runExec starts a command in an existing container. The launcher joins the
// target cgroup before nsenter forks, so all descendants inherit its limits.
func runExec(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var env, envFiles envList
	var workdir string
	var interactive, tty bool
	flags.StringVar(&workdir, "workdir", "", "working directory inside the container")
	flags.StringVar(&workdir, "w", "", "alias for --workdir")
	flags.BoolVar(&interactive, "interactive", false, "keep standard input open")
	flags.BoolVar(&interactive, "i", false, "alias for --interactive")
	flags.BoolVar(&tty, "tty", false, "allocate a pseudo-terminal")
	flags.BoolVar(&tty, "t", false, "alias for --tty")
	flags.Var(&env, "env", "set or override an environment variable (repeatable KEY=VALUE)")
	flags.Var(&env, "e", "alias for --env (repeatable)")
	flags.Var(&envFiles, "env-file", "load environment assignments from a file (repeatable)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: boxr exec [-i] [-t] [--workdir PATH] [--env KEY=VALUE] [--env-file PATH] CONTAINER COMMAND [ARG...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() < 2 {
		flags.Usage()
		return 2
	}
	if workdir != "" && (!filepath.IsAbs(workdir) || strings.ContainsRune(workdir, '\x00')) {
		fmt.Fprintln(stderr, "boxr exec: --workdir must be an absolute path without NUL characters")
		return 2
	}
	for _, entry := range env {
		if err := validateEnv(entry); err != nil {
			fmt.Fprintf(stderr, "boxr exec: invalid --env: %v\n", err)
			return 2
		}
	}
	fileEnvironment := make([]string, 0)
	for _, path := range envFiles {
		values, err := readEnvFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "boxr exec: env file %s: %v\n", path, err)
			return 2
		}
		fileEnvironment = append(fileEnvironment, values...)
	}
	command := append([]string(nil), flags.Args()[1:]...)
	for index, value := range command {
		if value == "" || strings.ContainsRune(value, '\x00') {
			fmt.Fprintf(stderr, "boxr exec: command argument %d is empty or contains NUL\n", index+1)
			return 2
		}
	}
	if len(command) > 256 {
		fmt.Fprintln(stderr, "boxr exec: command contains more than 256 arguments")
		return 2
	}

	record, err := state.Get(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: %v\n", err)
		return 1
	}
	if record.Status != "running" || record.PID <= 1 {
		fmt.Fprintf(stderr, "boxr exec: container %s is %s, not running\n", record.ID, record.Status)
		return 1
	}
	if err := state.VerifyProcessIdentity(record); err != nil {
		fmt.Fprintf(stderr, "boxr exec: refusing stale container PID: %v\n", err)
		return 1
	}
	if !state.ProcessAlive(record.PID) {
		fmt.Fprintf(stderr, "boxr exec: container %s is no longer alive\n", record.ID)
		return 1
	}
	expectedStartTicks := record.ProcessStartTicks
	if expectedStartTicks == 0 {
		// Older records did not persist a start tick. Capture one now so the
		// launch handshake can still detect PID reuse during namespace setup.
		expectedStartTicks, err = state.ReadProcessStartTicks(record.PID)
		if err != nil {
			fmt.Fprintf(stderr, "boxr exec: capture container process identity: %v\n", err)
			return 1
		}
	}
	if verifyContainerNamespacePID != nil {
		if err := verifyContainerNamespacePID(record.PID); err != nil {
			fmt.Fprintf(stderr, "boxr exec: container namespace verification failed: %v\n", err)
			return 1
		}
	}

	nsenterPath, err := exec.LookPath("nsenter")
	if err != nil {
		fmt.Fprintln(stderr, "boxr exec: nsenter is required for namespace entry (install util-linux)")
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: resolve Boxr executable: %v\n", err)
		return 1
	}
	rootFile, err := openProcDirectory(record.PID, "root")
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: open container root: %v\n", err)
		return 1
	}
	defer rootFile.Close()
	cwdFile, err := openProcDirectory(record.PID, "cwd")
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: open container working directory: %v\n", err)
		return 1
	}
	defer cwdFile.Close()

	namespaceNames := []string{"mnt", "uts", "ipc", "net", "pid"}
	namespaceFiles := make([]*os.File, 0, len(namespaceNames))
	for _, name := range namespaceNames {
		file, openErr := openProcNamespace(record.PID, name)
		if openErr != nil {
			for _, opened := range namespaceFiles {
				_ = opened.Close()
			}
			fmt.Fprintf(stderr, "boxr exec: open container %s namespace: %v\n", name, openErr)
			return 1
		}
		namespaceFiles = append(namespaceFiles, file)
	}
	defer func() {
		for _, file := range namespaceFiles {
			_ = file.Close()
		}
	}()
	var userNamespaceFile *os.File
	if record.Options.UserNamespace {
		userNamespaceFile, err = openProcNamespace(record.PID, "user")
		if err != nil {
			fmt.Fprintf(stderr, "boxr exec: open container user namespace: %v\n", err)
			return 1
		}
		defer userNamespaceFile.Close()
	}

	baseEnvironment, err := readContainerEnvironment(record.PID)
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: read container environment: %v\n", err)
		return 1
	}
	environment := container.MergeEnvironment(baseEnvironment, fileEnvironment, []string(env))
	environmentJSON, err := json.Marshal(environment)
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: encode container environment: %v\n", err)
		return 1
	}
	if len(environmentJSON) > maxExecEnvironmentBytes {
		fmt.Fprintf(stderr, "boxr exec: combined environment exceeds %d bytes\n", maxExecEnvironmentBytes)
		return 2
	}

	helperConfig := execHelperConfig{
		RootFS:           record.Options.RootFS != "",
		Workdir:          workdir,
		Command:          command,
		DropCapabilities: append([]string(nil), record.Options.DropCapabilities...),
		NoNewPrivileges:  record.Options.NoNewPrivileges,
		SeccompProfile:   record.Options.SeccompProfile,
	}
	if helperConfig.RootFS && helperConfig.Workdir == "" {
		helperConfig.Workdir = record.Options.Workdir
		if helperConfig.Workdir == "" {
			helperConfig.Workdir = "/"
		}
	}
	configJSON, err := json.Marshal(helperConfig)
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: encode helper configuration: %v\n", err)
		return 1
	}
	encodedConfig := base64.RawURLEncoding.EncodeToString(configJSON)

	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: create launch barrier: %v\n", err)
		return 1
	}
	defer releaseWriter.Close()
	environmentReader, environmentWriter, err := os.Pipe()
	if err != nil {
		_ = releaseReader.Close()
		fmt.Fprintf(stderr, "boxr exec: create environment pipe: %v\n", err)
		return 1
	}
	defer environmentWriter.Close()

	// ExtraFiles are mapped to descriptors 3 onward in this order:
	// release=3, root=4, cwd=5, environment=6, namespaces=7..11, user=12.
	extraFiles := []*os.File{releaseReader, rootFile, cwdFile, environmentReader}
	extraFiles = append(extraFiles, namespaceFiles...)
	if userNamespaceFile != nil {
		extraFiles = append(extraFiles, userNamespaceFile)
	}
	nsenterArgs := []string{nsenterPath, "--fork"}
	if record.Options.UserNamespace {
		nsenterArgs = append(nsenterArgs,
			"--user=/proc/self/fd/12", "--setuid=0", "--setgid=0")
	}
	nsenterArgs = append(nsenterArgs,
		"--mount=/proc/self/fd/7",
		"--uts=/proc/self/fd/8",
		"--ipc=/proc/self/fd/9",
		"--net=/proc/self/fd/10",
		"--pid=/proc/self/fd/11",
		"--",
		executable, "__boxr-exec", encodedConfig,
	)

	launcherArgs := []string{
		"__boxr-exec-launch", nsenterPath, record.ID,
		strconv.Itoa(record.PID), strconv.FormatUint(expectedStartTicks, 10),
	}
	launcherArgs = append(launcherArgs, nsenterArgs[1:]...)
	launcher := exec.Command(executable, launcherArgs...)
	launcher.ExtraFiles = extraFiles
	var terminalCleanup func()
	if tty {
		var terminalErr error
		_, terminalCleanup, terminalErr = startExecTerminal(launcher, stdout)
		if terminalErr != nil {
			fmt.Fprintf(stderr, "boxr exec: allocate terminal: %v\n", terminalErr)
			_ = releaseReader.Close()
			_ = environmentReader.Close()
			return 1
		}
	} else {
		if interactive {
			launcher.Stdin = os.Stdin
		}
		launcher.Stdout = stdout
		launcher.Stderr = stderr
		if err := launcher.Start(); err != nil {
			fmt.Fprintf(stderr, "boxr exec: start namespace launcher: %v\n", err)
			_ = releaseReader.Close()
			_ = environmentReader.Close()
			return 1
		}
	}
	if terminalCleanup != nil {
		defer terminalCleanup()
	}
	_ = releaseReader.Close()
	_ = environmentReader.Close()

	if execNeedsContainerCgroup(record.Options) {
		if err := attachExecProcessToCgroup(record.PID, launcher.Process.Pid); err != nil {
			_, _ = releaseWriter.Write([]byte{0})
			_ = releaseWriter.Close()
			_ = environmentWriter.Close()
			_ = launcher.Wait()
			fmt.Fprintf(stderr, "boxr exec: attach launcher to container cgroup: %v\n", err)
			return 1
		}
	}

	environmentWriteDone := make(chan error, 1)
	go func() {
		n, writeErr := environmentWriter.Write(environmentJSON)
		if writeErr == nil && n != len(environmentJSON) {
			writeErr = io.ErrShortWrite
		}
		closeErr := environmentWriter.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		environmentWriteDone <- writeErr
	}()
	if n, writeErr := releaseWriter.Write([]byte{1}); writeErr != nil || n != 1 {
		_ = releaseWriter.Close()
		_ = launcher.Process.Kill()
		_ = launcher.Wait()
		<-environmentWriteDone
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		fmt.Fprintf(stderr, "boxr exec: release namespace launcher: %v\n", writeErr)
		return 1
	}
	_ = releaseWriter.Close()

	waitErr := launcher.Wait()
	environmentWriteErr := <-environmentWriteDone
	if environmentWriteErr != nil && waitErr == nil {
		fmt.Fprintf(stderr, "boxr exec: transfer container environment: %v\n", environmentWriteErr)
		return 125
	}
	if waitErr == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		if code := exitError.ExitCode(); code >= 0 {
			return code
		}
	}
	fmt.Fprintf(stderr, "boxr exec: launcher failed: %v\n", waitErr)
	return 125
}

func startExecTerminal(launcher *exec.Cmd, output io.Writer) (*os.File, func(), error) {
	sizeSource := firstExecTerminal(os.Stdin, os.Stdout, os.Stderr)
	if sizeSource == nil {
		return nil, nil, fmt.Errorf("--tty requires a terminal on stdin, stdout, or stderr")
	}
	size, err := pty.GetsizeFull(sizeSource)
	if err != nil {
		return nil, nil, fmt.Errorf("read terminal size: %w", err)
	}
	var rawState *term.State
	if term.IsTerminal(int(os.Stdin.Fd())) {
		rawState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return nil, nil, fmt.Errorf("set terminal raw mode: %w", err)
		}
	}
	restoreTerminal := func() {
		if rawState != nil {
			_ = term.Restore(int(os.Stdin.Fd()), rawState)
		}
	}
	launcher.Stdin = nil
	launcher.Stdout = nil
	launcher.Stderr = nil
	master, err := pty.StartWithSize(launcher, size)
	if err != nil {
		restoreTerminal()
		return nil, nil, err
	}
	done := make(chan struct{})
	resizeSignals := make(chan os.Signal, 1)
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		for {
			select {
			case <-resizeSignals:
				_ = pty.InheritSize(sizeSource, master)
			case <-done:
				return
			}
		}
	}()
	outputDone := make(chan struct{})
	if output == nil {
		output = io.Discard
	}
	go func() {
		_, _ = io.Copy(output, master)
		close(outputDone)
	}()
	go func() {
		_, _ = io.Copy(master, os.Stdin)
	}()
	cleanup := func() {
		close(done)
		signal.Stop(resizeSignals)
		<-resizeDone
		_ = master.Close()
		<-outputDone
		restoreTerminal()
	}
	return master, cleanup, nil
}

func firstExecTerminal(files ...*os.File) *os.File {
	for _, file := range files {
		if file != nil && term.IsTerminal(int(file.Fd())) {
			return file
		}
	}
	return nil
}

func runExecLaunch(args []string) int {
	if len(args) < 5 {
		fmt.Fprintln(os.Stderr, "boxr exec: invalid internal launcher arguments")
		return 125
	}
	release := os.NewFile(uintptr(3), "boxr-exec-release")
	if release == nil {
		fmt.Fprintln(os.Stderr, "boxr exec: launch barrier is unavailable")
		return 125
	}
	var readySignal [1]byte
	if _, err := io.ReadFull(release, readySignal[:]); err != nil || readySignal[0] != 1 {
		_ = release.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "boxr exec: launch barrier failed: %v\n", err)
		}
		return 125
	}
	_ = release.Close()

	expectedPID, err := strconv.Atoi(args[2])
	if err != nil || expectedPID <= 1 {
		fmt.Fprintln(os.Stderr, "boxr exec: invalid expected container PID")
		return 125
	}
	expectedStartTicks, err := strconv.ParseUint(args[3], 10, 64)
	if err != nil || expectedStartTicks == 0 {
		fmt.Fprintln(os.Stderr, "boxr exec: invalid expected process identity")
		return 125
	}
	record, err := state.Get(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxr exec: recheck container state: %v\n", err)
		return 125
	}
	if record.Status != "running" || record.PID != expectedPID ||
		(record.ProcessStartTicks != 0 && record.ProcessStartTicks != expectedStartTicks) {
		fmt.Fprintln(os.Stderr, "boxr exec: container restarted or stopped before namespace entry")
		return 125
	}
	identity := record
	identity.ProcessStartTicks = expectedStartTicks
	if !state.ProcessAlive(record.PID) {
		fmt.Fprintf(os.Stderr, "boxr exec: container %s exited before namespace entry\n", record.ID)
		return 125
	}
	if err := state.VerifyProcessIdentity(identity); err != nil {
		fmt.Fprintf(os.Stderr, "boxr exec: container identity changed before namespace entry: %v\n", err)
		return 125
	}
	if verifyContainerNamespacePID != nil {
		if err := verifyContainerNamespacePID(record.PID); err != nil {
			fmt.Fprintf(os.Stderr, "boxr exec: namespace helper changed before entry: %v\n", err)
			return 125
		}
	}
	nsenterPath := args[0]
	nsenterArgs := append([]string{nsenterPath}, args[4:]...)
	if err := syscall.Exec(nsenterPath, nsenterArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "boxr exec: start nsenter: %v\n", err)
		return 125
	}
	return 0
}

func runExecChild(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "boxr exec: invalid internal command arguments")
		return 125
	}
	configJSON, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: decode helper configuration: %v\n", err)
		return 125
	}
	var helperConfig execHelperConfig
	if err := json.Unmarshal(configJSON, &helperConfig); err != nil {
		fmt.Fprintf(stderr, "boxr exec: read helper configuration: %v\n", err)
		return 125
	}
	if len(helperConfig.Command) == 0 {
		fmt.Fprintln(stderr, "boxr exec: helper command is missing")
		return 125
	}
	for _, argument := range helperConfig.Command {
		if argument == "" || strings.ContainsRune(argument, '\x00') {
			fmt.Fprintln(stderr, "boxr exec: helper command contains an invalid argument")
			return 125
		}
	}
	if helperConfig.Workdir != "" && (!filepath.IsAbs(helperConfig.Workdir) || strings.ContainsRune(helperConfig.Workdir, '\x00')) {
		fmt.Fprintln(stderr, "boxr exec: helper working directory is invalid")
		return 125
	}

	environmentFile := os.NewFile(uintptr(6), "boxr-exec-environment")
	if environmentFile == nil {
		fmt.Fprintln(stderr, "boxr exec: container environment descriptor is unavailable")
		return 125
	}
	environmentJSON, err := io.ReadAll(io.LimitReader(environmentFile, maxExecEnvironmentBytes+1))
	_ = environmentFile.Close()
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: read container environment: %v\n", err)
		return 125
	}
	if len(environmentJSON) > maxExecEnvironmentBytes {
		fmt.Fprintf(stderr, "boxr exec: container environment exceeds %d bytes\n", maxExecEnvironmentBytes)
		return 125
	}
	var environment []string
	if err := json.Unmarshal(environmentJSON, &environment); err != nil {
		fmt.Fprintf(stderr, "boxr exec: decode container environment: %v\n", err)
		return 125
	}
	for index, entry := range environment {
		if err := validateEnv(entry); err != nil {
			fmt.Fprintf(stderr, "boxr exec: invalid inherited environment entry %d: %v\n", index+1, err)
			return 125
		}
	}

	if helperConfig.RootFS {
		if err := syscall.Fchdir(4); err != nil {
			fmt.Fprintf(stderr, "boxr exec: enter container root: %v\n", err)
			return 125
		}
		if err := syscall.Chroot("."); err != nil {
			fmt.Fprintf(stderr, "boxr exec: enter container root: %v\n", err)
			return 125
		}
	}
	if helperConfig.Workdir != "" {
		if err := os.Chdir(helperConfig.Workdir); err != nil {
			fmt.Fprintf(stderr, "boxr exec: change working directory to %q: %v\n", helperConfig.Workdir, err)
			return 125
		}
	} else if helperConfig.RootFS {
		if err := os.Chdir("/"); err != nil {
			fmt.Fprintf(stderr, "boxr exec: change to container root: %v\n", err)
			return 125
		}
	} else if err := syscall.Fchdir(5); err != nil {
		fmt.Fprintf(stderr, "boxr exec: restore container working directory: %v\n", err)
		return 125
	}
	for descriptor := 3; descriptor <= 12; descriptor++ {
		_ = syscall.Close(descriptor)
	}

	security := container.Options{
		DropCapabilities: helperConfig.DropCapabilities,
		NoNewPrivileges:  helperConfig.NoNewPrivileges,
		SeccompProfile:   helperConfig.SeccompProfile,
	}
	if err := container.ApplyExecSecurity(security); err != nil {
		fmt.Fprintf(stderr, "boxr exec: apply container security policy: %v\n", err)
		return 125
	}
	resolved, err := findExecPath(helperConfig.Command[0], environment)
	if err != nil {
		fmt.Fprintf(stderr, "boxr exec: %v\n", err)
		return 127
	}
	if err := syscall.Exec(resolved, helperConfig.Command, environment); err != nil {
		fmt.Fprintf(stderr, "boxr exec: execute %q: %v\n", helperConfig.Command[0], err)
		return 126
	}
	return 0
}

func findExecPath(command string, environment []string) (string, error) {
	if command == "" || strings.ContainsRune(command, '\x00') {
		return "", fmt.Errorf("invalid executable name")
	}
	if strings.ContainsRune(command, '/') {
		info, err := os.Stat(command)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", fmt.Errorf("%q is not an executable regular file", command)
		}
		return command, nil
	}
	pathValue := "/bin:/usr/bin"
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "PATH" {
			pathValue = value
		}
	}
	for _, directory := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, command)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q was not found in the container PATH", command)
}

const linuxOPath = 0x200000 // Linux O_PATH: open a directory handle without requiring read access.

func openProcDirectory(pid int, entry string) (*os.File, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), entry)
	fd, err := syscall.Open(path, linuxOPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	return file, nil
}

func openProcNamespace(pid int, namespace string) (*os.File, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "ns", namespace)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func readContainerEnvironment(pid int) ([]string, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "environ")
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxExecEnvironmentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxExecEnvironmentBytes {
		return nil, fmt.Errorf("container environment exceeds %d bytes", maxExecEnvironmentBytes)
	}
	return parseProcEnvironment(data)
}

func parseProcEnvironment(data []byte) ([]string, error) {
	if len(data) > maxExecEnvironmentBytes {
		return nil, fmt.Errorf("environment exceeds %d bytes", maxExecEnvironmentBytes)
	}
	values := make([]string, 0)
	for _, raw := range bytes.Split(data, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		entry := string(raw)
		if err := validateEnv(entry); err != nil {
			return nil, fmt.Errorf("invalid process environment entry: %w", err)
		}
		values = append(values, entry)
	}
	return container.MergeEnvironment(values), nil
}

func execNeedsContainerCgroup(options container.Options) bool {
	return options.MemoryLimitBytes > 0 || options.MemoryHighBytes > 0 ||
		options.MemoryLowBytes > 0 || options.MemoryMinBytes > 0 ||
		options.MemorySwapMaxBytes > 0 || options.MemoryOOMGroup ||
		options.CPUs > 0 || options.CPUWeight > 0 ||
		options.IOWeight > 0 || options.PidsLimit > 0
}

func attachExecProcessToCgroup(containerPID, processPID int) error {
	if containerPID <= 1 || processPID <= 1 {
		return fmt.Errorf("container and exec PIDs must be greater than 1")
	}
	root := "/sys/fs/cgroup/boxr"
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("Boxr cgroup root is not a real directory")
	}
	path := filepath.Join(root, strconv.Itoa(containerPID))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("container cgroup is not a real directory")
	}
	controlPath := filepath.Join(path, "cgroup.procs")
	fd, err := syscall.Open(controlPath, syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	control := os.NewFile(uintptr(fd), controlPath)
	defer control.Close()
	_, err = control.WriteString(strconv.Itoa(processPID))
	return err
}
