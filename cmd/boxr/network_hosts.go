package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/manit6752025/boxr/internal/container"
	"github.com/manit6752025/boxr/internal/network"
	"github.com/manit6752025/boxr/internal/state"
)

const managedContainerHostsTarget = "/etc/hosts"

// addManagedNetworkHostsMount prepares the stable, read-only hosts-file inode
// before the namespace helper mounts the container rootfs. User-provided
// /etc or /etc/hosts mounts take precedence and are never overwritten.
func addManagedNetworkHostsMount(containerID string, options *container.Options) error {
	if options == nil {
		return fmt.Errorf("container options are required")
	}
	if runtime.GOOS != "linux" || options.RootFS == "" || options.NetworkMode == "host" {
		return nil
	}
	for _, mount := range options.BindMounts {
		covered, nested := managedHostsMountConflict(mount.Target)
		if covered {
			return nil
		}
		if nested {
			return fmt.Errorf("bind mount target %q is nested below the managed /etc/hosts file", mount.Target)
		}
	}
	for _, mount := range options.TmpfsMounts {
		covered, nested := managedHostsMountConflict(mount.Target)
		if covered {
			return nil
		}
		if nested {
			return fmt.Errorf("tmpfs target %q is nested below the managed /etc/hosts file", mount.Target)
		}
	}
	source, err := network.HostsFilePath(containerID)
	if err != nil {
		return err
	}
	mount := container.BindMount{Source: source, Target: managedContainerHostsTarget, ReadOnly: true}
	for _, existing := range options.BindMounts {
		if filepath.Clean(existing.Target) == managedContainerHostsTarget {
			if filepath.Clean(existing.Source) == filepath.Clean(source) && existing.ReadOnly {
				return nil
			}
			return fmt.Errorf("container already has a user-managed /etc/hosts mount")
		}
	}
	options.BindMounts = append(options.BindMounts, mount)
	if err := state.AddManagedBindMount(containerID, mount); err != nil {
		options.BindMounts = options.BindMounts[:len(options.BindMounts)-1]
		return err
	}
	return nil
}

// managedHostsMountConflict reports an existing mount that covers /etc/hosts,
// and a nested child mount that would be hidden by the managed hosts file.
func managedHostsMountConflict(target string) (covered, nested bool) {
	clean := filepath.Clean(target)
	hostFile := filepath.Clean(managedContainerHostsTarget)
	if clean == hostFile {
		return true, false
	}
	if pathIsWithin(clean, hostFile) {
		return true, false
	}
	if pathIsWithin(hostFile, clean) {
		return false, true
	}
	return false, false
}

func pathIsWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return err == nil && relative == "."
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
