package deploy

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"gopkg.in/yaml.v3"
)

// parseComposeStopSignal applies Boxr's catchable signal subset so the attached
// deployment supervisor can forward the stop signal and still reap its helper.
func parseComposeStopSignal(value string) (string, error) {
	if value == "" {
		// Docker Compose defaults to SIGTERM.
		return "SIGTERM", nil
	}
	normalized, err := container.NormalizeStopSignal(value)
	if err != nil {
		return "", fmt.Errorf("Compose stop_signal: %w", err)
	}
	switch normalized {
	case "SIGHUP", "SIGINT", "SIGQUIT", "SIGUSR1", "SIGUSR2", "SIGTERM":
		return normalized, nil
	default:
		return "", fmt.Errorf("Compose stop_signal %q cannot be safely forwarded by the attached supervisor", value)
	}
}

// parseComposeStopGracePeriod translates a Compose duration into the bounded
// millisecond shutdown budget used by Boxr's attached deployment supervisor.
func parseComposeStopGracePeriod(node *yaml.Node) (int, error) {
	if node == nil || node.Kind == 0 {
		return 0, nil // Boxr's attached deployment default is 10 seconds.
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" || strings.TrimSpace(node.Value) != node.Value || node.Value == "" {
		return 0, fmt.Errorf("Compose stop_grace_period must be a duration string")
	}
	duration, err := time.ParseDuration(node.Value)
	if err != nil {
		return 0, fmt.Errorf("Compose stop_grace_period: %w", err)
	}
	if duration < 10*time.Millisecond || duration > 10*time.Minute {
		return 0, fmt.Errorf("Compose stop_grace_period must be between 10ms and 10m")
	}
	milliseconds := math.Ceil(float64(duration) / float64(time.Millisecond))
	if milliseconds > 600000 {
		return 0, fmt.Errorf("Compose stop_grace_period exceeds 10m")
	}
	return int(milliseconds), nil
}
