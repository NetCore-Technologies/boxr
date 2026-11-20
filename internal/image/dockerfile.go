package image

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const (
	maxDockerfileBytes        = 1 << 20
	maxDockerfileLineBytes    = 64 << 10
	maxDockerfileInstructions = 512
)

// LoadDockerfile loads a bounded, regular Dockerfile without following symlinks.
func LoadDockerfile(filename string) (BuildSpec, error) {
	var empty BuildSpec
	info, err := os.Lstat(filename)
	if err != nil {
		return empty, fmt.Errorf("inspect Dockerfile: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxDockerfileBytes {
		return empty, fmt.Errorf("Dockerfile must be a regular non-symlink file no larger than %d bytes", maxDockerfileBytes)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return empty, fmt.Errorf("read Dockerfile: %w", err)
	}
	return ParseDockerfile(data)
}

// ParseDockerfile converts the supported non-executing Dockerfile subset into a
// Boxr build spec. RUN, ADD, and multi-stage builds are rejected rather than
// silently ignored or executed with host privileges.
func ParseDockerfile(data []byte) (BuildSpec, error) {
	var spec BuildSpec
	if len(data) == 0 || len(data) > maxDockerfileBytes {
		return spec, fmt.Errorf("Dockerfile is empty or exceeds %d bytes", maxDockerfileBytes)
	}
	spec.Version = 1
	args := make(map[string]string)
	environment := make(map[string]string)
	workingDir := "/"
	fromSeen := false
	instructionCount := 0
	var logical strings.Builder
	logicalStart := 0

	process := func(line string, lineNumber int) error {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			return nil
		}
		instructionCount++
		if instructionCount > maxDockerfileInstructions {
			return fmt.Errorf("Dockerfile exceeds %d instructions", maxDockerfileInstructions)
		}
		split := strings.IndexFunc(trimmed, unicode.IsSpace)
		instruction, rest := trimmed, ""
		if split >= 0 {
			instruction, rest = trimmed[:split], strings.TrimSpace(trimmed[split:])
		}
		instruction = strings.ToUpper(instruction)
		if instruction != "ARG" && !fromSeen && instruction != "FROM" {
			return fmt.Errorf("line %d: %s must follow FROM", lineNumber, instruction)
		}
		if instruction == "ARG" {
			words, err := dockerfileWords(rest)
			if err != nil || len(words) != 1 {
				return fmt.Errorf("line %d: ARG expects NAME or NAME=DEFAULT", lineNumber)
			}
			name, value, hasDefault := strings.Cut(words[0], "=")
			if !dockerfileIdentifier(name) {
				return fmt.Errorf("line %d: invalid ARG name %q", lineNumber, name)
			}
			if hasDefault {
				args[name] = expandDockerVariables(value, args, environment)
			} else if _, exists := args[name]; !exists {
				args[name] = ""
			}
			return nil
		}
		expanded := expandDockerVariables(rest, args, environment)
		switch instruction {
		case "FROM":
			if fromSeen {
				return fmt.Errorf("line %d: multi-stage FROM is not supported yet", lineNumber)
			}
			words, err := dockerfileWords(expanded)
			if err != nil {
				return fmt.Errorf("line %d: parse FROM: %w", lineNumber, err)
			}
			if len(words) == 3 && strings.EqualFold(words[1], "AS") {
				words = words[:1]
			}
			if len(words) != 1 {
				return fmt.Errorf("line %d: FROM supports one base image and an optional AS name", lineNumber)
			}
			if strings.HasPrefix(words[0], "--") {
				return fmt.Errorf("line %d: FROM platform flags are not supported; use --platform on boxr image build", lineNumber)
			}
			spec.Base = words[0]
			fromSeen = true

		case "COPY":
			var words []string
			if strings.HasPrefix(expanded, "[") {
				if err := json.Unmarshal([]byte(expanded), &words); err != nil {
					return fmt.Errorf("line %d: invalid JSON COPY: %w", lineNumber, err)
				}
			} else {
				var err error
				words, err = dockerfileWords(expanded)
				if err != nil {
					return fmt.Errorf("line %d: parse COPY: %w", lineNumber, err)
				}
			}
			if len(words) < 2 {
				return fmt.Errorf("line %d: COPY expects source(s) and a destination", lineNumber)
			}
			for _, option := range words[:len(words)-1] {
				if strings.HasPrefix(option, "--") {
					return fmt.Errorf("line %d: COPY flags such as --from, --chown and --chmod are not supported yet", lineNumber)
				}
			}
			destination := words[len(words)-1]
			if !path.IsAbs(destination) {
				destination = path.Join(workingDir, destination)
			}
			destination = path.Clean(destination)
			sources := words[:len(words)-1]
			if len(sources) > 1 && !strings.HasSuffix(words[len(words)-1], "/") {
				return fmt.Errorf("line %d: multiple COPY sources require a destination ending in /", lineNumber)
			}
			for _, source := range sources {
				cleanSource := path.Clean(source)
				if strings.HasPrefix(cleanSource, "/") || cleanSource == ".." || strings.HasPrefix(cleanSource, "../") {
					return fmt.Errorf("line %d: COPY source %q must stay inside the build context", lineNumber, source)
				}
				if strings.ContainsAny(cleanSource, "*?[]") {
					return fmt.Errorf("line %d: COPY wildcard sources are not supported yet", lineNumber)
				}
				if len(sources) > 1 {
					spec.Copy = append(spec.Copy, CopyInstruction{Source: cleanSource, Destination: path.Join(destination, path.Base(cleanSource))})
				} else {
					spec.Copy = append(spec.Copy, CopyInstruction{Source: cleanSource, Destination: destination})
				}
			}

		case "WORKDIR":
			words, err := dockerfileWords(expanded)
			if err != nil || len(words) != 1 {
				return fmt.Errorf("line %d: WORKDIR expects one path", lineNumber)
			}
			if path.IsAbs(words[0]) {
				workingDir = path.Clean(words[0])
			} else {
				workingDir = path.Clean(path.Join(workingDir, words[0]))
			}
			if !strings.HasPrefix(workingDir, "/") {
				return fmt.Errorf("line %d: WORKDIR must resolve inside the image root", lineNumber)
			}
			spec.Config.WorkingDir = workingDir

		case "ENV":
			words, err := dockerfileWords(expanded)
			if err != nil || len(words) == 0 {
				return fmt.Errorf("line %d: ENV expects KEY=VALUE assignments or KEY VALUE", lineNumber)
			}
			assignments, err := dockerfileAssignments(words, true)
			if err != nil {
				return fmt.Errorf("line %d: ENV: %w", lineNumber, err)
			}
			keys := make([]string, 0, len(assignments))
			for key := range assignments {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				value := assignments[key]
				environment[key] = value
				replaced := false
				for index, existing := range spec.Config.Env {
					existingKey, _, _ := strings.Cut(existing, "=")
					if existingKey == key {
						spec.Config.Env[index] = key + "=" + value
						replaced = true
						break
					}
				}
				if !replaced {
					spec.Config.Env = append(spec.Config.Env, key+"="+value)
				}
			}

		case "LABEL":
			words, err := dockerfileWords(expanded)
			if err != nil || len(words) == 0 {
				return fmt.Errorf("line %d: LABEL expects KEY=VALUE assignments", lineNumber)
			}
			assignments, err := dockerfileAssignments(words, false)
			if err != nil {
				return fmt.Errorf("line %d: LABEL: %w", lineNumber, err)
			}
			if spec.Config.Labels == nil {
				spec.Config.Labels = make(map[string]string)
			}
			for key, value := range assignments {
				spec.Config.Labels[key] = value
			}

		case "USER":
			words, err := dockerfileWords(expanded)
			if err != nil || len(words) != 1 {
				return fmt.Errorf("line %d: USER expects one user or UID[:GID]", lineNumber)
			}
			spec.Config.User = words[0]

		case "CMD":
			command, err := dockerfileCommand(rest)
			if err != nil {
				return fmt.Errorf("line %d: CMD: %w", lineNumber, err)
			}
			spec.Config.Cmd = command

		case "ENTRYPOINT":
			command, err := dockerfileCommand(rest)
			if err != nil {
				return fmt.Errorf("line %d: ENTRYPOINT: %w", lineNumber, err)
			}
			spec.Config.Entrypoint = command

		case "RUN":
			return fmt.Errorf("line %d: RUN is not supported yet; Boxr does not execute build commands", lineNumber)
		case "ADD":
			return fmt.Errorf("line %d: ADD is not supported yet; use COPY for local context files", lineNumber)
		case "ONBUILD", "HEALTHCHECK", "EXPOSE", "VOLUME", "STOPSIGNAL", "SHELL":
			return fmt.Errorf("line %d: Dockerfile instruction %s is not supported yet", lineNumber, instruction)
		default:
			return fmt.Errorf("line %d: unknown Dockerfile instruction %s", lineNumber, instruction)
		}
		return nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxDockerfileLineBytes)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if logical.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if logical.Len() == 0 {
			logicalStart = lineNumber
		}
		if strings.HasSuffix(line, "\\") {
			logical.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")))
			logical.WriteByte(' ')
			if logical.Len() > maxDockerfileLineBytes {
				return spec, fmt.Errorf("line %d: continued Dockerfile instruction exceeds %d bytes", logicalStart, maxDockerfileLineBytes)
			}
			continue
		}
		logical.WriteString(line)
		if err := process(logical.String(), logicalStart); err != nil {
			return spec, err
		}
		logical.Reset()
	}
	if err := scanner.Err(); err != nil {
		return spec, fmt.Errorf("read Dockerfile: %w", err)
	}
	if logical.Len() > 0 {
		return spec, fmt.Errorf("line %d: Dockerfile ends with an unfinished continuation", logicalStart)
	}
	if !fromSeen {
		return spec, fmt.Errorf("Dockerfile must contain exactly one FROM instruction")
	}
	if err := ValidateBuildSpec(spec); err != nil {
		return spec, fmt.Errorf("invalid Dockerfile build configuration: %w", err)
	}
	return spec, nil
}

