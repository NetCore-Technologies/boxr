//go:build !linux

package network

// Container networking is Linux-only. The process mutex still protects
// read-only metadata operations in CLI-only builds on other platforms.
func acquireNetworkStoreFileLock(string) (func(), error) {
	return func() {}, nil
}
