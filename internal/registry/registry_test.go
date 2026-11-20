package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"alpine", "registry-1.docker.io/library/alpine:latest"},
		{"alpine:3.20", "registry-1.docker.io/library/alpine:3.20"},
		{"team/app:v1", "registry-1.docker.io/team/app:v1"},
		{"ghcr.io/team/app:edge", "ghcr.io/team/app:edge"},
		{"localhost:5000/team/app:dev", "localhost:5000/team/app:dev"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseReference(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Canonical() != test.want {
				t.Fatalf("canonical = %q, want %q", got.Canonical(), test.want)
			}
		})
	}
}

func TestParseReferenceRejectsUnsafeInputs(t *testing.T) {
	for _, input := range []string{"", "../escape:latest", "team//app:latest", "team/app:", "team/app:tag with space", "https://registry/team/app:tag", "team/app@sha512:bad", "team/app@sha256:abc"} {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			if _, err := ParseReference(input); err == nil {
				t.Fatalf("ParseReference(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestDigestPullMapsToStableLocalTag(t *testing.T) {
	ref, err := ParseReference("ghcr.io/team/boxr@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/team/boxr:sha256-" + strings.Repeat("a", 64)
	if got := ref.LocalReference(); got != want {
		t.Fatalf("local reference = %q, want %q", got, want)
	}
}

func TestPullUsesBearerTokenAndSelectsRequestedPlatform(t *testing.T) {
	layer, diffID := testLayer(t)
	configData := testConfig(t, "arm64", diffID, RuntimeConfig{
		Env:        []string{"PATH=/usr/bin:/bin", "APP_MODE=production"},
		Entrypoint: []string{"/usr/local/bin/server"},
		Cmd:        []string{"--listen", ":8080"},
		WorkingDir: "/srv/app",
		Labels:     map[string]string{"org.boxr.component": "api"},
	})
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: boxrLayerMediaType, Digest: digestBytes(layer), Size: int64(len(layer))}
	manifestData, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	manifestDigest := digestBytes(manifestData)
	indexData, _ := json.Marshal(index{SchemaVersion: 2, Manifests: []struct {
		Descriptor
		Platform Platform `json:"platform"`
	}{
		{Descriptor: Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: digestBytes([]byte("wrong")), Size: 5}, Platform: Platform{OS: "linux", Architecture: "amd64"}},
		{Descriptor: Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest, Size: int64(len(manifestData))}, Platform: Platform{OS: "linux", Architecture: "arm64"}},
	}})
	indexDigest := digestBytes(indexData)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.URL.Query().Get("scope") != "repository:team/boxr:pull" {
				t.Errorf("token scope = %q", r.URL.Query().Get("scope"))
			}
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="boxr-test"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/team/boxr/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			_, _ = w.Write(indexData)
		case r.URL.Path == "/v2/team/boxr/manifests/"+manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestData)
		case r.URL.Path == "/v2/team/boxr/blobs/"+configDescriptor.Digest:
			w.Header().Set("Content-Type", configDescriptor.MediaType)
			_, _ = w.Write(configData)
		case r.URL.Path == "/v2/team/boxr/blobs/"+layerDescriptor.Digest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/boxr:latest"
	artifact, err := Pull(context.Background(), ref, "linux/arm64", Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Cleanup()
	if artifact.Architecture != "arm64" || artifact.OS != "linux" {
		t.Fatalf("platform = %s/%s", artifact.OS, artifact.Architecture)
	}
	if artifact.ManifestDigest != manifestDigest || artifact.LayerDigest != layerDescriptor.Digest || artifact.DiffID != diffID {
		t.Fatalf("unexpected image digests: %#v", artifact)
	}
	if got := artifact.RuntimeConfig; len(got.Env) != 2 || got.Env[1] != "APP_MODE=production" || len(got.Entrypoint) != 1 || got.Entrypoint[0] != "/usr/local/bin/server" || got.WorkingDir != "/srv/app" || got.Labels["org.boxr.component"] != "api" {
		t.Fatalf("runtime config was not preserved by registry pull: %#v", got)
	}
	if artifact.Reference != strings.TrimSuffix(ref, ":latest")+":latest" {
		t.Fatalf("local reference = %q", artifact.Reference)
	}
	if info, err := os.Stat(artifact.LayerPath); err != nil || info.Size() != int64(len(layer)) {
		t.Fatalf("layer file info=%v err=%v", info, err)
	}
}

func TestPullRejectsOrdinaryOCIImageLayers(t *testing.T) {
	layer, diffID := testLayer(t)
	configData := testConfig(t, "amd64", diffID)
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: digestBytes(layer), Size: int64(len(layer))}
	manifestData, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	if _, err := Pull(context.Background(), ref, "linux/amd64", Credentials{}); err == nil || !strings.Contains(err.Error(), "Boxr rootfs layers only") {
		t.Fatalf("expected clear unsupported-layer error, got %v", err)
	}
}

