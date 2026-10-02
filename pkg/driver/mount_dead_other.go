//go:build !linux
// +build !linux

package driver

// statfsFn is a no-op on non-Linux platforms: the dead-FUSE recovery path it
// feeds is Linux-specific.
var statfsFn = func(path string) error {
	return nil
}
