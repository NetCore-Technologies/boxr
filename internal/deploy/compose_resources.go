package deploy

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxComposeCPULimit = 1_000_000

func composeResourceScalar(node *yaml.Node, field string, required bool) (string, error) {
	if node == nil || node.Kind == 0 {
		if required {
			return "", fmt.Errorf("Compose %s is required", field)
		}
		return "", nil
	}
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!float") {
		return "", fmt.Errorf("Compose %s must be a scalar number or size string", field)
	}
	value := strings.TrimSpace(node.Value)
	if value == "" || value != node.Value {
		return "", fmt.Errorf("Compose %s cannot be empty or contain surrounding whitespace", field)
	}
	return value, nil
}

// parseComposeMemoryLimit uses binary Docker-style size suffixes and checks
// overflow before mapping the value into cgroup v2 memory.max.
func parseComposeResourceBytes(node *yaml.Node, field string) (int64, error) {
	value, err := composeResourceScalar(node, field, false)
	if err != nil || value == "" {
		return 0, err
	}
	lower := strings.ToLower(value)
	units := []struct {
		suffix string
		mult   uint64
	}{
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"b", 1},
	}
	number := lower
	var multiplier uint64 = 1
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			number = strings.TrimSpace(strings.TrimSuffix(lower, unit.suffix))
			multiplier = unit.mult
			break
		}
	}
	amount, parseErr := strconv.ParseUint(number, 10, 64)
	maxInt64 := uint64(math.MaxInt64)
	if parseErr != nil || amount == 0 || amount > maxInt64/multiplier {
		return 0, fmt.Errorf("Compose %s must be a positive byte size within the signed 64-bit range", field)
	}
	return int64(amount * multiplier), nil
}

func parseComposeMemoryLimit(node *yaml.Node) (int64, error) {
	return parseComposeResourceBytes(node, "mem_limit")
}

func parseComposeMemoryReservation(node *yaml.Node) (int64, error) {
	return parseComposeResourceBytes(node, "mem_reservation")
}

// parseComposeCPUs rejects values that the cgroup v2 quota/period interface
// cannot faithfully represent.
func parseComposeCPUs(node *yaml.Node) (float64, error) {
	value, err := composeResourceScalar(node, "cpus", false)
	if err != nil || value == "" {
		return 0, err
	}
	cpus, parseErr := strconv.ParseFloat(value, 64)
	if parseErr != nil || math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus <= 0 || cpus > maxComposeCPULimit || cpus*100000 < 1 {
		return 0, fmt.Errorf("Compose cpus must be a finite positive number representable by cgroup v2 (maximum %d)", maxComposeCPULimit)
	}
	return cpus, nil
}

// parseComposePidsLimit accepts -1 for unlimited, which maps to Boxr's zero
// (no pids.max limit); a positive integer maps directly to cgroup v2 pids.max.
func parseComposePidsLimit(node *yaml.Node) (int64, error) {
	value, err := composeResourceScalar(node, "pids_limit", false)
	if err != nil || value == "" {
		return 0, err
	}
	limit, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || limit == 0 || limit < -1 {
		return 0, fmt.Errorf("Compose pids_limit must be a positive integer or -1 for unlimited")
	}
	if limit == -1 {
		return 0, nil
	}
	return limit, nil
}

// parseComposeCPUShares converts Docker's 2..262144 CPU-share scale to
// cgroup v2's 1..10000 CPU-weight scale using the kernel-compatible formula.
func parseComposeCPUShares(node *yaml.Node) (int, error) {
	value, err := composeResourceScalar(node, "cpu_shares", false)
	if err != nil || value == "" {
		return 0, err
	}
	shares, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || shares < 2 || shares > 262144 {
		return 0, fmt.Errorf("Compose cpu_shares must be an integer between 2 and 262144")
	}
	weight := 1 + ((shares - 2) * 9999 / 262142)
	return int(weight), nil
}

