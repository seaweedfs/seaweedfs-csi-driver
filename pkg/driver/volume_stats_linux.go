package driver

import (
	"os"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func volumeStatsSupported() error {
	return nil
}

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

func (ns *NodeServer) readVolumeUsageForStats(path string) (*volumeUsage, error) {
	healthyFn := ns.isHealthyFn
	if healthyFn == nil {
		healthyFn = isStagingPathHealthy
	}
	if !healthyFn(path) {
		return nil, status.Errorf(codes.NotFound, "volume path %s is not a live mount", path)
	}
	return ns.readVolumeUsage(path)
}
