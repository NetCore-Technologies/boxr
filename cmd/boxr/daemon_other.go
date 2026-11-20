//go:build !linux

package main

import (
	"fmt"
	"io"
)

func runDaemon(_ []string, _ io.Writer, stderr io.Writer) int {
	fmt.Fprintln(stderr, "boxr daemon: the local container Engine API requires Linux")
	return 1
}
