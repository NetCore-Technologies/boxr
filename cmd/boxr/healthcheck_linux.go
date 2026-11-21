//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/state"
)

type healthOutputBuffer struct {
	mu sync.Mutex
	data bytes.Buffer
}

func (buffer *healthOutputBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	length := len(value)
	remaining := container.MaxHealthcheckOutputBytes - buffer.data.Len()
	if remaining > 0 {
		if length > remaining {
			_, _ = buffer.data.Write(value[:remaining])
		} else {
			_, _ = buffer.data.Write(value)
		}
	}
	return length, nil
}

func (buffer *healthOutputBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.String()
}

func startContainerHealthMonitor(id string, input *container.HealthcheckConfig) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runContainerHealthMonitor(ctx, id, input)
	}()
	return cancel, done
}

func runContainerHealthMonitor(ctx context.Context, id string, input *container.HealthcheckConfig) {
	config, err := container.NormalizeHealthcheck(input)
	if err != nil || !container.HealthcheckEnabled(config) {
		return
	}
	initialDelay := config.Interval
	if config.StartPeriod > 0 {
		initialDelay = config.StartInterval
	}
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		record, err := state.Get(id)
		if err != nil || record.Status != "running" {
			return
		}
		result := executeContainerHealthcheck(ctx, id, config)
		if ctx.Err() != nil {
			return
		}
		record, err = state.Get(id)
		if err != nil || record.Status != "running" {
			return
		}
		current := state.HealthState{Status: "starting", Log: []state.HealthLog{}}
		if record.Health != nil {
			current = *record.Health
			current.Log = append([]state.HealthLog(nil), record.Health.Log...)
		}
		next := nextContainerHealthState(current, record.StartedAt, config, result)
		if err := state.SetHealth(id, next); err != nil {
			return
		}
		delay := config.Interval
		if config.StartPeriod > 0 && record.StartedAt != nil &&
			next.Status == "starting" && result.End.Sub(*record.StartedAt) < config.StartPeriod {
			delay = config.StartInterval
		}
		timer.Reset(delay)
	}
}

func executeContainerHealthcheck(parent context.Context, id string, config *container.HealthcheckConfig) state.HealthLog {
	started := time.Now().UTC()
	result := state.HealthLog{Start: started, ExitCode: 1}
	output := &healthOutputBuffer{}
	executable, err := os.Executable()
	if err != nil {
		_, _ = output.Write([]byte("resolve Boxr executable: " + err.Error()))
		result.Output = output.String()
		result.End = time.Now().UTC()
		return result
	}
	var command []string
	switch config.Test[0] {
	case "CMD":
		command = append([]string(nil), config.Test[1:]...)
	case "CMD-SHELL":
		command = []string{"/bin/sh", "-c", config.Test[1]}
	default:
		_, _ = output.Write([]byte("unsupported healthcheck command type"))
		result.Output = output.String()
		result.End = time.Now().UTC()
		return result
	}
	args := append([]string{"exec", id}, command...)
	ctx, cancel := context.WithTimeout(parent, config.Timeout)
	defer cancel()
	process := exec.CommandContext(ctx, executable, args...)
	process.Stdout = output
	process.Stderr = output
	runErr := process.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.ExitCode = 124
	case runErr == nil:
		result.ExitCode = 0
	default:
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			result.ExitCode = exitError.ExitCode()
		} else {
			_, _ = output.Write([]byte(runErr.Error()))
		}
	}
	result.Output = output.String()
	result.End = time.Now().UTC()
	return result
}

func nextContainerHealthState(current state.HealthState, startedAt *time.Time, config *container.HealthcheckConfig, result state.HealthLog) state.HealthState {
	next := current
	next.Log = append(append([]state.HealthLog(nil), current.Log...), result)
	if len(next.Log) > 5 {
		next.Log = append([]state.HealthLog(nil), next.Log[len(next.Log)-5:]...)
	}
	if result.ExitCode == 0 {
		next.Status = "healthy"
		next.FailingStreak = 0
		return next
	}
	inGracePeriod := config.StartPeriod > 0 && startedAt != nil &&
		result.End.Sub(*startedAt) < config.StartPeriod
	if !inGracePeriod || current.Status == "healthy" || current.Status == "unhealthy" {
		next.FailingStreak++
	}
	retries := config.Retries
	if retries <= 0 {
		retries = container.DefaultHealthcheckRetries
	}
	if next.FailingStreak >= retries {
		next.Status = "unhealthy"
	} else if current.Status == "healthy" || current.Status == "unhealthy" {
		next.Status = current.Status
	} else {
		next.Status = "starting"
	}
	return next
}

var _ io.Writer = (*healthOutputBuffer)(nil)
