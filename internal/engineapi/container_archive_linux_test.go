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
		{input: "relative/path", want: "relative/path"},
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

func TestDockerArchivePathFlags(t *testing.T) {
	tests := []struct {
		input          string
		wantPath       string
		contentsOnly   bool
		directoryExpected bool
	}{
		{input: "etc/passwd", wantPath: "etc/passwd"},
		{input: "/etc/", wantPath: "etc", directoryExpected: true},
		{input: "/etc/.", wantPath: "etc", contentsOnly: true, directoryExpected: true},
		{input: "/.", wantPath: "", contentsOnly: true, directoryExpected: true},
	}
	for _, test := range tests {
		gotPath, contentsOnly, directoryExpected, err := dockerArchivePathFlags(test.input)
		if err != nil {
			t.Fatalf("dockerArchivePathFlags(%q): %v", test.input, err)
		}
		if gotPath != test.wantPath || contentsOnly != test.contentsOnly || directoryExpected != test.directoryExpected {
			t.Errorf("dockerArchivePathFlags(%q) = (%q,%v,%v), want (%q,%v,%v)",
				test.input, gotPath, contentsOnly, directoryExpected,
				test.wantPath, test.contentsOnly, test.directoryExpected)
		}
	}
}

func TestDockerArchiveResolveFollowsInRootSymlinks(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lower, "real", "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lower, "real", "nested", "data.txt"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(lower, "relative-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/real", filepath.Join(lower, "absolute-link")); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"relative-link/nested/data.txt", "absolute-link/nested/data.txt"} {
		lowerPath, _, info, err := resolveDockerOverlayPath(lower, upper, query)
		if err != nil {
			t.Fatalf("resolve %q: %v", query, err)
		}
		if !info.Mode().IsRegular() || filepath.Clean(lowerPath) != filepath.Join(lower, "real", "nested", "data.txt") {
			t.Fatalf("resolve %q = (%q, %v)", query, lowerPath, info.Mode())
		}
	}
}

func TestDockerArchiveResolveRejectsEscapingAndLoopingSymlinks(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	if err := os.Symlink("../../host-secret", filepath.Join(lower, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop-b", filepath.Join(lower, "loop-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop-a", filepath.Join(lower, "loop-b")); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"escape", "loop-a"} {
		if _, _, _, err := resolveDockerOverlayPath(lower, upper, query); err == nil {
			t.Errorf("resolve %q unexpectedly succeeded", query)
		}
	}
}

func TestDockerArchiveSymlinkTargetsCannotBypassLiveMountExclusions(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()
	if err := os.Mkdir(filepath.Join(lower, "proc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lower, "proc", "secret"), []byte("not mounted in this test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc", filepath.Join(lower, "proc-alias")); err != nil {
		t.Fatal(err)
	}
	resolvedLower, _, _, err := resolveDockerOverlayPath(lower, upper, "proc-alias/secret")
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(lower, resolvedLower)
	if err != nil {
		t.Fatal(err)
	}
	if !dockerArchivePathUsesRestrictedMount(filepath.ToSlash(relative)) {
		t.Fatalf("symlink resolved to restricted mount path %q without being blocked", relative)
	}
}

func TestDockerArchiveSingleRootPreservesLiteralWhiteoutFilenames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "literal"), []byte("visible lower name"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".wh.literal"), []byte("visible whiteout-like name"), 0600); err != nil {
		t.Fatal(err)
	}
	lowerPath, upperPath, info, err := resolveDockerOverlayPath(root, root, "literal")
	if err != nil {
		t.Fatal(err)
	}
	if lowerPath != filepath.Join(root, "literal") || upperPath != lowerPath || !info.Mode().IsRegular() {
		t.Fatalf("single-root resolution unexpectedly treated a literal .wh file as overlay metadata: lower=%q upper=%q mode=%v", lowerPath, upperPath, info.Mode())
	}
}

func TestDockerArchiveNoOverwriteRejectsDirectoryTypeMismatch(t *testing.T) {
	stage := t.TempDir()
	lower := t.TempDir()
	upper := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "existing-dir"), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(lower, "existing-dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerArchiveNoOverwrite(stage, lower, upper, ""); err == nil {
		t.Fatal("expected file-over-directory overwrite to be rejected")
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

func TestMergeDockerArchiveTreeVerifiesProcessAfterOpeningRoot(t *testing.T) {
	stage := t.TempDir()
	upper := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "payload"), []byte("must not be written"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyErr := errors.New("container PID identity changed")
	err := mergeDockerArchiveTreeVerified(stage, upper, "", func() error { return verifyErr })
	if !errors.Is(err, verifyErr) {
		t.Fatalf("merge error = %v, want process verification error", err)
	}
	if _, err := os.Stat(filepath.Join(upper, "payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write happened before process identity verification: %v", err)
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
