package engineapi

import (
	"testing"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

func TestDockerStatsSnapshotUsesDistinctPreviousCounters(t *testing.T) {
	record := state.Record{
		ID: "0123456789ab", Name: "stats-api", Status: "running",
		Options: container.Options{PidsLimit: 25},
	}
	previous := &dockerStatsSample{
		read: time.Unix(100, 0).UTC(),
		stats: state.Stats{
			CPUUsageUsec: 100, CPUUserUsec: 70, CPUSystemUsec: 30,
			SystemCPUUsageNS: 500_000_000,
		},
	}
	current := state.Stats{
		CPUUsageUsec: 250, CPUUserUsec: 180, CPUSystemUsec: 70,
		SystemCPUUsageNS: 1_500_000_000,
		CPUPeriods:       1000, CPUThrottledPeriods: 42, CPUThrottledUsec: 900,
		MemoryCurrent: 4096, MemoryMax: "max", SystemMemoryTotalBytes: 1 << 30,
		MemoryStats: map[string]int64{
			"anon": 2048, "file": 1024, "max_usage": 8192, "oom": 1,
		},
		PIDsCurrent: 4, PIDsMax: 40,
		IOCounters: map[string]string{
			"8:0": "rbytes=4096 wbytes=2048 rios=4 wios=2",
		},
		Networks: map[string]state.NetworkCounters{
			"eth0": {RxBytes: 1000, RxPackets: 10, RxErrors: 1, RxDropped: 2, TxBytes: 2000, TxPackets: 20, TxErrors: 3, TxDropped: 4},
		},
	}

	payload, _, err := dockerStatsSnapshotFromStats(record, current, previous)
	if err != nil {
		t.Fatal(err)
	}
	cpu := payload["cpu_stats"].(map[string]any)
	preCPU := payload["precpu_stats"].(map[string]any)
	currentUsage := cpu["cpu_usage"].(map[string]any)
	previousUsage := preCPU["cpu_usage"].(map[string]any)
	if currentUsage["total_usage"] != int64(250_000) || previousUsage["total_usage"] != int64(100_000) {
		t.Fatalf("current/previous CPU usage = %#v / %#v", currentUsage, previousUsage)
	}
	if cpu["system_cpu_usage"] != int64(1_500_000_000) || preCPU["system_cpu_usage"] != int64(500_000_000) {
		t.Fatalf("current/previous system CPU = %#v / %#v", cpu["system_cpu_usage"], preCPU["system_cpu_usage"])
	}
	if payload["preread"] != previous.read.Format(time.RFC3339Nano) {
		t.Fatalf("preread = %#v", payload["preread"])
	}

	memory := payload["memory_stats"].(map[string]any)
	if memory["usage"] != int64(4096) || memory["max_usage"] != int64(8192) || memory["limit"] != int64(1<<30) {
		t.Fatalf("Docker memory stats = %#v", memory)
	}
	memoryDetail := memory["stats"].(map[string]int64)
	if memoryDetail["anon"] != 2048 || memoryDetail["oom"] != 1 {
		t.Fatalf("Docker detailed memory stats = %#v", memoryDetail)
	}
	pids := payload["pids_stats"].(map[string]any)
	if pids["current"] != int64(4) || pids["limit"] != int64(40) {
		t.Fatalf("Docker PID stats = %#v", pids)
	}
	throttling := cpu["throttling_data"].(map[string]any)
	if throttling["periods"] != int64(1000) || throttling["throttled_periods"] != int64(42) ||
		throttling["throttled_time"] != int64(900_000) {
		t.Fatalf("Docker CPU throttling stats = %#v", throttling)
	}
	networks := payload["networks"].(map[string]any)
	eth0 := networks["eth0"].(map[string]any)
	if eth0["rx_bytes"] != int64(1000) || eth0["tx_bytes"] != int64(2000) ||
		eth0["rx_dropped"] != int64(2) || eth0["tx_errors"] != int64(3) {
		t.Fatalf("Docker network stats = %#v", eth0)
	}
}

func findDockerStatsBlockIOEntry(entries []map[string]any, major, minor int64, operation string) (map[string]any, bool) {
	for _, entry := range entries {
		if entry["major"] == major && entry["minor"] == minor && entry["op"] == operation {
			return entry, true
		}
	}
	return nil, false
}

func TestDockerStatsBlockIOConvertsCgroupCounters(t *testing.T) {
	result := dockerStatsBlockIO(map[string]string{
		"8:0":     "rbytes=4096 wbytes=2048 rios=4 wios=2",
		"253:1":   "rbytes=0 wbytes=8192 rios=0 wios=8",
		"invalid": "rbytes=10 wbytes=20 rios=1 wios=2",
	})
	serviceBytes := result["io_service_bytes_recursive"].([]map[string]any)
	serviced := result["io_serviced_recursive"].([]map[string]any)
	if len(serviceBytes) != 4 || len(serviced) != 4 {
		t.Fatalf("Docker block IO entries: bytes=%#v serviced=%#v", serviceBytes, serviced)
	}
	readBytes, ok := findDockerStatsBlockIOEntry(serviceBytes, int64(8), int64(0), "Read")
	if !ok || readBytes["value"] != int64(4096) {
		t.Fatalf("Docker 8:0 read-byte entry = %#v, found=%t", readBytes, ok)
	}
	writeIO, ok := findDockerStatsBlockIOEntry(serviced, int64(8), int64(0), "Write")
	if !ok || writeIO["value"] != int64(2) {
		t.Fatalf("Docker 8:0 write-I/O entry = %#v, found=%t", writeIO, ok)
	}
	if serviceBytes[0]["major"] != int64(8) || serviceBytes[0]["minor"] != int64(0) {
		t.Fatalf("Docker block IO device ordering is not deterministic: %#v", serviceBytes)
	}
}