func dockerfileWords(value string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for _, character := range value {
		if escaped {
			word.WriteRune(character)
			escaped = false
			started = true
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				word.WriteRune(character)
			}
			started = true
			continue
		}
		switch {
		case character == '"' || character == '\'':
			quote = character
			started = true
		case unicode.IsSpace(character):
			flush()
		default:
			word.WriteRune(character)
			started = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape character")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quoted value")
	}
	flush()
	return words, nil
}

func dockerfileAssignments(words []string, allowLegacy bool) (map[string]string, error) {
	assignments := make(map[string]string, len(words))
	if !strings.Contains(words[0], "=") {
		if !allowLegacy || len(words) < 2 {
			return nil, fmt.Errorf("expected KEY=VALUE")
		}
		assignments[words[0]] = strings.Join(words[1:], " ")
	} else {
		for _, word := range words {
			key, value, ok := strings.Cut(word, "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("mixing assignment and legacy key-value syntax is not supported")
			}
			assignments[key] = value
		}
	}
	for key := range assignments {
		if !dockerfileIdentifier(key) {
			return nil, fmt.Errorf("invalid key %q", key)
		}
	}
	return assignments, nil
}

func dockerfileIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character == '_' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func expandDockerVariables(value string, args, environment map[string]string) string {
	return os.Expand(value, func(key string) string {
		if resolved, ok := environment[key]; ok {
			return resolved
		}
		return args[key]
	})
}

