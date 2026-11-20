//go:build !linux

package main

import (
	"fmt"
	"io"
)

func runExec(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "boxr exec is supported only on Linux")
	return 2
}

func runExecLaunch(args []string) int { return 125 }

func runExecChild(args []string, stdout, stderr io.Writer) int { return 125 }
