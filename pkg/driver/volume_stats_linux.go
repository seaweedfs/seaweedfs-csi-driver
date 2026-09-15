package driver

import (
	"os"
	"syscall"

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

// readVolumeUsage stats the given volume path and converts statfs counters to
// CSI VolumeUsage values. Linux only; other platforms return Unimplemented.
func readVolumeUsage(path string) (*volumeUsage, error) {
	var sfs syscall.Statfs_t
	if err := syscall.Statfs(path, &sfs); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", path)
		}
		return nil, status.Errorf(codes.Internal, "statfs %s failed: %v", path, err)
	}

	blockSize := int64(sfs.Bsize)
	usage := &volumeUsage{
		capacityBytes:  int64(sfs.Blocks) * blockSize,
		usedBytes:      int64(sfs.Blocks-sfs.Bfree) * blockSize,
		availableBytes: int64(sfs.Bavail) * blockSize,
		inodes:         int64(sfs.Files),
		inodesFree:     int64(sfs.Ffree),
	}
	usage.inodesUsed = usage.inodes - usage.inodesFree
	if usage.inodesUsed < 0 {
		usage.inodesUsed = 0
	}
	return usage, nil
}