func dockerfileCommand(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("command cannot be empty")
	}
	if strings.HasPrefix(value, "[") {
		var command []string
		if err := json.Unmarshal([]byte(value), &command); err != nil {
			return nil, fmt.Errorf("invalid JSON command form: %w", err)
		}
		if len(command) == 0 {
			return nil, fmt.Errorf("JSON command form cannot be empty")
		}
		return command, nil
	}
	return []string{"/bin/sh", "-c", value}, nil
}

// BuildDockerfileForPlatform builds the supported non-executing Dockerfile
// subset. The Dockerfile and .dockerignore must remain inside the build context.
func BuildDockerfileForPlatform(contextDir, dockerfilePath, reference, platform string) (Image, error) {
	contextAbs, err := filepath.Abs(contextDir)
	if err != nil {
		return Image{}, err
	}
	dockerfileAbs, err := filepath.Abs(dockerfilePath)
	if err != nil {
		return Image{}, err
	}
	relative, err := filepath.Rel(contextAbs, dockerfileAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return Image{}, fmt.Errorf("Dockerfile must be located inside the build context")
	}
	spec, err := LoadDockerfile(dockerfileAbs)
	if err != nil {
		return Image{}, err
	}
	return buildWithSpec(contextAbs, spec, reference, platform, ".dockerignore", filepath.ToSlash(relative), ".dockerignore")
}
