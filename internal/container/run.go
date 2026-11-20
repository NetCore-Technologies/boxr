//go:build linux

package container

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Run starts a helper as PID 1 in new PID, UTS, mount, and IPC namespaces.
// The helper blocks until the parent has applied any requested cgroup limits,
// preventing the workload from running outside its configured limits.
func Run(command string, args []string, options Options) error {
	return RunWithHooks(command, args, options, Hooks{})
}

// RunWithHooks is Run with host-side lifecycle callbacks for state tracking.
func RunWithHooks(command string, args []string, options Options, hooks Hooks) error {
	if command == "" {
		return fmt.Errorf("command cannot be empty")
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate boxr executable: %w", err)
	}
	if err := options.Validate(); err != nil {
		return fmt.Errorf("invalid container options: %w", err)
	}
	var overlayLock *os.File
	if options.OverlayDir != "" {
		overlayLock, err = acquireOverlayLock(options.OverlayDir)
		if err != nil {
			return err
		}
		defer overlayLock.Close()
	}
	resolvedCommand := command
	if options.RootFS == "" {
		resolvedCommand, err = exec.LookPath(command)
		if err != nil {
			return fmt.Errorf("find command %q: %w", command, err)
		}
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create runtime synchronization pipe: %w", err)
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	encodedOptions, err := json.Marshal(options)
	if err != nil {
		_ = readyReader.Close()
		_ = readyWriter.Close()
		return fmt.Errorf("encode runtime options: %w", err)
	}
	helperArgs := []string{"__boxr-init", base64.RawURLEncoding.EncodeToString(encodedOptions), resolvedCommand}
	helperArgs = append(helperArgs, args...)
	cmd := exec.Command(executable, helperArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = hooks.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = hooks.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.ExtraFiles = []*os.File{readyReader}
	if len(options.Env) > 0 {
		cmd.Env = mergeEnvironment(os.Environ(), options.Env)
	}
	cloneFlags := uintptr(syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC)
	if options.NetworkMode == "none" {
		cloneFlags |= syscall.CLONE_NEWNET
	}
	if options.UserNamespace {
		cloneFlags |= syscall.CLONE_NEWUSER
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
		Setpgid:    true,
		Pdeathsig:  syscall.SIGKILL,
	}

	if err := cmd.Start(); err != nil {
		_ = readyReader.Close()
		_ = readyWriter.Close()
		return fmt.Errorf("start isolated process (root or appropriate namespace capabilities are required): %w", err)
	}
	_ = readyReader.Close()
	if options.UserNamespace {
		if err := configureUserNamespace(cmd.Process.Pid); err != nil {
			_ = readyWriter.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("configure user namespace: %w", err)
		}
	}
	if hooks.OnStart != nil {
		if err := hooks.OnStart(cmd.Process.Pid); err != nil {
			_ = readyWriter.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("record container startup: %w", err)
		}
	}

	// Keep the namespace helper in a separate host process group. Terminal
	// signals received by this CLI are forwarded to the helper and then to the
	// workload process group inside the PID namespace.
	forwardDone := make(chan struct{})
	forwarderDone := make(chan struct{})
	go func() {
		defer close(forwarderDone)
		for {
			select {
			case received := <-signals:
				if sig, ok := received.(syscall.Signal); ok {
					_ = cmd.Process.Signal(sig)
				}
			case <-forwardDone:
				return
			}
		}
	}()
	stopForwarder := func() {
		close(forwardDone)
		<-forwarderDone
	}

	cleanupCgroup, err := applyCgroup(cmd.Process.Pid, options)
	if err != nil {
		_ = readyWriter.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stopForwarder()
		runErr := fmt.Errorf("configure cgroup limits: %w", err)
		if hooks.OnExit != nil {
			if hookErr := hooks.OnExit(cmd.Process.Pid, runErr); hookErr != nil {
				return fmt.Errorf("%v; record container exit: %w", runErr, hookErr)
			}
		}
		return runErr
	}

	if _, err := readyWriter.Write([]byte{1}); err != nil {
		_ = readyWriter.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stopForwarder()
		if cleanupCgroup != nil {
			_ = cleanupCgroup()
		}
		runErr := fmt.Errorf("release container startup barrier: %w", err)
		if hooks.OnExit != nil {
			if hookErr := hooks.OnExit(cmd.Process.Pid, runErr); hookErr != nil {
				return fmt.Errorf("%v; record container exit: %w", runErr, hookErr)
			}
		}
		return runErr
	}
	_ = readyWriter.Close()

	waitErr := cmd.Wait()
	stopForwarder()
	var cleanupErr error
	if cleanupCgroup != nil {
		cleanupErr = cleanupCgroup()
	}
	if hooks.OnExit != nil {
		if hookErr := hooks.OnExit(cmd.Process.Pid, waitErr); hookErr != nil {
			if waitErr != nil {
				return fmt.Errorf("container exited with %v; record container exit: %w", waitErr, hookErr)
			}
			return fmt.Errorf("record container exit: %w", hookErr)
		}
	}
	if cleanupErr != nil {
		if waitErr != nil {
			return fmt.Errorf("container exited with %v; cgroup cleanup failed: %w", waitErr, cleanupErr)
		}
		return fmt.Errorf("clean up container cgroup: %w", cleanupErr)
	}
	return waitErr
}

// Init is the hidden PID-1 entrypoint used by Run. It is exported only because
// the CLI entrypoint and runtime live in separate Go packages.
func Init(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("invalid internal init arguments")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil {
		return fmt.Errorf("decode runtime options: %w", err)
	}
	var options Options
	if err := json.Unmarshal(encoded, &options); err != nil {
		return fmt.Errorf("parse runtime options: %w", err)
	}
	if err := options.Validate(); err != nil {
		return fmt.Errorf("invalid runtime options: %w", err)
	}
	ready := os.NewFile(uintptr(3), "boxr-ready")
	if ready == nil {
		return fmt.Errorf("startup synchronization pipe is unavailable")
	}
	defer ready.Close()

	var token [1]byte
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if _, err := io.ReadFull(ready, token[:]); err != nil {
		return fmt.Errorf("wait for parent cgroup setup: %w", err)
	}
	if token[0] != 1 {
		return fmt.Errorf("received invalid startup signal")
	}

	if options.Hostname != "" {
		if err := syscall.Sethostname([]byte(options.Hostname)); err != nil {
			return fmt.Errorf("set container hostname: %w", err)
		}
	}
	// The mount namespace starts with a copy of the host mount tree. Marking
	// it private prevents mount events from propagating back to the host.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make container mount tree private: %w", err)
	}
	if options.RootFS != "" {
		if err := setupRootFS(options); err != nil {
			return err
		}
	} else if options.Workdir != "" {
		if err := os.Chdir(options.Workdir); err != nil {
			return fmt.Errorf("change container working directory to %q: %w", options.Workdir, err)
		}
	}
	if options.NetworkMode == "none" {
		if err := bringLoopbackUp(); err != nil {
			return fmt.Errorf("configure loopback in private network namespace: %w", err)
		}
	}
	if len(options.DropCapabilities) > 0 {
		if err := dropLinuxCapabilities(options.DropCapabilities); err != nil {
			return fmt.Errorf("drop capabilities: %w", err)
		}
	}
	if options.NoNewPrivileges || len(options.DropCapabilities) > 0 || options.SeccompProfile != "" {
		if err := setNoNewPrivileges(); err != nil {
			return fmt.Errorf("set no-new-privileges: %w", err)
		}
	}
	if options.SeccompProfile != "" {
		if err := applySeccompProfile(options.SeccompProfile); err != nil {
			return fmt.Errorf("apply seccomp profile %q: %w", options.SeccompProfile, err)
		}
	}
	commandArgs := args[1:]
	if options.RootFS != "" {
		resolved, err := exec.LookPath(commandArgs[0])
		if err != nil {
			return fmt.Errorf("find command %q inside rootfs: %w", commandArgs[0], err)
		}
		commandArgs[0] = resolved
	}
	return runAsInit(commandArgs, signals)
}

func setupRootFS(options Options) error {
	root, err := filepath.EvalSymlinks(options.RootFS)
	if err != nil {
		return fmt.Errorf("resolve rootfs %q: %w", options.RootFS, err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("rootfs %q is not a directory", root)
	}
	if options.OverlayDir != "" {
		root, err = prepareOverlayRoot(root, options.OverlayDir)
		if err != nil {
			return err
		}
	}
	if err := syscall.Mount(root, root, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs: %w", err)
	}
	for _, mount := range options.BindMounts {
		source, err := filepath.EvalSymlinks(mount.Source)
		if err != nil {
			return fmt.Errorf("resolve bind source %q: %w", mount.Source, err)
		}
		sourceInfo, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("inspect bind source %q: %w", source, err)
		}
		target, err := PrepareRootPath(root, mount.Target, sourceInfo.IsDir())
		if err != nil {
			return fmt.Errorf("prepare bind target %q: %w", mount.Target, err)
		}
		if err := syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind mount %s at %s: %w", source, target, err)
		}
		if mount.ReadOnly {
			if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("make bind mount %s read-only: %w", target, err)
			}
		}
	}
	for _, mount := range options.TmpfsMounts {
		target, err := PrepareRootPath(root, mount.Target, true)
		if err != nil {
			return fmt.Errorf("prepare tmpfs target %q: %w", mount.Target, err)
		}
		data := "mode=" + strconv.FormatUint(uint64(mount.Mode), 8)
		if mount.SizeBytes > 0 {
			data += ",size=" + strconv.FormatInt(mount.SizeBytes, 10)
		}
		if err := syscall.Mount("tmpfs", target, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, data); err != nil {
			return fmt.Errorf("mount tmpfs at %s: %w", target, err)
		}
	}
	if options.MountProc {
		if _, err := PrepareRootPath(root, "/proc", true); err != nil {
			return fmt.Errorf("prepare proc mountpoint: %w", err)
		}
		if err := syscall.Mount("proc", filepath.Join(root, "proc"), "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mount proc: %w", err)
		}
	}
	if options.ReadOnlyRootFS {
		if err := syscall.Mount("", root, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("make rootfs read-only: %w", err)
		}
	}
	if err := os.Chdir(root); err != nil {
		return fmt.Errorf("enter rootfs: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot to %s: %w", root, err)
	}
	workdir := options.Workdir
	if workdir == "" {
		workdir = "/"
	}
	if err := os.Chdir(workdir); err != nil {
		return fmt.Errorf("change container working directory to %q: %w", workdir, err)
	}
	return nil
}

