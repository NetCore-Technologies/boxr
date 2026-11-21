//go:build linux

package engineapi

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanDockerArchivePath(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "/", want: ""},
		{input: "/var/lib/app", want: "var/lib/app"},
		{input: "/var//lib/./app", want: "var/lib/app"},
		{input: "relative/path", wantErr: true},
		{input: "/var/../etc", wantErr: true},
		{input: "/var/file\x00", wantErr: true},
		{input: "/var/file\nname", wantErr: true},
		{input: "/var\\file", wantErr: true},
	}
	for _, test := range tests {
		got, err := cleanDockerArchivePath(test.input)
		if (err != nil) != test.wantErr {
			t.Fatalf("cleanDockerArchivePath(%q) error = %v, wantErr %v", test.input, err, test.wantErr)
		}
		if err == nil && got != test.want {
			t.Errorf("cleanDockerArchivePath(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestDockerOverlayArchiveMergesUpperAndHonoursWhiteouts(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	for name, data := range map[string]string{
		"base.txt": "base",
		"replace.txt": "old",
		"removed.txt": "removed",
	} {
		if err := os.WriteFile(filepath.Join(lower, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(upper, "replace.txt"), []byte("upper"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, ".wh.removed.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	children, err := dockerOverlayChildren(lower, upper)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := children["removed.txt"]; ok {
		t.Fatal("whiteouted lower entry appeared in merged archive")
	}
	if _, ok := children["base.txt"]; !ok {
		t.Fatal("unchanged lower entry missing from merged archive")
	}
	if children["replace.txt"].lowerPath == "" || children["replace.txt"].upperPath == "" {
		t.Fatal("overlaid entry did not retain both layer paths")
	}

	var archive bytes.Buffer
	limiter := &dockerArchiveLimitWriter{writer: &archive, remaining: maxDockerArchiveExpanded}
	writer := tar.NewWriter(limiter)
	visited := 0
	if err := addDockerArchiveNode(lower, upper, "etc", writer, &visited, 0, limiter); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	got := make(map[string]string)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			got[header.Name] = string(data)
		}
	}
	if got["etc/replace.txt"] != "upper" {
		t.Fatalf("upper file did not override lower file: %#v", got)
	}
	if got["etc/base.txt"] != "base" || got["etc/new.txt"] != "new" {
		t.Fatalf("merged archive omitted expected files: %#v", got)
	}
	if _, ok := got["etc/removed.txt"]; ok {
		t.Fatalf("archive included a whiteouted file: %#v", got)
	}
}

func TestDockerOverlayArchiveOpaqueDirectoryHidesLowerChildren(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	if err := os.WriteFile(filepath.Join(lower, "hidden.txt"), []byte("hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, ".wh..wh..opq"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	children, err := dockerOverlayChildren(lower, upper)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("opaque upper directory exposed lower entries: %#v", children)
	}
}

func TestMergeDockerArchiveTreeRemovesWhiteoutsAndPreservesSafeSymlinks(t *testing.T) {
	stage := t.TempDir()
	upper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stage, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "sub", "new.txt"), []byte("payload"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/new.txt", filepath.Join(stage, "current")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(upper, "app")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".wh.new.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := mergeDockerArchiveTree(stage, upper, "app"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "sub", "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("copied content = %q, want payload", data)
	}
	link, err := os.Readlink(filepath.Join(target, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "sub/new.txt" {
		t.Fatalf("copied symlink = %q", link)
	}
	if _, err := os.Lstat(filepath.Join(target, ".wh.new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("whiteout remains after adding file: %v", err)
	}
}

func TestMergeDockerArchiveTreeRejectsSymlinkParents(t *testing.T) {
	stage := t.TempDir()
	upper := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "payload"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(upper, "escape")); err != nil {
		t.Fatal(err)
	}
	err := mergeDockerArchiveTree(stage, upper, "escape/target")
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected symlink-parent rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "target", "payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive escaped writable layer: %v", err)
	}
}

func TestValidateDockerArchiveUploadRejectsOverlayWhiteoutNames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".wh.deleted"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerArchiveUploadTree(root); err == nil || !strings.Contains(err.Error(), "whiteout") {
		t.Fatalf("expected whiteout-name rejection, got %v", err)
	}
}
