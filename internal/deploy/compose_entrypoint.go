package deploy

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// parseComposeEntrypoint distinguishes an omitted/null value (inherit image
// metadata) from an explicit empty value (clear the image entrypoint).
func parseComposeEntrypoint(node *yaml.Node) (*[]string, error) {
	if node == nil || node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	arguments, err := parseComposeCommand(node)
	if err != nil {
		return nil, fmt.Errorf("entrypoint: %w", err)
	}
	normalized := make([]string, len(arguments))
	copy(normalized, arguments)
	return &normalized, nil
}
