//go:build linux
// +build linux

package driver

import "syscall"

// statfs probes the path's filesystem daemon. On a FUSE mount it always
// reaches the daemon, so a dead daemon returns ENOTCONN — while statx-based
// mount checks can still be answered from cached inode attributes.
var statfsFn = func(path string) error {
	var sfs syscall.Statfs_t
	return syscall.Statfs(path, &sfs)
}