// prepareOverlayRoot mounts a persistent OverlayFS writable layer over the
// supplied lower rootfs. Upper and work directories are deliberately kept
// outside the image tree so writes never mutate the source rootfs.
func prepareOverlayRoot(lower, overlayDir string) (string, error) {
	overlayDir, err := filepath.Abs(overlayDir)
	if err != nil {
		return "", fmt.Errorf("resolve overlay directory: %w", err)
	}
	if err := ensureOverlayDirectory(overlayDir); err != nil {
		return "", err
	}
	overlayDir, err = filepath.EvalSymlinks(overlayDir)
	if err != nil {
		return "", fmt.Errorf("resolve overlay directory: %w", err)
	}
	lower, err = filepath.EvalSymlinks(lower)
	if err != nil {
		return "", fmt.Errorf("resolve lower rootfs: %w", err)
	}
	if pathWithin(lower, overlayDir) || pathWithin(overlayDir, lower) {
		return "", fmt.Errorf("overlay directory and rootfs must be separate directory trees")
	}
	for _, value := range []string{lower, overlayDir} {
		if strings.ContainsAny(value, ",:\\") {
			return "", fmt.Errorf("resolved OverlayFS paths cannot contain commas, colons, or backslashes")
		}
	}
	upper := filepath.Join(overlayDir, "upper")
	work := filepath.Join(overlayDir, "work")
	merged := filepath.Join(overlayDir, "merged")
	for _, directory := range []string{upper, work, merged} {
		if err := ensureOverlayDirectory(directory); err != nil {
			return "", err
		}
	}
	if entries, err := os.ReadDir(work); err != nil {
		return "", fmt.Errorf("inspect OverlayFS work directory: %w", err)
	} else if len(entries) != 0 {
		return "", fmt.Errorf("OverlayFS work directory %s is not empty; inspect it before retrying", work)
	}
	if entries, err := os.ReadDir(merged); err != nil {
		return "", fmt.Errorf("inspect OverlayFS mountpoint: %w", err)
	} else if len(entries) != 0 {
		return "", fmt.Errorf("OverlayFS mountpoint %s is not empty", merged)
	}
	data := "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work
	if err := syscall.Mount("overlay", merged, "overlay", syscall.MS_NODEV|syscall.MS_NOSUID, data); err != nil {
		return "", fmt.Errorf("mount OverlayFS (upper/work must be on the same filesystem and the kernel must support overlayfs): %w", err)
	}
	return merged, nil
}

