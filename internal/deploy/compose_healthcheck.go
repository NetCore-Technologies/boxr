package deploy

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"gopkg.in/yaml.v3"
)

// parseComposeHealthcheck translates the Docker Compose healthcheck subset
// supported by Boxr's existing per-container health monitor.
func parseComposeHealthcheck(node *yaml.Node) (*container.HealthcheckConfig, error) {
	if node == nil || node.Kind == 0 {
		return nil, nil
	}
	fields, err := composeMountMap(node, "healthcheck",
		"test", "interval", "timeout", "start_period", "start_interval", "retries", "disable")
	if err != nil {
		return nil, err
	}
	disabled := false
	if value, exists := fields["disable"]; exists {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
			return nil, fmt.Errorf("healthcheck disable must be a boolean")
		}
		disabled, err = strconv.ParseBool(value.Value)
		if err != nil {
			return nil, fmt.Errorf("healthcheck disable must be a boolean")
		}
	}
	if disabled {
		if len(fields) != 1 {
			return nil, fmt.Errorf("healthcheck disable cannot be combined with test or timing options")
		}
		return &container.HealthcheckConfig{Test: []string{"NONE"}}, nil
	}
	testNode, exists := fields["test"]
	if !exists {
		return nil, fmt.Errorf("healthcheck requires test unless disable is true")
	}
	config := &container.HealthcheckConfig{}
	switch testNode.Kind {
	case yaml.ScalarNode:
		if testNode.Tag != "!!str" || strings.TrimSpace(testNode.Value) == "" {
			return nil, fmt.Errorf("healthcheck test string must be non-empty")
		}
		config.Test = []string{"CMD-SHELL", testNode.Value}
	case yaml.SequenceNode:
		if len(testNode.Content) == 0 || len(testNode.Content) > 17 {
			return nil, fmt.Errorf("healthcheck test must contain between 1 and 17 string entries")
		}
		config.Test = make([]string, 0, len(testNode.Content))
		for index, value := range testNode.Content {
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return nil, fmt.Errorf("healthcheck test entry %d must be a string", index+1)
			}
			config.Test = append(config.Test, value.Value)
		}
	default:
		return nil, fmt.Errorf("healthcheck test must be a string or a string list")
	}
	for _, item := range []struct {
		name        string
		destination *time.Duration
	}{
		{name: "interval", destination: &config.Interval},
		{name: "timeout", destination: &config.Timeout},
		{name: "start_period", destination: &config.StartPeriod},
		{name: "start_interval", destination: &config.StartInterval},
	} {
		value, exists := fields[item.name]
		if !exists {
			continue
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || strings.TrimSpace(value.Value) != value.Value || value.Value == "" {
			return nil, fmt.Errorf("healthcheck %s must be a duration string", item.name)
		}
		duration, parseErr := time.ParseDuration(value.Value)
		if parseErr != nil {
			return nil, fmt.Errorf("healthcheck %s: %w", item.name, parseErr)
		}
		*item.destination = duration
	}
	if value, exists := fields["retries"]; exists {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
			return nil, fmt.Errorf("healthcheck retries must be an integer")
		}
		retries, parseErr := strconv.Atoi(value.Value)
		if parseErr != nil {
			return nil, fmt.Errorf("healthcheck retries: %w", parseErr)
		}
		config.Retries = retries
	}
	normalized, err := container.NormalizeHealthcheck(config)
	if err != nil {
		return nil, fmt.Errorf("invalid healthcheck: %w", err)
	}
	return normalized, nil
}
