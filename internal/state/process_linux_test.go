//go:build linux

package state

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestParseProcessStartTicksHandlesParenthesesInCommandName(t *testing.T) {
	fields := make([]string, 20)
	fields[0] = "S"
	for index := 1; index < len(fields); index++ {
		fields[index] = "1"
	}
	fields[19] = "987654"
	stat := fmt.Sprintf("123 (worker ) with spaces)) %s", strings.Join(fields, " "))
	got, err := parseProcessStartTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if got != 987654 {
		t.Fatalf("start ticks = %d, want 987654", got)
	}
}

func TestVerifyProcessIdentityRejectsChangedStartTicks(t *testing.T) {
	ticks, err := ReadProcessStartTicks(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if ticks == 0 {
		t.Fatal("current process has zero start ticks")
	}
	record := Record{ID: "identity-test", PID: os.Getpid(), ProcessStartTicks: ticks + 1}
	if err := VerifyProcessIdentity(record); err == nil {
		t.Fatal("changed process start ticks were accepted")
	}
	record.ProcessStartTicks = ticks
	if err := VerifyProcessIdentity(record); err != nil {
		t.Fatalf("matching process identity rejected: %v", err)
	}
}

func TestVerifyProcessIdentityAcceptsLegacyRecordWithoutTicks(t *testing.T) {
	record := Record{ID: "legacy", PID: os.Getpid()}
	if err := VerifyProcessIdentity(record); err != nil {
		t.Fatalf("legacy record without start ticks was rejected: %v", err)
	}
}
