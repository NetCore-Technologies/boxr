//go:build !linux && !darwin

package image

import "fmt"

func applyDockerfileCopyOwner(path string, uid, gid int) error {
	return fmt.Errorf("COPY --chown %d:%d is supported only on Linux and macOS", uid, gid)
}
