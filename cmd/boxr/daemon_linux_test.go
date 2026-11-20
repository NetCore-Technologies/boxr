//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manit6752025/boxr/internal/engineapi"
)

func TestParseDaemonHostAcceptsOnlyAbsoluteUnixSockets(t *testing.T) {
	for _, value := range []string{
		"unix:///run/user/1000/boxr/engine.sock",
		"unix:///tmp/boxr.sock",
	} {
		if path, err := parseDaemonHost(value); err != nil || !filepath.IsAbs(path) {
			t.Errorf("parseDaemonHost(%q) = %q, %v", value, path, err)
		}
	}
	for _, value := range []string{
		"tcp://127.0.0.1:2375",
		"unix://relative.sock",
		"unix:///tmp/boxr.sock?mode=0666",
		"file:///tmp/boxr.sock",
	} {
		if _, err := parseDaemonHost(value); err == nil {
			t.Errorf("parseDaemonHost(%q) unexpectedly succeeded", value)
		}
	}
}

func TestListenDaemonSocketSetsPrivatePermissionsAndRecoversStaleSocket(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "engine.sock")
	listener, info, err := listenDaemonSocket(path)
	if err != nil {
		t.Fatalf("listen on socket: %v", err)
	}
	socketInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0600 {
		t.Fatalf("socket mode = %v, want socket 0600", socketInfo.Mode())
	}
	if _, _, err := listenDaemonSocket(path); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second listener error = %v, want already listening", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	staleListener, staleInfo, err := listenDaemonSocket(path)
	if err != nil {
		t.Fatalf("recover stale socket: %v", err)
	}
	defer staleListener.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("restarted socket is missing: %v", err)
	}
	_ = info
	_ = staleInfo
}

func TestEnsureDaemonSocketDirectoryRejectsSymlinkAndWritableParents(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureDaemonSocketDirectory(filepath.Join(link, "socket-dir")); err == nil {
		t.Fatal("symlink in socket directory path was accepted")
	}
	if err := os.Chmod(real, 0777); err != nil {
		t.Fatal(err)
	}
	if err := ensureDaemonSocketDirectory(real); err == nil {
		t.Fatal("world-writable socket directory was accepted")
	}
}

func TestListenDaemonSocketDoesNotReplaceRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, _, err := listenDaemonSocket(path)
	if err == nil {
		_ = listener.Close()
		t.Fatal("regular file was replaced with a socket")
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "do not replace" {
		t.Fatalf("regular file changed: %q %v", data, readErr)
	}
}


func TestDaemonUnixSocketServesDockerPing(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "engine.sock")
	listener, _, err := listenDaemonSocket(socketPath)
	if err != nil {
		t.Fatalf("listen on daemon socket: %v", err)
	}
	server := &http.Server{Handler: engineapi.New("test-version")}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		<-serveDone
	})

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Get("http://boxr/_ping")
	if err != nil {
		t.Fatalf("GET /_ping over unix socket: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ping status=%d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "OK" || response.Header.Get("API-Version") != "1.45" {
		t.Fatalf("ping response body=%q API-Version=%q", body, response.Header.Get("API-Version"))
	}
}

func TestRunDaemonRejectsTCPAndPrintsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runDaemon([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon --help returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unix:///absolute/path") {
		t.Fatalf("daemon help missing Unix socket usage: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runDaemon([]string{"--host", "tcp://127.0.0.1:2375"}, &stdout, &stderr); code != 2 {
		t.Fatalf("daemon TCP host returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "TCP listeners are disabled") {
		t.Fatalf("daemon TCP rejection message = %q", stderr.String())
	}
}
