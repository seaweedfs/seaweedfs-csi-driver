//go:build !linux

package driver

// remountInContainers is a no-op on non-Linux platforms.
// Container mount namespace manipulation requires Linux-specific
// setns(2) and /proc filesystem support.
func remountInContainers(publishPath, stagingPath, oldDevice string) {}

// remountStaleFuseInContainers is a no-op on non-Linux platforms.
func remountStaleFuseInContainers(publishPath, stagingPath string) {}

// getMountDevice is a stub on non-Linux platforms.
func getMountDevice(mountPath string) (string, error) {
	return "", nil
}
