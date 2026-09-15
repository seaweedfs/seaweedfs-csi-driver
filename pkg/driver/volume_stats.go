package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type volumeUsage struct {
	capacityBytes  int64
	usedBytes      int64
	availableBytes int64
	inodes         int64
	inodesUsed     int64
	inodesFree     int64
}

type volumeStatsCall struct {
	done  chan struct{}
	usage *volumeUsage
	err   error
}

func (ns *NodeServer) validateVolumeStatsPath(volumeID, volumePath string) error {
	if !filepath.IsAbs(volumePath) {
		return status.Errorf(codes.InvalidArgument, "volume path %s is not absolute", volumePath)
	}

	if value, ok := ns.volumes.Load(volumeID); ok {
		vol := value.(*Volume)
		if vol.StagedPath == volumePath || vol.HasPublishPath(volumePath) {
			return nil
		}
		return status.Errorf(codes.NotFound, "volume path %s is not published for volume %s", volumePath, volumeID)
	}

	if isKubeletCSIPublishPath(volumePath) {
		return nil
	}

	return status.Errorf(codes.NotFound, "volume %s is not staged or published", volumeID)
}

func (ns *NodeServer) readVolumeUsageWithTimeout(ctx context.Context, volumeID, volumePath string) (*volumeUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHealthCheckTimeout)
	defer cancel()

	call := &volumeStatsCall{done: make(chan struct{})}
	if _, loaded := ns.activeStats.LoadOrStore(volumeID, call); loaded {
		return nil, status.Errorf(codes.Unavailable, "volume stats collection already in progress for volume %s", volumeID)
	}

	go func() {
		defer ns.activeStats.Delete(volumeID)
		defer close(call.done)
		call.usage, call.err = ns.readVolumeUsageForStats(volumePath)
	}()

	select {
	case <-call.done:
		return call.usage, call.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, status.Error(codes.Canceled, "volume stats request canceled")
		}
		return nil, status.Errorf(codes.DeadlineExceeded, "volume stats collection timed out for volume %s", volumeID)
	}
}

func (ns *NodeServer) readVolumeUsage(path string) (*volumeUsage, error) {
	if ns.readVolumeUsageFn != nil {
		return ns.readVolumeUsageFn(path)
	}
	return readVolumeUsage(path)
}

func isKubeletCSIPublishPath(path string) bool {
	parts := strings.Split(filepath.Clean(path), string(os.PathSeparator))
	for i := 0; i+5 < len(parts); i++ {
		if parts[i] == "pods" &&
			parts[i+1] != "" &&
			parts[i+2] == "volumes" &&
			parts[i+3] == "kubernetes.io~csi" &&
			parts[i+4] != "" &&
			parts[i+5] == "mount" {
			return i+6 == len(parts)
		}
	}
	return false
}
