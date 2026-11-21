package state

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/manit6752025/boxr/internal/container"
)

func TestContainerLogsTailFollowAndSecurePermissions(t *testing.T) {
	dir := useTempState(t)
	record, err := Create("logged", []string{"/bin/sh", "-c", "echo hello"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("one\ntwo\nthree\n"); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, record.ID+".log"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions: %v %v", info, err)
	}
	all, offset, err := ReadLogsSnapshot("logged", 0)
	if err != nil || string(all) != "one\ntwo\nthree\n" || offset != int64(len(all)) {
		t.Fatalf("ReadLogsSnapshot(all) = %q, %d, %v", all, offset, err)
	}
	tail, err := ReadLogs(record.ID[:6], 2)
	if err != nil || string(tail) != "two\nthree\n" {
		t.Fatalf("ReadLogs(tail) = %q, %v", tail, err)
	}
	var copied bytes.Buffer
	copiedOffset, err := CopyLogFrom(record.ID, 4, &copied)
	if err != nil || copied.String() != "two\nthree\n" || copiedOffset != int64(len(all)) {
		t.Fatalf("CopyLogFrom() = %d, %q, %v", copiedOffset, copied.String(), err)
	}
	if err := Start(record.ID, 12345); err != nil {
		t.Fatal(err)
	}
	if err := Finish(record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := Remove(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, record.ID+".log")); !os.IsNotExist(err) {
		t.Fatalf("log file remained after record removal: %v", err)
	}
}

func TestReadLogsRejectsNegativeTailAndHandlesMissingFile(t *testing.T) {
	useTempState(t)
	record, err := Create("empty-log", []string{"true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLogs(record.ID, -1); err == nil {
		t.Fatal("negative tail count accepted")
	}
	data, err := ReadLogs(record.ID, 100)
	if err != nil || len(data) != 0 {
		t.Fatalf("missing log = %q, %v", data, err)
	}
}

func TestLogOffsetAndCopyFollowLargeLogsWithoutUnboundedReads(t *testing.T) {
	dir := useTempState(t)
	record, err := Create("large-log", []string{"sleep", "1"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := OpenLog(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	largeSize := maxLogBytes + 4
	if err := logFile.Truncate(largeSize); err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.Seek(maxLogBytes, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	offset, err := LogOffset(record.ID)
	if err != nil || offset != largeSize {
		t.Fatalf("LogOffset() = %d, %v; want %d", offset, err, largeSize)
	}
	var copied bytes.Buffer
	next, err := CopyLogFrom(record.ID, maxLogBytes, &copied)
	if err != nil || next != largeSize || copied.String() != "tail" {
		t.Fatalf("CopyLogFrom large log = %d, %q, %v; want %d, tail", next, copied.String(), err, largeSize)
	}
	if _, err := os.Stat(filepath.Join(dir, record.ID+".log")); err != nil {
		t.Fatal(err)
	}
}

func TestContainerLogsRejectSymlinkTargets(t *testing.T) {
	dir := useTempState(t)
	record, err := Create("linked-log", []string{"true"}, container.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(external, []byte("do not change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, record.ID+".log")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLog(record.ID); err == nil {
		t.Fatal("OpenLog accepted a symlink target")
	}
	if _, _, err := ReadLogsSnapshot(record.ID, 0); err == nil {
		t.Fatal("ReadLogsSnapshot accepted a symlink target")
	}
	contents, err := os.ReadFile(external)
	if err != nil || string(contents) != "do not change\n" {
		t.Fatalf("external file changed: %q, %v", contents, err)
	}
}