// acquireOverlayLock prevents two Boxr workloads from mounting the same
// upper/work pair concurrently, which OverlayFS does not support safely.
func acquireOverlayLock(overlayDir string) (*os.File, error) {
	absolute, err := filepath.Abs(overlayDir)
	if err != nil {
		return nil, fmt.Errorf("resolve OverlayFS lock directory: %w", err)
	}
	if err := ensureOverlayDirectory(absolute); err != nil {
		return nil, err
	}
	// Canonicalize before selecting the lock path so two symlink-free aliases
	// cannot acquire separate locks for the same upper/work directories.
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve OverlayFS lock directory: %w", err)
	}
	lockPath := filepath.Join(canonical, ".boxr.lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open OverlayFS lock without following symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("wrap OverlayFS lock descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect OverlayFS lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("OverlayFS lock %s must be a regular file", lockPath)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("OverlayFS storage %s is already in use by another container", canonical)
	}
	return file, nil
}

func ensureOverlayDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create OverlayFS directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("OverlayFS path %s must be a real directory, not a symlink", path)
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("secure OverlayFS directory %s: %w", path, err)
	}
	return nil
}

// configureUserNamespace maps the invoking host UID/GID to root inside the
// newly-created user namespace. The helper is blocked on its startup pipe until
// these files have been written successfully.
func configureUserNamespace(pid int) error {
	uid := os.Getuid()
	gid := os.Getgid()
	proc := "/proc/" + strconv.Itoa(pid)
	if err := os.WriteFile(filepath.Join(proc, "setgroups"), []byte("deny"), 0600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("write setgroups: %w", err)
	}
	if err := os.WriteFile(filepath.Join(proc, "uid_map"), []byte(fmt.Sprintf("0 %d 1\n", uid)), 0600); err != nil {
		return fmt.Errorf("write uid_map: %w", err)
	}
	if err := os.WriteFile(filepath.Join(proc, "gid_map"), []byte(fmt.Sprintf("0 %d 1\n", gid)), 0600); err != nil {
		return fmt.Errorf("write gid_map: %w", err)
	}
	return nil
}

