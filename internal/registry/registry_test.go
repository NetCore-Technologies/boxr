package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	imageStore "github.com/manit6752025/boxr/internal/image"
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

func TestPullAcceptsMultiLayerOCIImageWithWhiteouts(t *testing.T) {
	firstLayer, firstDiffID := testRegistryLayer(t, true,
		registryLayerEntry{name: "etc/old", data: []byte("remove me\n")},
		registryLayerEntry{name: "opt/app", data: []byte("preserve me\n")},
		registryLayerEntry{name: "var/cache/old", data: []byte("remove cache\n")},
	)
	secondLayer, secondDiffID := testRegistryLayer(t, false,
		registryLayerEntry{name: "etc/.wh.old"},
		registryLayerEntry{name: "etc/new", data: []byte("new config\n")},
		registryLayerEntry{name: "var/cache/.wh..wh..opq"},
		registryLayerEntry{name: "var/cache/new", data: []byte("new cache\n")},
	)
	configData := testConfigWithDiffIDs(t, "amd64", []string{firstDiffID, secondDiffID}, RuntimeConfig{
		Entrypoint: []string{"/usr/bin/app"},
		WorkingDir: "/opt",
		Labels:     map[string]string{"org.example.component": "web"},
	})
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptors := []Descriptor{
		{MediaType: ociGzipLayerMediaType, Digest: digestBytes(firstLayer), Size: int64(len(firstLayer))},
		{MediaType: ociTarLayerMediaType, Digest: digestBytes(secondLayer), Size: int64(len(secondLayer))},
	}
	manifestData, err := json.Marshal(manifest{
		SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config: configDescriptor, Layers: layerDescriptors,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case "/v2/team/app/blobs/" + configDescriptor.Digest:
			w.Header().Set("Content-Type", configDescriptor.MediaType)
			_, _ = w.Write(configData)
		case "/v2/team/app/blobs/" + layerDescriptors[0].Digest:
			_, _ = w.Write(firstLayer)
		case "/v2/team/app/blobs/" + layerDescriptors[1].Digest:
			_, _ = w.Write(secondLayer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reference := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	artifact, err := Pull(context.Background(), reference, "linux/amd64", Credentials{})
	if err != nil {
		t.Fatalf("pull standard OCI image: %v", err)
	}
	defer artifact.Cleanup()
	if len(artifact.Layers) != 2 || artifact.LayerBytes != int64(len(firstLayer)+len(secondLayer)) {
		t.Fatalf("unexpected pulled layer set: %+v", artifact)
	}
	if artifact.Layers[0].MediaType != ociGzipLayerMediaType || artifact.Layers[1].MediaType != ociTarLayerMediaType {
		t.Fatalf("layer media types not preserved: %+v", artifact.Layers)
	}
	if artifact.RuntimeConfig.WorkingDir != "/opt" || artifact.RuntimeConfig.Labels["org.example.component"] != "web" {
		t.Fatalf("runtime config not preserved: %+v", artifact.RuntimeConfig)
	}
	root := t.TempDir()
	for index, layer := range artifact.Layers {
		if err := imageStore.ApplyLayerArchive(root, layer.Path); err != nil {
			t.Fatalf("apply layer %d: %v", index+1, err)
		}
	}
	for _, removed := range []string{"etc/old", "var/cache/old"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(removed))); !os.IsNotExist(err) {
			t.Errorf("whiteout did not remove %s (error %v)", removed, err)
		}
	}
	for name, want := range map[string]string{
		"opt/app":       "preserve me\n",
		"etc/new":       "new config\n",
		"var/cache/new": "new cache\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestPullAcceptsDockerDistributionLayerMediaTypes(t *testing.T) {
	for _, test := range []struct {
		name       string
		mediaType  string
		compressed bool
	}{
		{name: "gzip", mediaType: dockerGzipLayerMediaType, compressed: true},
		{name: "plain-tar", mediaType: dockerTarLayerMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			layer, diffID := testRegistryLayer(t, test.compressed, registryLayerEntry{name: "usr/bin/app", data: []byte("docker image layer\n")})
			configData := testConfig(t, "amd64", diffID)
			configDescriptor := Descriptor{MediaType: "application/vnd.docker.container.image.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
			layerDescriptor := Descriptor{MediaType: test.mediaType, Digest: digestBytes(layer), Size: int64(len(layer))}
			manifestData, err := json.Marshal(manifest{
				SchemaVersion: 2, MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Config: configDescriptor, Layers: []Descriptor{layerDescriptor},
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/team/app/manifests/latest":
					w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
					_, _ = w.Write(manifestData)
				case "/v2/team/app/blobs/" + configDescriptor.Digest:
					_, _ = w.Write(configData)
				case "/v2/team/app/blobs/" + layerDescriptor.Digest:
					_, _ = w.Write(layer)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
			artifact, err := Pull(context.Background(), ref, "linux/amd64", Credentials{})
			if err != nil {
				t.Fatalf("pull Docker layer: %v", err)
			}
			defer artifact.Cleanup()
			if len(artifact.Layers) != 1 || artifact.Layers[0].MediaType != test.mediaType {
				t.Fatalf("unexpected Docker layer artifact: %+v", artifact.Layers)
			}
			if artifact.LayerDigest != layerDescriptor.Digest || artifact.DiffID != diffID {
				t.Fatalf("single-layer compatibility metadata missing: %+v", artifact)
			}
		})
	}
}

func TestPullDownloadsLayersConcurrentlyAndPreservesManifestOrder(t *testing.T) {
	firstLayer, firstDiffID := testRegistryLayer(t, true, registryLayerEntry{name: "opt/first", data: []byte("first layer")})
	secondLayer, secondDiffID := testRegistryLayer(t, true, registryLayerEntry{name: "opt/second", data: []byte("second layer")})
	configData := testConfigWithDiffIDs(t, "amd64", []string{firstDiffID, secondDiffID}, RuntimeConfig{})
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptors := []Descriptor{
		{MediaType: ociGzipLayerMediaType, Digest: digestBytes(firstLayer), Size: int64(len(firstLayer))},
		{MediaType: ociGzipLayerMediaType, Digest: digestBytes(secondLayer), Size: int64(len(secondLayer))},
	}
	manifestData, err := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: layerDescriptors})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, len(layerDescriptors))
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case "/v2/team/app/blobs/" + configDescriptor.Digest:
			_, _ = w.Write(configData)
		case "/v2/team/app/blobs/" + layerDescriptors[0].Digest:
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(firstLayer)
		case "/v2/team/app/blobs/" + layerDescriptors[1].Digest:
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(secondLayer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	type pullResult struct {
		artifact Artifact
		err      error
	}
	done := make(chan pullResult, 1)
	reference := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	go func() {
		artifact, pullErr := Pull(context.Background(), reference, "linux/amd64", Credentials{})
		done <- pullResult{artifact: artifact, err: pullErr}
	}()

	startDeadline := time.NewTimer(2 * time.Second)
	defer startDeadline.Stop()
	for range layerDescriptors {
		select {
		case <-started:
		case <-startDeadline.C:
			close(release)
			select {
			case result := <-done:
				if result.err == nil {
					_ = result.artifact.Cleanup()
				}
			case <-time.After(5 * time.Second):
			}
			t.Fatal("registry layers were not downloaded concurrently")
		}
	}
	close(release)

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("parallel image pull failed: %v", result.err)
		}
		defer result.artifact.Cleanup()
		if len(result.artifact.Layers) != 2 {
			t.Fatalf("pulled %d layers, want 2", len(result.artifact.Layers))
		}
		for index, descriptor := range layerDescriptors {
			if result.artifact.Layers[index].Digest != descriptor.Digest {
				t.Errorf("layer %d digest = %q, want manifest digest %q", index, result.artifact.Layers[index].Digest, descriptor.Digest)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parallel image pull did not finish after releasing layer responses")
	}
}

func TestConcurrentRegistryRequestsShareBearerTokenRefresh(t *testing.T) {
	var tokenCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls.Add(1)
			time.Sleep(25 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"shared-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer shared-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="test",scope="repository:team/app:pull"`, server.URL+"/token"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	client := &registryClient{http: server.Client(), baseURL: server.URL, repository: "team/app"}
	const requestCount = 8
	var workers sync.WaitGroup
	results := make(chan error, requestCount)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for index := 0; index < requestCount; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/team/app/blob", nil)
			if err != nil {
				results <- err
				return
			}
			response, err := client.do(request)
			if err != nil {
				results <- err
				return
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				results <- readErr
				return
			}
			if closeErr != nil {
				results <- closeErr
				return
			}
			if response.StatusCode != http.StatusOK || string(body) != "ok" {
				results <- fmt.Errorf("unexpected registry response: status=%d body=%q", response.StatusCode, body)
				return
			}
			results <- nil
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times for concurrent requests, want exactly one refresh", got)
	}
}

func TestPullAcceptsOCIZstandardImageLayerAndVerifiesDiffID(t *testing.T) {
	plainLayer, diffID := testRegistryLayer(t, false, registryLayerEntry{name: "usr/share/boxr-zstd", data: []byte("pulled from zstandard\n")})
	var encodedLayer bytes.Buffer
	encoder, err := zstd.NewWriter(&encodedLayer, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(plainLayer); err != nil {
		t.Fatal(err)
	}
	encoder.Close()
	layer := encodedLayer.Bytes()

	configData := testConfig(t, "amd64", diffID)
	configDescriptor := Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestBytes(configData), Size: int64(len(configData))}
	layerDescriptor := Descriptor{MediaType: ociZstdLayerMediaType, Digest: digestBytes(layer), Size: int64(len(layer))}
	manifestData, err := json.Marshal(manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: configDescriptor, Layers: []Descriptor{layerDescriptor}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case "/v2/team/app/blobs/" + configDescriptor.Digest:
			_, _ = w.Write(configData)
		case "/v2/team/app/blobs/" + layerDescriptor.Digest:
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ref := strings.TrimPrefix(server.URL, "http://") + "/team/app:latest"
	artifact, err := Pull(context.Background(), ref, "linux/amd64", Credentials{})
	if err != nil {
		t.Fatalf("pull Zstandard image: %v", err)
	}
	defer artifact.Cleanup()
	if len(artifact.Layers) != 1 || artifact.Layers[0].DiffID != diffID {
		t.Fatalf("verified layers = %+v, want diff ID %q", artifact.Layers, diffID)
	}
	root := t.TempDir()
	if err := imageStore.ApplyLayerArchive(root, artifact.Layers[0].Path); err != nil {
		t.Fatalf("apply pulled Zstandard layer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "usr/share/boxr-zstd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pulled from zstandard\n" {
		t.Fatalf("pulled rootfs content = %q", got)
	}
}

type registryLayerEntry struct {
	name string
	data []byte
	kind byte
}

func testRegistryLayer(t *testing.T, compressed bool, entries ...registryLayerEntry) ([]byte, string) {
	t.Helper()
	var plain bytes.Buffer
	writer := tar.NewWriter(&plain)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0644,
			Typeflag: kind,
			Size:     int64(len(entry.data)),
			ModTime:  time.Unix(0, 0),
		}
		if kind == tar.TypeDir {
			header.Mode, header.Size = 0755, 0
		}
		if kind == tar.TypeSymlink || kind == tar.TypeLink {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.data) > 0 {
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	diffID := digestBytes(plain.Bytes())
	if !compressed {
		return plain.Bytes(), diffID
	}
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	if _, err := gz.Write(plain.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes(), diffID
}

func testConfigWithDiffIDs(t *testing.T, arch string, diffIDs []string, runtimeConfig RuntimeConfig) []byte {
	t.Helper()
	config := imageConfig{OS: "linux", Architecture: arch, Config: runtimeConfig}
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = append([]string(nil), diffIDs...)
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
	var portableManifest manifest
	if err := json.Unmarshal(manifestData, &portableManifest); err != nil {
		t.Fatal(err)
	}
	portableManifest.Layers[0].MediaType = ociGzipLayerMediaType
	portableManifestData, err := json.Marshal(portableManifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := digestBytes(portableManifestData)
	originalManifest := append([]byte(nil), manifestData...)
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
			var published manifest
			if err := json.Unmarshal(body, &published); err != nil {
				t.Errorf("decode published manifest: %v", err)
			} else if got := published.Layers[0].MediaType; got != ociGzipLayerMediaType {
				t.Errorf("published layer media type = %q, want %q", got, ociGzipLayerMediaType)
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
	if !bytes.Equal(manifestData, originalManifest) {
		t.Fatal("push mutated the local Boxr manifest")
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

func testLayer(t *testing.T) ([]byte, string) {
	t.Helper()
	return testRegistryLayer(t, true, registryLayerEntry{
		name: "etc/boxr-test",
		data: []byte("boxr registry layer fixture\n"),
	})
}

func testConfig(t *testing.T, arch, diffID string, runtimeConfigs ...RuntimeConfig) []byte {
	t.Helper()
	var runtimeConfig RuntimeConfig
	if len(runtimeConfigs) > 0 {
		runtimeConfig = runtimeConfigs[0]
	}
	return testConfigWithDiffIDs(t, arch, []string{diffID}, runtimeConfig)
}