func TestPullRejectsTamperedLayerDigest(t *testing.T) {
	layer, diffID := testLayer(t)
	configData := testConfig(t, "amd64", diffID)
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: boxrLayerMediaType, Digest: "sha256:" + strings.Repeat("0", 64), Size: int64(len(layer))}
	manifestData, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case strings.HasSuffix(r.URL.Path, configDescriptor.Digest):
			_, _ = w.Write(configData)
		case strings.HasSuffix(r.URL.Path, layerDescriptor.Digest):
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	if _, err := Pull(context.Background(), ref, "linux/amd64", Credentials{}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected layer digest mismatch, got %v", err)
	}
}

func TestParseAuthParameters(t *testing.T) {
	got := parseAuthParameters(`realm="https://auth.example/token",service="registry.example",scope="repository:team/app:pull,push"`)
	for key, want := range map[string]string{"realm": "https://auth.example/token", "service": "registry.example", "scope": "repository:team/app:pull,push"} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func testLayer(t *testing.T) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gz)
	content := []byte("boxr registry test rootfs\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "etc/boxr-release", Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	diffID := sha256.Sum256(plain)
	return buffer.Bytes(), "sha256:" + hex.EncodeToString(diffID[:])
}

func testConfig(t *testing.T, arch, diffID string, runtimeConfigs ...RuntimeConfig) []byte {
	t.Helper()
	config := imageConfig{OS: "linux", Architecture: arch}
	if len(runtimeConfigs) > 0 {
		config.Config = runtimeConfigs[0]
	}
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = []string{diffID}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestArtifactCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layer")
	if err := os.WriteFile(path, []byte("layer"), 0600); err != nil {
		t.Fatal(err)
	}
	artifact := Artifact{LayerPath: path}
	if err := artifact.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("artifact file still exists: %v", err)
	}
}

