package registry

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveCredentialsDockerInlineAuthAndLegacyHubAlias(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:secret:with:colons"))
	path := writeAuthFile(t, `{"auths":{"https://index.docker.io/v1/":{"auth":"`+auth+`"}}}`)
	credentials, err := ResolveCredentials("alpine:3.21", path, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "alice" || credentials.Password != "secret:with:colons" {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
}

func TestResolveCredentialsDockerAndPodmanUsernamePassword(t *testing.T) {
	for _, registryHost := range []string{"ghcr.io", "quay.io:5000"} {
		t.Run(registryHost, func(t *testing.T) {
			path := writeAuthFile(t, `{"auths":{"`+registryHost+`":{"username":"robot","password":"token"}}}`)
			ref := registryHost + "/team/image:v1"
			credentials, err := ResolveCredentials(ref, path, Credentials{})
			if err != nil {
				t.Fatal(err)
			}
			if credentials != (Credentials{Username: "robot", Password: "token"}) {
				t.Fatalf("unexpected credentials: %#v", credentials)
			}
		})
	}
}

func TestResolveCredentialsEnvironmentTakesPrecedence(t *testing.T) {
	credentials, err := ResolveCredentials("ghcr.io/team/image:v1", filepath.Join(t.TempDir(), "missing.json"), Credentials{Username: "env-user", Password: "env-token"})
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "env-user" || credentials.Password != "env-token" {
		t.Fatalf("environment credentials were not preferred: %#v", credentials)
	}
}

func TestResolveCredentialsRejectsAuthFileSymlink(t *testing.T) {
	path := writeAuthFile(t, `{"auths":{}}`)
	link := filepath.Join(t.TempDir(), "auth-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ResolveCredentials("ghcr.io/team/image:v1", link, Credentials{}); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("expected auth file symlink rejection, got %v", err)
	}
}

func TestResolveCredentialsRejectsOversizedAuthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxAuthFileBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCredentials("ghcr.io/team/image:v1", path, Credentials{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected auth file size rejection, got %v", err)
	}
}

func TestResolveCredentialsDoesNotExecuteExternalHelpers(t *testing.T) {
	path := writeAuthFile(t, `{"auths":{},"credHelpers":{"ghcr.io":"test-helper"}}`)
	if _, err := ResolveCredentials("ghcr.io/team/image:v1", path, Credentials{}); err == nil || !strings.Contains(err.Error(), "does not execute credential helpers") {
		t.Fatalf("expected safe credential-helper refusal, got %v", err)
	}
}

func TestResolveCredentialsRejectsInvalidInlineAuth(t *testing.T) {
	path := writeAuthFile(t, `{"auths":{"ghcr.io":{"auth":"not-base64"}}}`)
	if _, err := ResolveCredentials("ghcr.io/team/image:v1", path, Credentials{}); err == nil || !strings.Contains(err.Error(), "invalid base64") {
		t.Fatalf("expected invalid auth rejection, got %v", err)
	}
}

func TestResolveCredentialsFromDockerConfigEnvironment(t *testing.T) {
	dockerDir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("docker-user:docker-token"))
	if err := os.WriteFile(filepath.Join(dockerDir, "config.json"), []byte(`{"auths":{"ghcr.io":{"auth":"`+auth+`"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dockerDir)
	t.Setenv("REGISTRY_AUTH_FILE", "")
	credentials, err := ResolveCredentials("ghcr.io/team/image:v1", "", Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if credentials != (Credentials{Username: "docker-user", Password: "docker-token"}) {
		t.Fatalf("unexpected Docker config credentials: %#v", credentials)
	}
}

func TestResolveCredentialsFromPodmanAuthFile(t *testing.T) {
	home := t.TempDir()
	podmanDir := filepath.Join(home, ".config", "containers")
	if err := os.MkdirAll(podmanDir, 0700); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("podman-user:podman-token"))
	if err := os.WriteFile(filepath.Join(podmanDir, "auth.json"), []byte(`{"auths":{"quay.io":{"auth":"`+auth+`"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("REGISTRY_AUTH_FILE", "")
	credentials, err := ResolveCredentials("quay.io/team/image:v1", "", Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if credentials != (Credentials{Username: "podman-user", Password: "podman-token"}) {
		t.Fatalf("unexpected Podman auth credentials: %#v", credentials)
	}
}

func TestResolveCredentialsFromRegistryAuthFileEnvironment(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("registry-user:registry-token"))
	path := writeAuthFile(t, `{"auths":{"localhost:5000":{"auth":"`+auth+`"}}}`)
	t.Setenv("REGISTRY_AUTH_FILE", path)
	t.Setenv("DOCKER_CONFIG", "")
	credentials, err := ResolveCredentials("localhost:5000/team/image:v1", "", Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if credentials != (Credentials{Username: "registry-user", Password: "registry-token"}) {
		t.Fatalf("unexpected registry auth credentials: %#v", credentials)
	}
}

func TestResolvedAuthFileCredentialsCompleteBearerChallenge(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("robot:secret-token"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			username, password, ok := r.BasicAuth()
			if !ok || username != "robot" || password != "secret-token" {
				t.Errorf("token endpoint received incorrect Basic auth: %q %q %v", username, password, ok)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"token":"auth-file-token"}`)
		case "/resource":
			if r.Header.Get("Authorization") != "Bearer auth-file-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+"http://"+r.Host+`/token",service="test",scope="repository:team/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registryHost := strings.TrimPrefix(server.URL, "http://")
	path := writeAuthFile(t, `{"auths":{"`+registryHost+`":{"auth":"`+auth+`"}}}`)
	ref := registryHost + "/team/app:v1"
	credentials, err := ResolveCredentials(ref, path, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	fetcher := registryClient{
		http:        server.Client(),
		baseURL:     server.URL,
		repository:  "team/app",
		credentials: credentials,
	}
	data, _, _, err := fetcher.get(context.Background(), server.URL+"/resource", "text/plain", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected authenticated response %q", data)
	}
}
