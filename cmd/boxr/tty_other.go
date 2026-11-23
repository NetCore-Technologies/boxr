//go:build !linux

package main

import "fmt"

func openEngineContainerTTYResizeBridge(_ string, _ func(rows, columns uint16) error) (func(), error) {
	return nil, fmt.Errorf("container TTY resize currently requires Linux")
}
