// Package deploy's Compose interpolation follows Docker Compose's variable
// substitution rules for YAML scalar values. Project .env values are defaults;
// variables already present in the Boxr process environment take precedence.
package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxComposeDotEnvBytes = 1 << 20
	maxComposeDotEnvLine  = 64 << 10
	maxComposeExpandDepth = 8
)

func loadComposeInterpolationEnvironment(composePath string) (map[string]string, error) {
	values := make(map[string]string)
	dotEnvPath := filepath.Join(filepath.Dir(composePath), ".env")
	info, err := os.Lstat(dotEnvPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Compose .env file must be a regular non-symlink file")
		}
		if info.Size() > maxComposeDotEnvBytes {
			return nil, fmt.Errorf("Compose .env file exceeds %d bytes", maxComposeDotEnvBytes)
		}
		dotEnvFile, openErr := os.Open(dotEnvPath)
		if openErr != nil {
			return nil, fmt.Errorf("open Compose .env file: %w", openErr)
		}
		openedInfo, statErr := dotEnvFile.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
			_ = dotEnvFile.Close()
			if statErr != nil {
				return nil, fmt.Errorf("inspect opened Compose .env file: %w", statErr)
			}
			return nil, fmt.Errorf("Compose .env file changed while opening")
		}
		data, readErr := io.ReadAll(io.LimitReader(dotEnvFile, maxComposeDotEnvBytes+1))
		closeErr := dotEnvFile.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read Compose .env file: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close Compose .env file: %w", closeErr)
		}
		if len(data) > maxComposeDotEnvBytes {
			return nil, fmt.Errorf("Compose .env file exceeds %d bytes", maxComposeDotEnvBytes)
		}
		for lineIndex, line := range strings.Split(string(data), "\n") {
			if len(line) > maxComposeDotEnvLine {
				return nil, fmt.Errorf("Compose .env line %d exceeds %d bytes", lineIndex+1, maxComposeDotEnvLine)
			}
			line = strings.TrimSuffix(line, "\r")
			if lineIndex == 0 {
				line = strings.TrimPrefix(line, "\uFEFF")
			}
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			key, rawValue, hasValue := strings.Cut(line, "=")
			if !hasValue {
				continue // KEY without '=' is intentionally unset in the dotenv file.
			}
			key = strings.TrimSpace(key)
			if !validComposeVariableName(key) {
				return nil, fmt.Errorf("Compose .env line %d has an invalid variable name", lineIndex+1)
			}
			value, parseErr := parseComposeDotEnvValue(strings.TrimSpace(rawValue))
			if parseErr != nil {
				return nil, fmt.Errorf("Compose .env line %d: %w", lineIndex+1, parseErr)
			}
			values[key] = value
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect Compose .env file: %w", err)
	}

	// The invoking environment always wins, including an explicitly empty value.
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values, nil
}

func parseComposeDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		value := raw[1 : end+1]
		suffix := strings.TrimSpace(raw[end+2:])
		if suffix != "" && !strings.HasPrefix(suffix, "#") {
			return "", fmt.Errorf("unexpected text after quoted value")
		}
		return value, nil
	case '"':
		end := -1
		escaped := false
		for index := 1; index < len(raw); index++ {
			if escaped {
				escaped = false
				continue
			}
			if raw[index] == '\\' {
				escaped = true
				continue
			}
			if raw[index] == '"' {
				end = index
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		suffix := strings.TrimSpace(raw[end+1:])
		if suffix != "" && !strings.HasPrefix(suffix, "#") {
			return "", fmt.Errorf("unexpected text after quoted value")
		}
		return unquoteComposeDotEnv(raw[:end+1])
	default:
		if comment := strings.Index(raw, " #"); comment >= 0 {
			raw = raw[:comment]
		}
		return strings.TrimSpace(raw), nil
	}
}

// unquoteComposeDotEnv decodes the small escape set supported by dotenv
// double-quoted values without interpreting shell expansions or commands.
func unquoteComposeDotEnv(value string) (string, error) {
	var out strings.Builder
	for index := 1; index < len(value)-1; index++ {
		character := value[index]
		if character != '\\' {
			out.WriteByte(character)
			continue
		}
		index++
		if index >= len(value)-1 {
			return "", fmt.Errorf("invalid trailing escape in double-quoted value")
		}
		switch value[index] {
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case '\\', '"', '$':
			out.WriteByte(value[index])
		default:
			return "", fmt.Errorf("unsupported escape sequence in double-quoted value")
		}
	}
	return out.String(), nil
}

