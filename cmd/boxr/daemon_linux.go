//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/manit6752025/boxr/internal/engineapi"
)

func runDaemon(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	host := defaultDaemonHost()
	flags.StringVar(&host, "host", host, "listen on a local unix:// socket")
	flags.StringVar(&host, "H", host, "alias for --host")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: boxr daemon [--host unix:///absolute/path/engine.sock]")
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "boxr daemon: unexpected positional arguments")
		return 2
	}
	socketPath, err := parseDaemonHost(host)
	if err != nil {
		fmt.Fprintf(stderr, "boxr daemon: %v\n", err)
		return 2
	}
	listener, socketInfo, err := listenDaemonSocket(socketPath)
	if err != nil {
		fmt.Fprintf(stderr, "boxr daemon: %v\n", err)
		return 1
	}
	defer func() {
		_ = listener.Close()
		if current, statErr := os.Lstat(socketPath); statErr == nil && os.SameFile(socketInfo, current) {
			_ = os.Remove(socketPath)
		}
	}()

	server := &http.Server{
		Handler:           engineapi.New(version),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(stderr, "boxr daemon: ", log.LstdFlags),
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()
	fmt.Fprintf(stdout, "Boxr Engine API listening on unix://%s (owner-only socket)\n", socketPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return 0
		}
		fmt.Fprintf(stderr, "boxr daemon: serve: %v\n", err)
		return 1
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			fmt.Fprintf(stderr, "boxr daemon: graceful shutdown: %v\n", err)
			return 1
		}
		if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(stderr, "boxr daemon: serve: %v\n", err)
			return 1
		}
		return 0
	}
}

func defaultDaemonHost() string {
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return "unix://" + filepath.Join(runtimeDir, "boxr", "engine.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "unix://" + filepath.Join(os.TempDir(), "boxr-"+strconv.Itoa(os.Getuid()), "engine.sock")
	}
	return "unix://" + filepath.Join(home, ".cache", "boxr", "run", "engine.sock")
}

func parseDaemonHost(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid --host: %w", err)
	}
	if parsed.Scheme != "unix" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("only unix:///absolute/path sockets are supported; TCP listeners are disabled")
	}
	if !filepath.IsAbs(parsed.Path) || strings.ContainsRune(parsed.Path, '\x00') {
		return "", fmt.Errorf("unix socket path must be an absolute path")
	}
	if len(parsed.Path) > 100 {
		return "", fmt.Errorf("unix socket path is too long")
	}
	return filepath.Clean(parsed.Path), nil
}

func listenDaemonSocket(path string) (net.Listener, os.FileInfo, error) {
	directory := filepath.Dir(path)
	if err := ensureDaemonSocketDirectory(directory); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("refusing to replace non-socket path %s", path)
		}
		connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, nil, fmt.Errorf("another Boxr daemon is already listening on %s", path)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("cannot safely determine whether socket %s is stale: %w", path, dialErr)
		}
		if err := os.Remove(path); err != nil {
			return nil, nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect socket path: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on unix socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("secure unix socket permissions: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		_ = listener.Close()
		_ = os.Remove(path)
		if err == nil {
			err = fmt.Errorf("created socket path is not a socket")
		}
		return nil, nil, err
	}
	return listener, info, nil
}

func ensureDaemonSocketDirectory(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve socket directory: %w", err)
	}
	current := string(filepath.Separator)
	relative := strings.TrimPrefix(absolute, current)
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := os.Mkdir(current, 0700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return fmt.Errorf("create socket directory %s: %w", current, mkdirErr)
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return fmt.Errorf("inspect socket directory %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("socket directory path %s must not contain symlinks or non-directories", current)
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("inspect final socket directory: %w", err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("socket directory %s must be owned by the current user", absolute)
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("socket directory %s must not be group- or world-writable", absolute)
	}
	return nil
}
