package state

// Stats is the platform-neutral shape of a container resource snapshot.
// Linux populates cgroup v2 counters; non-Linux builds keep those fields empty
// and return the platform-specific unsupported-runtime error from ReadStats.
type NetworkCounters struct {
	RxBytes   int64
	RxPackets int64
	RxErrors  int64
	RxDropped int64
	TxBytes   int64
	TxPackets int64
	TxErrors  int64
	TxDropped int64
}

type Stats struct {
	PID                int                  `json:"pid"`
	MemoryCurrent      int64                `json:"memoryCurrentBytes,omitempty"`
	MemoryMax          string               `json:"memoryMax,omitempty"`
	MemoryStats        map[string]int64     `json:"memoryStats,omitempty"`
	PIDsCurrent        int64                `json:"pidsCurrent,omitempty"`
	PIDsMax            int64                `json:"pidsMax,omitempty"`
	CPUUsageUsec       int64                `json:"cpuUsageUsec,omitempty"`
	CPUUserUsec        int64                `json:"cpuUserUsec,omitempty"`
	CPUSystemUsec      int64                `json:"cpuSystemUsec,omitempty"`
	CPUPeriods         int64                `json:"cpuPeriods,omitempty"`
	CPUThrottledPeriods int64               `json:"cpuThrottledPeriods,omitempty"`
	CPUThrottledUsec   int64                `json:"cpuThrottledUsec,omitempty"`
	SystemCPUUsageNS   int64                `json:"systemCpuUsageNs,omitempty"`
	SystemMemoryTotalBytes int64             `json:"systemMemoryTotalBytes,omitempty"`
	IOCounters         map[string]string    `json:"ioCounters,omitempty"`
	Networks           map[string]NetworkCounters `json:"networks,omitempty"`
	AvailableFiles     []string             `json:"availableFiles,omitempty"`
}
