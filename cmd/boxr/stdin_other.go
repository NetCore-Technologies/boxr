//go:build !linux

package main

import (
	"fmt"
	"io"
)

func openEngineContainerStdinBridge(_ string, _ bool) (io.Reader, func(), error) {
	return nil, nil, fmt.Errorf("persistent Docker container stdin attachment requires Linux")
}
