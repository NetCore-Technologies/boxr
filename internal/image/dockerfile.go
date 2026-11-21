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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/manit6752025/boxr/internal/container"
)

type dockerfileStep struct {
	kind        string
	source      string
	destination string
	value       string
	copyMode    os.FileMode
	hasCopyMode bool
	copyFrom    string
	command     []string
	env         []string
	user        string
}

type dockerfileStage struct {
	name       string
	base       string
	config     ImageRuntimeConfig
	steps      []dockerfileStep
	hasWorkdir bool
}

const (
	maxDockerfileBytes        = 1 << 20
	maxDockerfileLineBytes    = 64 << 10
	maxDockerfileInstructions = 512
)

// LoadDockerfile loads a bounded, regular Dockerfile without following symlinks.
func LoadDockerfile(filename string) (BuildSpec, error) {
	return LoadDockerfileWithArgs(filename, nil)
}

// LoadDockerfileWithArgs loads a Dockerfile with explicit --build-arg values.
func LoadDockerfileWithArgs(filename string, buildArgs map[string]string) (BuildSpec, error) {
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
	return ParseDockerfileWithArgs(data, buildArgs)
}

// ParseDockerfile converts the supported Dockerfile subset into a Boxr build
// plan. RUN instructions are executed later inside Boxr's isolated runtime;
// local multi-stage FROM and COPY --from relationships are validated before building.
func ParseDockerfile(data []byte) (BuildSpec, error) {
	return ParseDockerfileWithArgs(data, nil)
}

