//go:build !linux

package container

import "fmt"

func Run(command string, args []string) error {
	return fmt.Errorf("boxr only supports Linux — namespace isolation requires the Linux kernel")
}
