package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type dockerTestEntry struct {
	name string
	kind byte
	data []byte
	link string
}

func dockerTestLayer(t *testing.T, entries ...dockerTestEntry) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Typeflag: kind, Mode: 0644, Size: int64(len(entry.data)), Linkname: entry.link}
		if kind == tar.TypeDir {
			header.Mode = 0755
			header.Size = 0
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
	data := buffer.Bytes()
	sum := sha256.Sum256(data)
	return append([]byte(nil), data...), "sha256:" + hex.EncodeToString(sum[:])
}

func TestApplyLayerArchiveSupportsZstandard(t *testing.T) {
	plain, _ := dockerTestLayer(t, dockerTestEntry{name: "usr/share/boxr-zstd", data: []byte("zstandard layer\n")})
	var encoded bytes.Buffer
	writer, err := zstd.NewWriter(&encoded, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	archive := filepath.Join(t.TempDir(), "layer.tar.zst")
	if err := os.WriteFile(archive, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := ApplyLayerArchive(root, archive); err != nil {
		t.Fatalf("apply Zstandard layer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "usr/share/boxr-zstd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "zstandard layer\n" {
		t.Fatalf("layer content = %q", got)
	}
}

func writeDockerFixture(t *testing.T, pathname string, compressOuter, corruptDiff bool, firstLayerPath string) {
	t.Helper()
	layerOne, diffOne := dockerTestLayer(t,
		dockerTestEntry{name: "etc/motd", data: []byte("old message\n")},
		dockerTestEntry{name: "opt/app", data: []byte("application\n")},
		dockerTestEntry{name: "var/cache/old", data: []byte("old cache\n")},
	)
	layerTwo, diffTwo := dockerTestLayer(t,
		dockerTestEntry{name: "etc/.wh.motd"},
		dockerTestEntry{name: "etc/new", data: []byte("new message\n")},
		dockerTestEntry{name: "var/cache/.wh..wh..opq"},
		dockerTestEntry{name: "var/cache/new", data: []byte("new cache\n")},
	)
	if firstLayerPath != "" {
		layerOne, diffOne = dockerTestLayer(t, dockerTestEntry{name: firstLayerPath, data: []byte("bad path\n")})
	}
	if corruptDiff {
		diffTwo = "sha256:" + strings.Repeat("0", 64)
	}
	config, err := json.Marshal(map[string]any{
		"created":      "2026-11-20T00:00:00Z",
		"architecture": "amd64",
		"os":           "linux",
		"config": ImageRuntimeConfig{
			Env:        []string{"APP_ENV=production"},
			Entrypoint: []string{"/usr/bin/app"},
			Cmd:        []string{"--serve"},
			WorkingDir: "/srv/app",
			Labels:     map[string]string{"org.example.name": "demo"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{diffOne, diffTwo}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal([]dockerManifestEntry{{
		Config:   "config.json",
		RepoTags: []string{"demo:latest", "demo:stable"},
		Layers:   []string{"layer-one/layer.tar", "layer-two/layer.tar"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	file, err := os.Create(pathname)
	if err != nil {
		t.Fatal(err)
	}
	var output = file
	var compressed *gzip.Writer
	if compressOuter {
		compressed = gzip.NewWriter(file)
		output = nil
	}
	var tarOutput *tar.Writer
	if compressed != nil {
		tarOutput = tar.NewWriter(compressed)
	} else {
		tarOutput = tar.NewWriter(output)
	}
	writeFile := func(name string, data []byte) {
		t.Helper()
		if err := tarOutput.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarOutput.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("manifest.json", manifest)
	writeFile("config.json", config)
	writeFile("layer-one/layer.tar", layerOne)
	writeFile("layer-two/layer.tar", layerTwo)
	if err := tarOutput.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed != nil {
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDockerSaveArchiveAppliesLayersAndWhiteouts(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "plain-tar"
		if compressed {
			name = "gzip-tar"
		}
		t.Run(name, func(t *testing.T) {
			useStore(t)
			archive := filepath.Join(t.TempDir(), "docker-save.tar")
			writeDockerFixture(t, archive, compressed, false, "")
			loaded, err := Load(archive)
			if err != nil {
				t.Fatalf("Load Docker save archive: %v", err)
			}
			if len(loaded) != 2 || loaded[0].Reference != "demo:latest" || loaded[1].Reference != "demo:stable" {
				t.Fatalf("unexpected imported tags: %+v", loaded)
			}
			inspected, err := Inspect("demo:latest")
			if err != nil {
				t.Fatal(err)
			}
			if inspected.Arch != "amd64" || inspected.OS != "linux" ||
				len(inspected.RuntimeConfig.Cmd) != 1 || inspected.RuntimeConfig.Cmd[0] != "--serve" ||
				inspected.RuntimeConfig.WorkingDir != "/srv/app" {
				t.Fatalf("runtime metadata was not preserved: %+v", inspected)
			}

			destination := filepath.Join(t.TempDir(), "rootfs")
			if _, err := Unpack("demo:latest", destination); err != nil {
				t.Fatal(err)
			}
			for _, removed := range []string{"etc/motd", "var/cache/old"} {
				if _, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(removed))); !os.IsNotExist(err) {
					t.Errorf("whiteout did not remove %s (error %v)", removed, err)
				}
			}
			for name, expected := range map[string]string{
				"etc/new":       "new message\n",
				"opt/app":       "application\n",
				"var/cache/new": "new cache\n",
			} {
				actual, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
				if err != nil {
					t.Errorf("read %s: %v", name, err)
					continue
				}
				if string(actual) != expected {
					t.Errorf("%s = %q, want %q", name, actual, expected)
				}
			}
		})
	}
}

func TestLoadDockerSaveRejectsLayerTraversalBeforePublishing(t *testing.T) {
	useStore(t)
	archive := filepath.Join(t.TempDir(), "malicious.tar")
	writeDockerFixture(t, archive, false, false, "../escape")
	if _, err := Load(archive); err == nil {
		t.Fatal("layer path traversal was accepted")
	}
	items, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("failed import published image tags: %+v", items)
	}
}

func TestLoadDockerSaveRejectsDiffIDMismatchBeforePublishing(t *testing.T) {
	useStore(t)
	archive := filepath.Join(t.TempDir(), "corrupt.tar")
	writeDockerFixture(t, archive, false, true, "")
	if _, err := Load(archive); err == nil || !strings.Contains(err.Error(), "diff_id mismatch") {
		t.Fatalf("expected diff_id mismatch, got %v", err)
	}
	items, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("corrupt import published image tags: %+v", items)
	}
}

func TestApplyLayerArchiveDoesNotFollowExistingSymlinkParents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside stays untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "pivot")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	layer, _ := dockerTestLayer(t, dockerTestEntry{
		name: "pivot/payload",
		data: []byte("confined to rootfs"),
	})
	archive := filepath.Join(t.TempDir(), "layer.tar")
	if err := os.WriteFile(archive, layer, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayerArchive(root, archive); err != nil {
		t.Fatalf("apply layer with a symlink parent: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside stays untouched" {
		t.Fatalf("symlink parent changed an external file: %q, %v", data, err)
	}
	info, err := os.Lstat(filepath.Join(root, "pivot"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("symlink parent was not safely replaced by a directory: info=%v err=%v", info, err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "pivot", "payload"))
	if err != nil || string(payload) != "confined to rootfs" {
		t.Fatalf("payload was not created inside rootfs: %q, %v", payload, err)
	}
}

func TestExtractDockerArchiveRejectsSymlinksAndTraversal(t *testing.T) {
	for _, entry := range []dockerTestEntry{
		{name: "../escape", data: []byte("no")},
		{name: "link", kind: tar.TypeSymlink, link: "../../outside"},
	} {
		archive := filepath.Join(t.TempDir(), "unsafe.tar")
		file, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		if err := writer.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0600, Size: 2}); err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte("[]"))
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0600, Size: int64(len(entry.data)), Linkname: entry.link}
		if entry.kind == 0 {
			header.Typeflag = tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.data) > 0 {
			_, _ = writer.Write(entry.data)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := extractDockerArchive(archive, t.TempDir()); err == nil {
			t.Errorf("unsafe outer archive entry %q was accepted", entry.name)
		}
	}
}