func validComposeVariableName(value string) bool {
	if value == "" || !composeVariableStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !composeVariableStart(character) && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func composeVariableStart(character byte) bool {
	return character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z' || character == '_'
}

func interpolateComposeNode(node *yaml.Node, environment map[string]string) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := interpolateComposeNode(child, environment); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		// Compose interpolates values, never mapping keys.
		for index := 1; index < len(node.Content); index += 2 {
			if err := interpolateComposeNode(node.Content[index], environment); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" || node.Style&yaml.SingleQuotedStyle != 0 {
			return nil
		}
		expanded, err := expandComposeInterpolation(node.Value, environment, 0)
		if err != nil {
			return fmt.Errorf("line %d column %d: %w", node.Line, node.Column, err)
		}
		if len(expanded) > MaxManifestBytes {
			return fmt.Errorf("interpolated scalar exceeds %d bytes", MaxManifestBytes)
		}
		node.Value = expanded
	}
	return nil
}

func expandComposeInterpolation(value string, environment map[string]string, depth int) (string, error) {
	if depth > maxComposeExpandDepth {
		return "", fmt.Errorf("variable expansion exceeds maximum nesting depth")
	}
	var output strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '$' {
			output.WriteByte(value[index])
			index++
			continue
		}
		if index+1 < len(value) && value[index+1] == '$' {
			output.WriteByte('$')
			index += 2
			continue
		}
		if index+1 < len(value) && value[index+1] == '{' {
			end, err := composeInterpolationEnd(value, index+2)
			if err != nil {
				return "", err
			}
			replacement, err := resolveComposeInterpolation(value[index+2:end], environment, depth)
			if err != nil {
				return "", err
			}
			output.WriteString(replacement)
			index = end + 1
			continue
		}
		if index+1 < len(value) && composeVariableStart(value[index+1]) {
			end := index + 2
			for end < len(value) {
				character := value[end]
				if !composeVariableStart(character) && (character < '0' || character > '9') {
					break
				}
				end++
			}
			name := value[index+1 : end]
			replacement, exists := environment[name]
			if !exists {
				return "", fmt.Errorf("variable %s is not set; define it in the process environment or project .env file", name)
			}
			output.WriteString(replacement)
			index = end
			continue
		}
		output.WriteByte('$')
		index++
		if output.Len() > MaxManifestBytes {
			return "", fmt.Errorf("interpolated value exceeds %d bytes", MaxManifestBytes)
		}
	}
	if output.Len() > MaxManifestBytes {
		return "", fmt.Errorf("interpolated value exceeds %d bytes", MaxManifestBytes)
	}
	return output.String(), nil
}

func composeInterpolationEnd(value string, start int) (int, error) {
	nesting := 0
	for index := start; index < len(value); index++ {
		if index+1 < len(value) && value[index] == '$' && value[index+1] == '{' {
			nesting++
			index++
			continue
		}
		if value[index] == '}' {
			if nesting == 0 {
				return index, nil
			}
			nesting--
		}
	}
	return 0, fmt.Errorf("unterminated Compose variable expression")
}

func resolveComposeInterpolation(expression string, environment map[string]string, depth int) (string, error) {
	index := 0
	for index < len(expression) {
		character := expression[index]
		if !composeVariableStart(character) && (character < '0' || character > '9' && index > 0) {
			break
		}
		if index == 0 && character >= '0' && character <= '9' {
			break
		}
		index++
	}
	name := expression[:index]
	if !validComposeVariableName(name) {
		return "", fmt.Errorf("invalid Compose variable expression %q", expression)
	}
	operatorAndWord := expression[index:]
	if operatorAndWord == "" {
		if value, exists := environment[name]; exists {
			return value, nil
		}
		return "", fmt.Errorf("variable %s is not set; define it in the process environment or project .env file", name)
	}
	operator := ""
	word := ""
	for _, candidate := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(operatorAndWord, candidate) {
			operator = candidate
			word = operatorAndWord[len(candidate):]
			break
		}
	}
	if operator == "" {
		return "", fmt.Errorf("unsupported Compose variable expression %q", expression)
	}
	value, exists := environment[name]
	nonEmpty := exists && value != ""
	switch operator {
	case ":-":
		if nonEmpty {
			return value, nil
		}
		return expandComposeInterpolation(word, environment, depth+1)
	case "-":
		if exists {
			return value, nil
		}
		return expandComposeInterpolation(word, environment, depth+1)
	case ":?":
		if nonEmpty {
			return value, nil
		}
		message := "variable " + name + " is required and must not be empty"
		if word != "" {
			if expanded, err := expandComposeInterpolation(word, environment, depth+1); err == nil && expanded != "" {
				message = expanded
			}
		}
		return "", fmt.Errorf("variable %s is required: %s", name, message)
	case "?":
		if exists {
			return value, nil
		}
		message := "variable " + name + " is required"
		if word != "" {
			if expanded, err := expandComposeInterpolation(word, environment, depth+1); err == nil && expanded != "" {
				message = expanded
			}
		}
		return "", fmt.Errorf("variable %s is required: %s", name, message)
	case ":+":
		if nonEmpty {
			return expandComposeInterpolation(word, environment, depth+1)
		}
		return "", nil
	case "+":
		if exists {
			return expandComposeInterpolation(word, environment, depth+1)
		}
		return "", nil
	default:
		return "", fmt.Errorf("unsupported Compose variable operator")
	}
}
