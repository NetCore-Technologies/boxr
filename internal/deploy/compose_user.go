package deploy

import (
	"fmt"

	"github.com/manit6752025/boxr/internal/container"
	"gopkg.in/yaml.v3"
)

// parseComposeUser validates Compose's USER[:GROUP] scalar and leaves identity
// resolution to the container runtime, which reads the selected image rootfs.
func parseComposeUser(node *yaml.Node) (string, error) {
	if node == nil || node.Kind == 0 {
		return "", nil
	}
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!str" && node.Tag != "!!int") || node.Value == "" {
		return "", fmt.Errorf("Compose user must be a non-empty string or numeric UID[:GID]")
	}
	if err := container.ValidateUserSpecForNamespace(node.Value, false); err != nil {
		return "", fmt.Errorf("Compose user: %w", err)
	}
	return node.Value, nil
}
