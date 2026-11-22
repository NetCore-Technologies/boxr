package deploy

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// parseComposeInit accepts init:true because Boxr always launches a PID-1
// helper that forwards signals and reaps the workload. Disabling that helper
// is not a supported or safe runtime mode.
func parseComposeInit(node *yaml.Node) error {
	if node == nil || node.Kind == 0 {
		return nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return fmt.Errorf("Compose init must be a boolean")
	}
	enabled, err := strconv.ParseBool(node.Value)
	if err != nil {
		return fmt.Errorf("Compose init must be a boolean: %w", err)
	}
	if !enabled {
		return fmt.Errorf("Compose init: false is unsupported because Boxr requires its PID-1 signal-forwarding and child-reaping helper")
	}
	return nil
}
