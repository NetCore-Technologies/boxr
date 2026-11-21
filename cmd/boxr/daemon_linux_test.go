//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(stderr.String(), "--tlsverify") {
		t.Fatalf("daemon TCP rejection message = %q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runDaemon([]string{"--host", "tcp://127.0.0.1:2376", "--tlsverify"}, &stdout, &stderr); code != 2 {
		t.Fatalf("daemon TLS without certificate files returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--tlscacert") {
		t.Fatalf("daemon missing TLS file error = %q", stderr.String())
	}
}

func TestParseDaemonTCPHostRequiresExplicitValidAddress(t *testing.T) {
	valid := map[string]string{
		"tcp://127.0.0.1:2376": "127.0.0.1:2376",
		"tcp://localhost:2376": "localhost:2376",
		"tcp://[::1]:2376":     "[::1]:2376",
	}
	for input, expected := range valid {
		actual, err := parseDaemonTCPHost(input)
		if err != nil || actual != expected {
			t.Errorf("parseDaemonTCPHost(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	invalid := []string{
		"tcp://:2376",
		"tcp://127.0.0.1",
		"tcp://127.0.0.1:0",
		"tcp://127.0.0.1:65536",
		"tcp://127.0.0.1:abc",
		"tcp://127.0.0.1:2376/path",
		"tcp://127.0.0.1:2376?insecure=1",
		"tcp://user@127.0.0.1:2376",
		"unix:///tmp/engine.sock",
	}
	for _, input := range invalid {
		if actual, err := parseDaemonTCPHost(input); err == nil {
			t.Errorf("parseDaemonTCPHost(%q) unexpectedly returned %q", input, actual)
		}
	}
}

type daemonTestPKI struct {
	caPath         string
	serverCertPath string
	serverKeyPath  string
	clientCertPath string
	clientKeyPath  string
}

func createDaemonTestPKI(t *testing.T) daemonTestPKI {
	t.Helper()
	directory := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Boxr test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	createLeaf := func(serial int64, commonName string, usage x509.ExtKeyUsage, server bool) ([]byte, []byte) {
		t.Helper()
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: commonName},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		}
		if server {
			template.DNSNames = []string{"localhost"}
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, createErr := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		if createErr != nil {
			t.Fatal(createErr)
		}
		certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		privateKeyDER, marshalErr := x509.MarshalPKCS8PrivateKey(key)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
		return certificatePEM, privateKeyPEM
	}
	serverCert, serverKey := createLeaf(2, "boxr-server", x509.ExtKeyUsageServerAuth, true)
	clientCert, clientKey := createLeaf(3, "boxr-client", x509.ExtKeyUsageClientAuth, false)
	write := func(name string, content []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return daemonTestPKI{
		caPath:         write("ca.pem", caPEM, 0644),
		serverCertPath: write("server-cert.pem", serverCert, 0644),
		serverKeyPath:  write("server-key.pem", serverKey, 0600),
		clientCertPath: write("client-cert.pem", clientCert, 0644),
		clientKeyPath:  write("client-key.pem", clientKey, 0600),
	}
}

func TestDaemonMutualTLSRequiresTrustedClientCertificates(t *testing.T) {
	pki := createDaemonTestPKI(t)
	serverTLS, err := loadDaemonTLSConfig(pki.caPath, pki.serverCertPath, pki.serverKeyPath)
	if err != nil {
		t.Fatalf("load mutual TLS config: %v", err)
	}
	if serverTLS.ClientAuth != tls.RequireAndVerifyClientCert || serverTLS.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config is not strict mutual TLS: ClientAuth=%v MinVersion=%v", serverTLS.ClientAuth, serverTLS.MinVersion)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, "authenticated")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(tls.NewListener(listener, serverTLS))
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		<-serveDone
	})

	caPEM, err := os.ReadFile(pki.caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA certificate did not parse")
	}
	clientCertificate, err := tls.LoadX509KeyPair(pki.clientCertPath, pki.clientKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots,
		Certificates: []tls.Certificate{clientCertificate},
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Get("https://" + listener.Addr().String() + "/_ping")
	if err != nil {
		t.Fatalf("authenticated client request failed: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK || string(body) != "authenticated" {
		t.Fatalf("authenticated response status=%d body=%q", response.StatusCode, body)
	}

	unauthenticated := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	defer unauthenticated.CloseIdleConnections()
	if response, err := (&http.Client{Transport: unauthenticated}).Get("https://" + listener.Addr().String() + "/_ping"); err == nil {
		_ = response.Body.Close()
		t.Fatal("client without a certificate was accepted")
	}
}

func TestLoadDaemonTLSConfigRejectsInsecurePrivateKeyPermissions(t *testing.T) {
	pki := createDaemonTestPKI(t)
	if err := os.Chmod(pki.serverKeyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDaemonTLSConfig(pki.caPath, pki.serverCertPath, pki.serverKeyPath); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
		t.Fatalf("insecure private key was accepted or produced wrong error: %v", err)
	}
}
