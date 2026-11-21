//go:build !linux

package main

import (
	"context"

	"github.com/manit6752025/boxr/internal/container"
)

func startContainerHealthMonitor(_ string, _ *container.HealthcheckConfig) (context.CancelFunc, <-chan struct{}) {
	done := make(chan struct{})
	close(done)
	return func() {}, done
}
