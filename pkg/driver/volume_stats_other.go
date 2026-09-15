//go:build !linux

package driver

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func volumeStatsSupported() error {
	return status.Errorf(codes.Unimplemented, "volume stats not supported on this platform")
}

func readVolumeUsage(path string) (*volumeUsage, error) {
	return nil, volumeStatsSupported()
}

func (ns *NodeServer) readVolumeUsageForStats(path string) (*volumeUsage, error) {
	return ns.readVolumeUsage(path)
}
