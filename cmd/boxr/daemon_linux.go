//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"bytes"
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
	flags.StringVar(&host, "host", host, "listen on unix:// or mutually authenticated tcp://")
	flags.StringVar(&host, "H", host, "alias for --host")
	tlsVerify := flags.Bool("tlsverify", false, "require TLS and verify every remote client certificate")
	tlsCACert := flags.String("tlscacert", "", "CA certificate bundle trusted to authenticate remote clients")
	tlsCert := flags.String("tlscert", "", "server TLS certificate chain")
	tlsKey := flags.String("tlskey", "", "server TLS private key (must be owner-only)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: boxr daemon [--host unix:///absolute/path/engine.sock | --host tcp://HOST:PORT --tlsverify --tlscacert CA --tlscert CERT --tlskey KEY]")
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "boxr daemon: unexpected positional arguments")
		return 2
	}
	var err error
	var listener net.Listener
	var serveListener net.Listener
	var socketPath string
	var socketInfo os.FileInfo
	tcpListener := strings.HasPrefix(strings.ToLower(host), "tcp://")
	if tcpListener {
		if !*tlsVerify {
			fmt.Fprintln(stderr, "boxr daemon: TCP listeners require --tlsverify; plaintext remote access is disabled")
			return 2
		}
		if strings.TrimSpace(*tlsCACert) == "" || strings.TrimSpace(*tlsCert) == "" || strings.TrimSpace(*tlsKey) == "" {
			fmt.Fprintln(stderr, "boxr daemon: --tlsverify requires --tlscacert, --tlscert, and --tlskey")
			return 2
		}
		address, parseErr := parseDaemonTCPHost(host)
		if parseErr != nil {
			fmt.Fprintf(stderr, "boxr daemon: %v\n", parseErr)
			return 2
		}
		tlsConfig, tlsErr := loadDaemonTLSConfig(*tlsCACert, *tlsCert, *tlsKey)
		if tlsErr != nil {
			fmt.Fprintf(stderr, "boxr daemon: configure mutual TLS: %v\n", tlsErr)
			return 2
		}
		listener, err = net.Listen("tcp", address)
		if err != nil {
			fmt.Fprintf(stderr, "boxr daemon: listen on %s: %v\n", address, err)
			return 1
		}
		serveListener = tls.NewListener(listener, tlsConfig)
	} else {
		if *tlsVerify || strings.TrimSpace(*tlsCACert) != "" || strings.TrimSpace(*tlsCert) != "" || strings.TrimSpace(*tlsKey) != "" {
			fmt.Fprintln(stderr, "boxr daemon: TLS options require a tcp:// listener")
			return 2
		}
		socketPath, err = parseDaemonHost(host)
		if err != nil {
			fmt.Fprintf(stderr, "boxr daemon: %v\n", err)
			return 2
		}
		listener, socketInfo, err = listenDaemonSocket(socketPath)
		if err != nil {
			fmt.Fprintf(stderr, "boxr daemon: %v\n", err)
			return 1
		}
		serveListener = listener
	}
	defer func() {
		_ = listener.Close()
		if socketPath != "" && socketInfo != nil {
			if current, statErr := os.Lstat(socketPath); statErr == nil && os.SameFile(socketInfo, current) {
				_ = os.Remove(socketPath)
			}
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
		serveResult <- server.Serve(serveListener)
	}()
	if tcpListener {
		fmt.Fprintf(stdout, "Boxr Engine API listening on https://%s (mutual TLS; client certificates required)\n", listener.Addr())
	} else {
		fmt.Fprintf(stdout, "Boxr Engine API listening on unix://%s (owner-only socket)\n", socketPath)
	}

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

func parseDaemonTCPHost(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid --host: %w", err)
	}
	if parsed.Scheme != "tcp" || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", fmt.Errorf("TCP host must be tcp://HOST:PORT without a path, query, fragment, or user information")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" || strings.TrimSpace(host) != host || port == "" {
		return "", fmt.Errorf("TCP host must include an explicit bind address and port")
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return "", fmt.Errorf("TCP port must be a decimal number")
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("TCP port must be between 1 and 65535")
	}
	return net.JoinHostPort(host, strconv.Itoa(portNumber)), nil
}

const maxDaemonTLSFileBytes = 1 << 20

func readDaemonTLSFile(path string, private bool) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("certificate path is required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect TLS file %s: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("TLS file %s must be a regular non-symlink file", path)
	}
	if before.Size() > maxDaemonTLSFileBytes {
		return nil, fmt.Errorf("TLS file %s exceeds %d bytes", path, maxDaemonTLSFileBytes)
	}
	if private && before.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private key %s must not be accessible by group or others (use chmod 0400 or 0600)", path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open TLS file without following symlinks %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open TLS file %s: invalid file descriptor", path)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat TLS file %s: %w", path, err)
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("TLS file %s changed while opening", path)
	}
	if private && after.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private key %s must not be accessible by group or others (use chmod 0400 or 0600)", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDaemonTLSFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read TLS file %s: %w", path, err)
	}
	if len(data) > maxDaemonTLSFileBytes {
		return nil, fmt.Errorf("TLS file %s exceeds %d bytes", path, maxDaemonTLSFileBytes)
	}
	return data, nil
}

func loadDaemonClientCAs(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	remaining := data
	count := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			return nil, fmt.Errorf("CA bundle contains invalid PEM data")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("CA bundle contains non-certificate PEM block %q", block.Type)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA certificate: %w", err)
		}
		if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, fmt.Errorf("certificate %q is not a CA permitted to sign client certificates", certificate.Subject.String())
		}
		now := time.Now()
		if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
			return nil, fmt.Errorf("CA certificate %q is outside its validity period", certificate.Subject.String())
		}
		pool.AddCert(certificate)
		count++
		remaining = rest
	}
	if count == 0 {
		return nil, fmt.Errorf("CA bundle contains no certificates")
	}
	return pool, nil
}

func loadDaemonTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	caPEM, err := readDaemonTLSFile(caPath, false)
	if err != nil {
		return nil, fmt.Errorf("read client CA certificate: %w", err)
	}
	clientCAs, err := loadDaemonClientCAs(caPEM)
	if err != nil {
		return nil, fmt.Errorf("load client CA certificate bundle %s: %w", caPath, err)
	}
	certPEM, err := readDaemonTLSFile(certPath, false)
	if err != nil {
		return nil, fmt.Errorf("read server certificate: %w", err)
	}
	keyPEM, err := readDaemonTLSFile(keyPath, true)
	if err != nil {
		return nil, fmt.Errorf("read server private key: %w", err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load server certificate and private key: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
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
		return "", fmt.Errorf("expected unix:///absolute/path; use tcp://HOST:PORT with --tlsverify for remote access")
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
