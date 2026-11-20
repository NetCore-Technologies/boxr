package state

// Stats is the platform-neutral shape of a container resource snapshot.
// Linux populates cgroup v2 counters; non-Linux builds keep those fields empty
// and return the platform-specific unsupported-runtime error from ReadStats.
type Stats struct {
	PID            int               `json:"pid"`
	MemoryCurrent  int64             `json:"memoryCurrentBytes,omitempty"`
	MemoryMax      string            `json:"memoryMax,omitempty"`
	PIDsCurrent    int64             `json:"pidsCurrent,omitempty"`
	CPUUsageUsec   int64             `json:"cpuUsageUsec,omitempty"`
	CPUUserUsec    int64             `json:"cpuUserUsec,omitempty"`
	CPUSystemUsec  int64             `json:"cpuSystemUsec,omitempty"`
	IOCounters     map[string]string `json:"ioCounters,omitempty"`
	AvailableFiles []string          `json:"availableFiles,omitempty"`
}
