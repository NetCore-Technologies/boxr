package container

import "strings"

var linuxCapabilities = map[string]int{
	"CHOWN": 0, "DAC_OVERRIDE": 1, "DAC_READ_SEARCH": 2, "FOWNER": 3,
	"FSETID": 4, "KILL": 5, "SETGID": 6, "SETUID": 7, "SETPCAP": 8,
	"LINUX_IMMUTABLE": 9, "NET_BIND_SERVICE": 10, "NET_BROADCAST": 11,
	"NET_ADMIN": 12, "NET_RAW": 13, "IPC_LOCK": 14, "IPC_OWNER": 15,
	"SYS_MODULE": 16, "SYS_RAWIO": 17, "SYS_CHROOT": 18, "SYS_PTRACE": 19,
	"SYS_PACCT": 20, "SYS_ADMIN": 21, "SYS_BOOT": 22, "SYS_NICE": 23,
	"SYS_RESOURCE": 24, "SYS_TIME": 25, "SYS_TTY_CONFIG": 26, "MKNOD": 27,
	"LEASE": 28, "AUDIT_WRITE": 29, "AUDIT_CONTROL": 30, "SETFCAP": 31,
	"MAC_OVERRIDE": 32, "MAC_ADMIN": 33, "SYSLOG": 34, "WAKE_ALARM": 35,
	"BLOCK_SUSPEND": 36, "AUDIT_READ": 37, "PERFMON": 38, "BPF": 39,
	"CHECKPOINT_RESTORE": 40,
}

func capabilityNumber(value string) (int, bool) {
	name := strings.ToUpper(strings.TrimSpace(value))
	name = strings.TrimPrefix(name, "CAP_")
	number, ok := linuxCapabilities[name]
	return number, ok
}

func normalizedCapabilities(values []string) ([]int, bool) {
	all := false
	seen := make(map[int]bool)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), "ALL") {
			all = true
			continue
		}
		if number, ok := capabilityNumber(value); ok {
			seen[number] = true
		}
	}
	if all {
		out := make([]int, 0, len(linuxCapabilities))
		for i := 0; i <= 40; i++ {
			out = append(out, i)
		}
		return out, true
	}
	out := make([]int, 0, len(seen))
	for i := 0; i <= 40; i++ {
		if seen[i] {
			out = append(out, i)
		}
	}
	return out, false
}
