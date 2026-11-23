//go:build linux || darwin

package image

import (
	"fmt"
	"os"
)

func applyDockerfileCopyOwner(path string, uid, gid int) error {
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("apply COPY --chown %d:%d to %s: %w", uid, gid, path, err)
	}
	return nil
}