func TestPushUploadsVerifiedBlobsAndManifestWithBearerScope(t *testing.T) {
	layer, diffID := testLayer(t)
	layerPath := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	if err := os.WriteFile(layerPath, layer, 0600); err != nil {
		t.Fatal(err)
	}
	configData := testConfig(t, "amd64", diffID)
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: boxrLayerMediaType, Digest: digestBytes(layer), Size: int64(len(layer))}
	manifestData, err := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := digestBytes(manifestData)
	uploaded := map[string]bool{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if got := r.URL.Query().Get("scope"); got != "repository:team/app:pull,push" {
				t.Errorf("token scope = %q", got)
			}
			_, _ = w.Write([]byte(`{"access_token":"push-token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer push-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test-registry",scope="repository:team/app:pull"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/team/app/blobs/uploads/":
			w.Header().Set("Location", "/v2/team/app/blobs/uploads/session?upload=1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/team/app/blobs/uploads/session":
			digest := r.URL.Query().Get("digest")
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read blob upload: %v", readErr)
			}
			if got := digestBytes(body); got != digest {
				t.Errorf("uploaded blob digest = %q, query = %q", got, digest)
			}
			uploaded[digest] = true
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/team/app/manifests/v1":
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read manifest upload: %v", readErr)
			}
			if got := digestBytes(body); got != manifestDigest {
				t.Errorf("manifest digest = %q, want %q", got, manifestDigest)
			}
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:v1"
	if err := Push(context.Background(), ref, PushBundle{Manifest: manifestData, Config: configData, LayerPath: layerPath}, Credentials{}); err != nil {
		t.Fatal(err)
	}
	if !uploaded[configDescriptor.Digest] || !uploaded[layerDescriptor.Digest] {
		t.Fatalf("not all image blobs were uploaded: %#v", uploaded)
	}
}

func TestPushRejectsCrossOriginUploadLocation(t *testing.T) {
	layer, diffID := testLayer(t)
	layerPath := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	if err := os.WriteFile(layerPath, layer, 0600); err != nil {
		t.Fatal(err)
	}
	configData := testConfig(t, "amd64", diffID)
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: boxrLayerMediaType, Digest: digestBytes(layer), Size: int64(len(layer))}
	manifestData, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/") {
			w.Header().Set("Location", "https://attacker.invalid/upload/session")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:v1"
	err := Push(context.Background(), ref, PushBundle{Manifest: manifestData, Config: configData, LayerPath: layerPath}, Credentials{})
	if err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("expected cross-origin upload rejection, got %v", err)
	}
}

func TestPushRejectsDigestTargetAndTamperedLayer(t *testing.T) {
	layer, diffID := testLayer(t)
	layerPath := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	if err := os.WriteFile(layerPath, layer, 0600); err != nil {
		t.Fatal(err)
	}
	configData := testConfig(t, "amd64", diffID)
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: boxrLayerMediaType, Digest: digestBytes(layer), Size: int64(len(layer))}
	manifestData, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	bundle := PushBundle{Manifest: manifestData, Config: configData, LayerPath: layerPath}
	if err := Push(context.Background(), "localhost:5000/team/app@sha256:"+strings.Repeat("a", 64), bundle, Credentials{}); err == nil || !strings.Contains(err.Error(), "requires a tag") {
		t.Fatalf("expected digest target rejection, got %v", err)
	}
	if err := os.WriteFile(layerPath, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), "localhost:5000/team/app:v1", bundle, Credentials{}); err == nil || !strings.Contains(err.Error(), "descriptor") {
		t.Fatalf("expected layer descriptor rejection, got %v", err)
	}
}

func TestParseReferenceValidatesRegistryPorts(t *testing.T) {
	for _, input := range []string{"localhost:0/team/app:latest", "localhost:65536/team/app:latest", "..:5000/team/app:latest", "-bad.example/team/app:latest"} {
		if _, err := ParseReference(input); err == nil {
			t.Errorf("unsafe registry reference %q unexpectedly accepted", input)
		}
	}
	if _, err := ParseReference("localhost:65535/team/app:latest"); err != nil {
		t.Fatalf("valid maximum port rejected: %v", err)
	}
}

func TestBearerTokenRefusesUntrustedCredentialRealm(t *testing.T) {
	client := registryClient{
		http:        &http.Client{Timeout: time.Second},
		baseURL:     "https://registry.example",
		repository:  "team/app",
		credentials: Credentials{Username: "user", Password: "secret"},
	}
	_, err := client.bearerToken(context.Background(), `Bearer realm="https://attacker.invalid/token",service="registry.example"`)
	if err == nil || !strings.Contains(err.Error(), "untrusted token host") {
		t.Fatalf("expected credential host rejection, got %v", err)
	}
}

func TestBearerTokenRejectsInsecureRealmForHTTPSRegistry(t *testing.T) {
	client := registryClient{http: &http.Client{Timeout: time.Second}, baseURL: "https://registry.example", repository: "team/app"}
	_, err := client.bearerToken(context.Background(), `Bearer realm="http://auth.example/token"`)
	if err == nil || !strings.Contains(err.Error(), "insecure HTTP token realm") {
		t.Fatalf("expected insecure realm rejection, got %v", err)
	}
}