const (
	prSetNoNewPrivileges = 38
	prCapBsetDrop        = 24
	capabilityVersion3   = 0x20080522
)

type capabilityHeader struct {
	Version uint32
	PID     int32
}
type capabilityData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

func setNoNewPrivileges() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivileges, 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func dropLinuxCapabilities(values []string) error {
	numbers, _ := normalizedCapabilities(values)
	// Drop the bounding set while CAP_SETPCAP is still present in the effective set.
	for _, number := range numbers {
		_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prCapBsetDrop, uintptr(number), 0)
		if errno != 0 && errno != syscall.EINVAL {
			return fmt.Errorf("drop capability %d from bounding set: %w", number, errno)
		}
	}
	header := capabilityHeader{Version: capabilityVersion3}
	var data [2]capabilityData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("read process capabilities: %w", errno)
	}
	for _, number := range numbers {
		word, bit := number/32, uint(number%32)
		mask := ^(uint32(1) << bit)
		data[word].Effective &= mask
		data[word].Permitted &= mask
		data[word].Inheritable &= mask
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("apply capability set: %w", errno)
	}
	return nil
}

func bringLoopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var request [40]byte
	copy(request[:16], []byte("lo"))
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(0x8913), uintptr(unsafe.Pointer(&request[0]))) // SIOCGIFFLAGS
	if errno != 0 {
		return errno
	}
	flags := *(*uint16)(unsafe.Pointer(&request[16]))
	flags |= 0x1 | 0x40 // IFF_UP | IFF_RUNNING
	*(*uint16)(unsafe.Pointer(&request[16])) = flags
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(0x8914), uintptr(unsafe.Pointer(&request[0]))) // SIOCSIFFLAGS
	if errno != 0 {
		return errno
	}
	return nil
}

// runAsInit keeps a small PID-1 process alive while the workload runs as its
// child. It forwards signals and reaps exited children, avoiding the special
// signal semantics and zombie accumulation of executing the workload as PID 1.
func runAsInit(commandArgs []string, signals <-chan os.Signal) error {
	if len(commandArgs) == 0 {
		return fmt.Errorf("workload command is missing")
	}
	pid, err := syscall.ForkExec(commandArgs[0], commandArgs, &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return fmt.Errorf("start workload %q: %w", commandArgs[0], err)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	var workloadStatus syscall.WaitStatus
	for {
		select {
		case received := <-signals:
			if sig, ok := received.(syscall.Signal); ok {
				_ = syscall.Kill(-pid, sig)
			}
		default:
		}

		var status syscall.WaitStatus
		waitedPID, waitErr := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if waitErr != nil && waitErr != syscall.EINTR {
			return fmt.Errorf("reap container child: %w", waitErr)
		}
		if waitedPID == pid {
			workloadStatus = status
			break
		}
		if waitedPID == -1 && waitErr == nil {
			return fmt.Errorf("workload process disappeared before it could be reaped")
		}
		<-ticker.C
	}

	// The primary command has exited. Stop any remaining members of its process
	// group; any process that escaped the group is still contained by this PID
	// namespace and will be terminated when the namespace init exits.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if workloadStatus.Exited() {
		if code := workloadStatus.ExitStatus(); code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	if workloadStatus.Signaled() {
		return &ExitError{Code: 128 + int(workloadStatus.Signal())}
	}
	return &ExitError{Code: 1}
}
