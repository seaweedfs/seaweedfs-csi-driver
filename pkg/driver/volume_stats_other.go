//go:build !linux

package driver

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// volumeUsage holds filesystem usage numbers reported by NodeGetVolumeStats.
type volumeUsage struct {
	capacityBytes  int64
	usedBytes      int64
	availableBytes int64
	inodes         int64
	inodesUsed     int64
	inodesFree     int64
}

// readVolumeUsage is not implemented on non-linux platforms; the CSI node
// plugin only runs on linux nodes.
func readVolumeUsage(path string) (*volumeUsage, error) {
	return nil, status.Errorf(codes.Unimplemented, "volume stats not supported on this platform")
}
