package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDockerfileVolumeInstructionRecordsAndValidatesTargets(t *testing.T) {
	spec, err := ParseDockerfile([]byte("FROM scratch\nVOLUME [\"/data\", \"/cache\"]\nVOLUME /logs\n"))
	if err != nil {
		t.Fatalf("parse Dockerfile VOLUME: %v", err)
	}
	for _, target := range []string{"/data", "/cache", "/logs"} {
		if _, ok := spec.Config.Volumes[target]; !ok {
			t.Errorf("Dockerfile VOLUME target %q was not retained: %+v", target, spec.Config.Volumes)
		}
	}

	for _, dockerfile := range []string{
		"FROM scratch\nVOLUME relative\n",
		"FROM scratch\nVOLUME /\n",
		"FROM scratch\nVOLUME /data/../host\n",
		"FROM scratch\nVOLUME []\n",
	} {
		if _, err := ParseDockerfile([]byte(dockerfile)); err == nil {
			t.Errorf("unsafe or empty VOLUME instruction accepted: %q", dockerfile)
		}
	}
}

func TestBuildRuntimeConfigInheritsDeclaredVolumes(t *testing.T) {
	base := ImageRuntimeConfig{Volumes: map[string]struct{}{"/data": {}}}
	overrides := ImageRuntimeConfig{Volumes: map[string]struct{}{"/cache": {}}}
	merged := inheritBuildRuntimeConfig(base, overrides)
	if len(merged.Volumes) != 2 {
		t.Fatalf("inherited volume declarations = %#v", merged.Volumes)
	}
	if _, ok := merged.Volumes["/data"]; !ok {
		t.Fatal("base image volume declaration was lost")
	}
	if _, ok := merged.Volumes["/cache"]; !ok {
		t.Fatal("Dockerfile volume declaration was not merged")
	}
}

func TestEnsureVolumeMountpointsCreatesDirectoriesAndRejectsEscapingSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := EnsureVolumeMountpoints(root, []string{"/var/lib/service/cache", "/data"}); err != nil {
		t.Fatalf("create volume mountpoints: %v", err)
	}
	for _, target := range []string{"/var/lib/service/cache", "/data"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(target)))
		if err != nil || !info.IsDir() {
			t.Fatalf("mountpoint %s: info=%v err=%v", target, info, err)
		}
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureVolumeMountpoints(root, []string{"/escape/volume"}); err == nil {
		t.Fatal("volume mountpoint followed a symlink outside the image root")
	}
}

func TestImportRootFSWithConfigCreatesDeclaredVolumeDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	_, err := ImportRootFSWithConfig(root, "registry.example/team/imported-volume-metadata:v1", "linux", "amd64", "", ImageRuntimeConfig{
		Cmd:     []string{"/bin/app"},
		Volumes: map[string]struct{}{"/var/lib/service/data": {}, "/cache": {}},
	})
	if err != nil {
		t.Fatalf("import image with declared volumes: %v", err)
	}
	for _, target := range []string{"/var/lib/service/data", "/cache"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(target)))
		if err != nil || !info.IsDir() {
			t.Fatalf("image volume directory %s: info=%v err=%v", target, info, err)
		}
	}
}