// ParseDockerfileWithArgs parses a Dockerfile using explicitly supplied build
// arguments. Arguments are visible only after an ARG declaration; pre-FROM
// arguments are scoped to FROM unless they are declared again in the stage.
func ParseDockerfileWithArgs(data []byte, buildArgs map[string]string) (BuildSpec, error) {
	var spec BuildSpec
	if len(data) == 0 || len(data) > maxDockerfileBytes {
		return spec, fmt.Errorf("Dockerfile is empty or exceeds %d bytes", maxDockerfileBytes)
	}
	spec.Version = 1
	providedArgs := make(map[string]string, len(buildArgs))
	for key, value := range buildArgs {
		if !dockerfileIdentifier(key) || strings.ContainsRune(value, '\x00') {
			return spec, fmt.Errorf("invalid build argument %q", key)
		}
		providedArgs[key] = value
	}
	args := make(map[string]string)
	globalArgs := make(map[string]string)
	usedBuildArgs := make(map[string]bool)
	environment := make(map[string]string)
	workingDir := "/"
	currentUser := ""
	fromSeen := false
	currentStageName := ""
	var stages []dockerfileStage
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
			provided, hasProvided := providedArgs[name]
			if hasProvided {
				args[name] = provided
				usedBuildArgs[name] = true
			} else if hasDefault {
				args[name] = expandDockerVariables(value, args, environment)
			} else if fromSeen {
				if inherited, ok := globalArgs[name]; ok {
					args[name] = inherited
				} else if _, exists := args[name]; !exists {
					args[name] = ""
				}
			} else if _, exists := args[name]; !exists {
				args[name] = ""
			}
			if !fromSeen {
				globalArgs[name] = args[name]
			}
			return nil
		}
		expanded := expandDockerVariables(rest, args, environment)
		if instruction == "FROM" {
			expanded = expandDockerVariables(rest, globalArgs, nil)
		}
		switch instruction {
		case "FROM":
			if fromSeen {
				stages = append(stages, dockerfileStage{
					name: currentStageName, base: spec.Base, config: spec.Config,
					steps:      append([]dockerfileStep(nil), spec.dockerfileSteps...),
					hasWorkdir: spec.dockerfileHasWorkdir,
				})
				spec.Config = ImageRuntimeConfig{}
				spec.Copy = nil
				spec.dockerfileSteps = nil
				spec.dockerfileHasWorkdir = false
				environment = make(map[string]string)
				workingDir = "/"
				currentUser = ""
			}
			words, err := dockerfileWords(expanded)
			if err != nil {
				return fmt.Errorf("line %d: parse FROM: %w", lineNumber, err)
			}
			stageName := ""
			if len(words) == 3 && strings.EqualFold(words[1], "AS") {
				stageName = words[2]
				if !dockerfileStageIdentifier(stageName) {
					return fmt.Errorf("line %d: invalid Dockerfile stage name %q", lineNumber, stageName)
				}
				words = words[:1]
			}
			if len(words) != 1 {
				return fmt.Errorf("line %d: FROM supports one base image and an optional AS name", lineNumber)
			}
			if strings.HasPrefix(words[0], "--") {
				return fmt.Errorf("line %d: FROM platform flags are not supported; use --platform on boxr image build", lineNumber)
			}
			spec.Base = words[0]
			currentStageName = stageName
			fromSeen = true
			// Global ARG declarations are used for FROM expansion only.
			args = make(map[string]string)

		case "COPY", "ADD":
			instructionName := instruction
			remaining := strings.TrimSpace(expanded)
			copyMode := os.FileMode(0)
			hasCopyMode := false
			copyFrom := ""
			for strings.HasPrefix(remaining, "--") {
				split := strings.IndexFunc(remaining, unicode.IsSpace)
				option := remaining
				if split >= 0 {
					option = remaining[:split]
					remaining = strings.TrimSpace(remaining[split:])
				} else {
					remaining = ""
				}
				name, value, hasValue := strings.Cut(option, "=")
				switch name {
				case "--chmod":
					if instructionName != "COPY" || !hasValue || hasCopyMode {
						return fmt.Errorf("line %d: unsupported %s option %q; COPY supports one octal --chmod=MODE option", lineNumber, instructionName, option)
					}
					if len(value) < 3 || len(value) > 4 {
						return fmt.Errorf("line %d: COPY --chmod must be a three- or four-digit octal mode from 000 to 0777", lineNumber)
					}
					mode, modeErr := strconv.ParseUint(value, 8, 12)
					if modeErr != nil || mode > 0777 {
						return fmt.Errorf("line %d: COPY --chmod mode %q is invalid; setuid, setgid and sticky bits are not supported", lineNumber, value)
					}
					copyMode, hasCopyMode = os.FileMode(mode), true
				case "--from":
					if instructionName != "COPY" || !hasValue || value == "" || copyFrom != "" {
						return fmt.Errorf("line %d: COPY --from requires a previous stage name/index or a local image reference", lineNumber)
					}
					copyFrom = value
				default:
					return fmt.Errorf("line %d: unsupported %s option %q", lineNumber, instructionName, option)
				}
			}
			var words []string
			if strings.HasPrefix(remaining, "[") {
				if err := json.Unmarshal([]byte(remaining), &words); err != nil {
					return fmt.Errorf("line %d: invalid JSON %s: %w", lineNumber, instructionName, err)
				}
			} else {
				var err error
				words, err = dockerfileWords(remaining)
				if err != nil {
					return fmt.Errorf("line %d: parse %s: %w", lineNumber, instructionName, err)
				}
			}
			if len(words) < 2 {
				return fmt.Errorf("line %d: %s expects source(s) and a destination", lineNumber, instructionName)
			}
			for _, option := range words[:len(words)-1] {
				if strings.HasPrefix(option, "--") {
					return fmt.Errorf("line %d: %s option %q is unsupported", lineNumber, instructionName, option)
				}
			}
			rawDestination := path.Clean(words[len(words)-1])
			destination := rawDestination
			if !path.IsAbs(destination) {
				destination = path.Join(workingDir, destination)
			}
			destination = path.Clean(destination)
			sources := words[:len(words)-1]
			for _, source := range sources {
				if instructionName == "ADD" && isRemoteDockerfileSource(source) {
					return fmt.Errorf("line %d: remote ADD sources are not supported; download the file explicitly into the build context", lineNumber)
				}
				if strings.TrimSpace(source) == "" {
					return fmt.Errorf("line %d: %s source must not be empty", lineNumber, instructionName)
				}
				cleanSource := path.Clean(source)
				if copyFrom == "" {
					if strings.HasPrefix(cleanSource, "/") || cleanSource == ".." || strings.HasPrefix(cleanSource, "../") {
						return fmt.Errorf("line %d: %s source %q must stay inside the build context", lineNumber, instructionName, source)
					}
				} else {
					if cleanSource == ".." || strings.HasPrefix(cleanSource, "../") {
						return fmt.Errorf("line %d: COPY --from source %q must stay inside the source stage", lineNumber, source)
					}
					cleanSource = strings.TrimPrefix(cleanSource, "/")
					if cleanSource == "" {
						cleanSource = "."
					}
				}
				if strings.ContainsAny(cleanSource, "*?[]") {
					return fmt.Errorf("line %d: %s wildcard sources are not supported yet", lineNumber, instructionName)
				}
				validatedDestination := destination
				stepDestination := rawDestination
				if len(sources) > 1 {
					validatedDestination = path.Join(destination, path.Base(cleanSource))
					stepDestination = path.Join(rawDestination, path.Base(cleanSource))
				}
				spec.Copy = append(spec.Copy, CopyInstruction{Source: cleanSource, Destination: validatedDestination})
				stepKind := "copy"
				if instructionName == "ADD" {
					stepKind = "add"
				}
				spec.dockerfileSteps = append(spec.dockerfileSteps, dockerfileStep{
					kind: stepKind, source: cleanSource, destination: stepDestination,
					copyMode: copyMode, hasCopyMode: hasCopyMode, copyFrom: copyFrom,
				})
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
			spec.dockerfileHasWorkdir = true
			spec.dockerfileSteps = append(spec.dockerfileSteps, dockerfileStep{kind: "workdir", value: words[0]})

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
			currentUser = words[0]

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
			if strings.HasPrefix(expanded, "--") {
				return fmt.Errorf("line %d: RUN flags such as --mount and --network are not supported; configure networking with boxr image build --network", lineNumber)
			}
			if currentUser != "" && currentUser != "root" && currentUser != "0" && currentUser != "0:0" {
				return fmt.Errorf("line %d: RUN after USER %q is not supported yet; build commands currently execute as root inside a user namespace", lineNumber, currentUser)
			}
			command, err := dockerfileCommand(rest)
			if err != nil {
				return fmt.Errorf("line %d: RUN: %w", lineNumber, err)
			}
			argKeys := make([]string, 0, len(args))
			for key := range args {
				argKeys = append(argKeys, key)
			}
			sort.Strings(argKeys)
			argEnv := make([]string, 0, len(argKeys))
			for _, key := range argKeys {
				argEnv = append(argEnv, key+"="+args[key])
			}
			spec.dockerfileSteps = append(spec.dockerfileSteps, dockerfileStep{
				kind: "run", command: command,
				env:  mergeBuildEnvironment(argEnv, spec.Config.Env),
				user: currentUser,
			})
		case "VOLUME":
			var targets []string
			if strings.HasPrefix(expanded, "[") {
				if err := json.Unmarshal([]byte(expanded), &targets); err != nil {
					return fmt.Errorf("line %d: invalid JSON VOLUME instruction: %w", lineNumber, err)
				}
			} else {
				var err error
				targets, err = dockerfileWords(expanded)
				if err != nil {
					return fmt.Errorf("line %d: parse VOLUME: %w", lineNumber, err)
				}
			}
			if len(targets) == 0 {
				return fmt.Errorf("line %d: VOLUME expects one or more absolute container paths", lineNumber)
			}
			if spec.Config.Volumes == nil {
				spec.Config.Volumes = make(map[string]struct{})
			}
			for _, target := range targets {
				if !path.IsAbs(target) || target == "/" || path.Clean(target) != target || strings.ContainsAny(target, "\x00\r\n") {
					return fmt.Errorf("line %d: VOLUME target %q must be a clean absolute non-root path", lineNumber, target)
				}
				spec.Config.Volumes[target] = struct{}{}
				if len(spec.Config.Volumes) > 64 {
					return fmt.Errorf("line %d: Dockerfile declares more than 64 volumes", lineNumber)
				}
			}

		case "EXPOSE":
			words, err := dockerfileWords(expanded)
			if err != nil || len(words) == 0 {
				return fmt.Errorf("line %d: EXPOSE expects one or more ports", lineNumber)
			}
			ports, err := normalizeImageExposedPorts(words)
			if err != nil {
				return fmt.Errorf("line %d: invalid EXPOSE declaration: %w", lineNumber, err)
			}
			if spec.Config.ExposedPorts == nil {
				spec.Config.ExposedPorts = make(map[string]struct{})
			}
			for _, port := range ports {
				spec.Config.ExposedPorts[port] = struct{}{}
			}
			if len(spec.Config.ExposedPorts) > 256 {
				return fmt.Errorf("line %d: Dockerfile declares more than 256 exposed ports", lineNumber)
			}

		case "STOPSIGNAL":
			if expanded == "" || strings.ContainsAny(expanded, " \t") {
				return fmt.Errorf("line %d: STOPSIGNAL expects one signal name or number", lineNumber)
			}
			signal, err := container.NormalizeStopSignal(expanded)
			if err != nil {
				return fmt.Errorf("line %d: invalid STOPSIGNAL: %w", lineNumber, err)
			}
			spec.Config.StopSignal = signal

		case "HEALTHCHECK":
			healthcheck, err := parseDockerfileHealthcheck(expanded)
			if err != nil {
				return fmt.Errorf("line %d: invalid HEALTHCHECK: %w", lineNumber, err)
			}
			spec.Config.Healthcheck = healthcheck

		case "ONBUILD", "SHELL":
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
	needsStageExecutor := len(stages) > 0
	for _, step := range spec.dockerfileSteps {
		if step.copyFrom != "" {
			needsStageExecutor = true
			break
		}
	}
	if needsStageExecutor {
		stages = append(stages, dockerfileStage{
			name: currentStageName, base: spec.Base, config: spec.Config,
			steps:      append([]dockerfileStep(nil), spec.dockerfileSteps...),
			hasWorkdir: spec.dockerfileHasWorkdir,
		})
		if err := validateDockerfileStages(stages); err != nil {
			return spec, err
		}
		spec.dockerfileStages = stages
		spec.Base = "scratch"
		spec.Copy = nil
		spec.dockerfileSteps = nil
		spec.Config = stages[len(stages)-1].config
		spec.dockerfileHasWorkdir = stages[len(stages)-1].hasWorkdir
	}
	if !needsStageExecutor {
		if err := validateDockerfileStages([]dockerfileStage{{
			base: spec.Base, config: spec.Config, steps: spec.dockerfileSteps,
		}}); err != nil {
			return spec, err
		}
	}
	unusedArgs := make([]string, 0)
	for key := range providedArgs {
		if !usedBuildArgs[key] {
			unusedArgs = append(unusedArgs, key)
		}
	}
	if len(unusedArgs) > 0 {
		sort.Strings(unusedArgs)
		return spec, fmt.Errorf("build argument(s) not declared with ARG: %s", strings.Join(unusedArgs, ", "))
	}
	if err := ValidateBuildSpec(spec); err != nil {
		return spec, fmt.Errorf("invalid Dockerfile build configuration: %w", err)
	}
	return spec, nil
}

func dockerfileStageIdentifier(value string) bool {
	if value == "" || strings.EqualFold(value, "scratch") {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character == '_' || index > 0 && (character >= '0' && character <= '9' || character == '-' || character == '.') {
			continue
		}
		return false
	}
	_, numericErr := strconv.Atoi(value)
	return numericErr != nil
}

func dockerfileStageIndex(reference string, names map[string]int, current int) (int, bool) {
	if index, err := strconv.Atoi(reference); err == nil {
		return index, index >= 0 && index < current
	}
	index, ok := names[strings.ToLower(reference)]
	return index, ok && index < current
}

func validateDockerfileStages(stages []dockerfileStage) error {
	allNames := make(map[string]int, len(stages))
	for index, stage := range stages {
		if stage.name == "" {
			continue
		}
		key := strings.ToLower(stage.name)
		if _, exists := allNames[key]; exists {
			return fmt.Errorf("Dockerfile stage name %q is declared more than once", stage.name)
		}
		allNames[key] = index
	}

	names := make(map[string]int, len(stages))
	for index, stage := range stages {
		if _, isStage := dockerfileStageIndex(stage.base, names, index); !isStage && stage.base != "scratch" {
			if laterIndex, isLaterStage := allNames[strings.ToLower(stage.base)]; isLaterStage && laterIndex >= index {
				return fmt.Errorf("Dockerfile stage %d FROM %q must reference a previously declared stage", index+1, stage.base)
			}
			if err := ValidateReference(stage.base); err != nil {
				return fmt.Errorf("Dockerfile stage %d has an invalid base image: %w", index+1, err)
			}
		}
		if err := ValidateBuildSpec(BuildSpec{Version: 1, Base: "scratch", Config: stage.config}); err != nil {
			return fmt.Errorf("Dockerfile stage %d has invalid configuration: %w", index+1, err)
		}
		for _, step := range stage.steps {
			if step.copyFrom == "" {
				continue
			}
			if _, ok := dockerfileStageIndex(step.copyFrom, names, index); ok {
				continue
			}
			if _, isStageName := allNames[strings.ToLower(step.copyFrom)]; isStageName {
				return fmt.Errorf("Dockerfile stage %d COPY --from=%s must reference a previously declared stage", index+1, step.copyFrom)
			}
			if _, numeric := strconv.Atoi(step.copyFrom); numeric == nil {
				return fmt.Errorf("Dockerfile stage %d COPY --from=%s does not reference a previous stage index", index+1, step.copyFrom)
			}
			if err := ValidateReference(step.copyFrom); err != nil {
				return fmt.Errorf("Dockerfile stage %d COPY --from=%s is neither a previous stage nor a valid local image reference: %w", index+1, step.copyFrom, err)
			}
		}
		if stage.name != "" {
			names[strings.ToLower(stage.name)] = index
		}
	}
	return nil
}

func parseDockerfileHealthcheck(value string) (*container.HealthcheckConfig, error) {
	remaining := strings.TrimSpace(value)
	config := &container.HealthcheckConfig{}
	for strings.HasPrefix(remaining, "--") {
		words, err := dockerfileWords(remaining)
		if err != nil || len(words) == 0 {
			return nil, fmt.Errorf("invalid healthcheck option list")
		}
		option := words[0]
		name, raw, ok := strings.Cut(option, "=")
		if !ok || raw == "" {
			return nil, fmt.Errorf("healthcheck option %q must use --name=value", option)
		}
		switch name {
		case "--interval", "--timeout", "--start-period", "--start-interval":
			duration, err := time.ParseDuration(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			switch name {
			case "--interval":
				config.Interval = duration
			case "--timeout":
				config.Timeout = duration
			case "--start-period":
				config.StartPeriod = duration
			case "--start-interval":
				config.StartInterval = duration
			}
		case "--retries":
			retries, err := strconv.Atoi(raw)
			if err != nil || retries < 1 || retries > 1000 {
				return nil, fmt.Errorf("--retries must be between 1 and 1000")
			}
			config.Retries = retries
		default:
			return nil, fmt.Errorf("unsupported healthcheck option %q", name)
		}
		remaining = strings.TrimSpace(strings.TrimPrefix(remaining, option))
	}
	if strings.EqualFold(remaining, "NONE") {
		if config.Interval != 0 || config.Timeout != 0 || config.StartPeriod != 0 || config.StartInterval != 0 || config.Retries != 0 {
			return nil, fmt.Errorf("HEALTHCHECK NONE cannot specify options")
		}
		return &container.HealthcheckConfig{Test: []string{"NONE"}}, nil
	}
	command, rest, ok := strings.Cut(remaining, " ")
	if !ok {
		return nil, fmt.Errorf("expected CMD followed by a healthcheck command")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil, fmt.Errorf("healthcheck command cannot be empty")
	}
	switch strings.ToUpper(command) {
	case "CMD":
		if strings.HasPrefix(rest, "[") {
			var arguments []string
			if err := json.Unmarshal([]byte(rest), &arguments); err != nil {
				return nil, fmt.Errorf("parse exec-form command: %w", err)
			}
			config.Test = append([]string{"CMD"}, arguments...)
		} else {
			config.Test = []string{"CMD-SHELL", rest}
		}
	case "CMD-SHELL":
		config.Test = []string{"CMD-SHELL", rest}
	default:
		return nil, fmt.Errorf("expected CMD, CMD-SHELL, or NONE")
	}
	return container.NormalizeHealthcheck(config)
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
		valid := dockerfileIdentifier(key)
		if !allowLegacy {
			valid = dockerfileLabelKey(key)
		}
		if !valid {
			return nil, fmt.Errorf("invalid key %q", key)
		}
	}
	return assignments, nil
}

func dockerfileLabelKey(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character == '=' || unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func isRemoteDockerfileSource(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "://") || strings.HasPrefix(lower, "git@") || strings.HasPrefix(lower, "git+")
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

// BuildDockerfileForPlatform builds a Dockerfile with RUN network access
// disabled by default. Use BuildDockerfileForPlatformWithNetwork for an
// explicit opt-in to host networking during build commands.
func BuildDockerfileForPlatform(contextDir, dockerfilePath, reference, platform string) (Image, error) {
	return BuildDockerfileForPlatformWithOptions(contextDir, dockerfilePath, reference, platform, "none", nil)
}

// BuildDockerfileForPlatformWithNetwork builds with an explicit build network mode.
func BuildDockerfileForPlatformWithNetwork(contextDir, dockerfilePath, reference, platform, networkMode string) (Image, error) {
	return BuildDockerfileForPlatformWithOptions(contextDir, dockerfilePath, reference, platform, networkMode, nil)
}

// BuildDockerfileForPlatformWithOptions builds a Dockerfile with explicit build
// networking and build-argument values.
func BuildDockerfileForPlatformWithOptions(contextDir, dockerfilePath, reference, platform, networkMode string, buildArgs map[string]string) (Image, error) {
	if networkMode != "none" && networkMode != "host" {
		return Image{}, fmt.Errorf("Dockerfile build network must be none or host")
	}
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
	spec, err := LoadDockerfileWithArgs(dockerfileAbs, buildArgs)
	if err != nil {
		return Image{}, err
	}
	spec.dockerfileNetwork = networkMode
	return buildWithSpec(contextAbs, spec, reference, platform, ".dockerignore", filepath.ToSlash(relative), ".dockerignore")
}
