package main

import (
	"fmt"
	"os"

	"github.com/manit6752025/boxr/internal/container"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: boxr run <command> [args...]")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		err := container.Run(os.Args[2], os.Args[3:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
