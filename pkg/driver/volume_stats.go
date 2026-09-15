package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	if owner, ok := ns.volumeStatsPathOwner(volumePath); ok && owner != volumeID {
		return status.Errorf(codes.NotFound, "volume path %s is not published for volume %s", volumePath, volumeID)
	}

	if value, ok := ns.volumes.Load(volumeID); ok {
		vol := value.(*Volume)
		if vol.StagedPath == volumePath || vol.HasPublishPath(volumePath) {
			return nil
		}
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
	actual, loaded := ns.activeStats.LoadOrStore(volumeID, call)
	if loaded {
		return waitVolumeStatsCall(ctx, volumeID, actual.(*volumeStatsCall))
	}

	go func() {
		defer ns.activeStats.Delete(volumeID)
		defer close(call.done)
		call.usage, call.err = ns.readVolumeUsageForStats(volumePath)
	}()

	return waitVolumeStatsCall(ctx, volumeID, call)
}

func waitVolumeStatsCall(ctx context.Context, volumeID string, call *volumeStatsCall) (*volumeUsage, error) {
	select {
	case <-call.done:
		return call.usage, call.err
	case <-ctx.Done():
		return nil, volumeStatsContextError(ctx, volumeID)
	}
}

func (ns *NodeServer) readVolumeUsage(path string) (*volumeUsage, error) {
	if ns.readVolumeUsageFn != nil {
		return ns.readVolumeUsageFn(path)
	}
	return readVolumeUsage(path)
}

func (ns *NodeServer) lockVolumeStats(ctx context.Context, volumeID string) (func(), error) {
	volumeMutex := ns.getVolumeMutex(volumeID)
	if volumeMutex.TryLock() {
		return volumeMutex.Unlock, nil
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, volumeStatsContextError(ctx, volumeID)
		case <-ticker.C:
			if volumeMutex.TryLock() {
				return volumeMutex.Unlock, nil
			}
		}
	}
}

func volumeStatsContextError(ctx context.Context, volumeID string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return status.Error(codes.Canceled, "volume stats request canceled")
	}
	return status.Errorf(codes.DeadlineExceeded, "volume stats collection timed out for volume %s", volumeID)
}

func (ns *NodeServer) volumeStatsPathOwner(volumePath string) (string, bool) {
	var owner string
	ns.volumes.Range(func(key, value interface{}) bool {
		vol := value.(*Volume)
		if vol.StagedPath == volumePath || vol.HasPublishPath(volumePath) {
			owner = key.(string)
			return false
		}
		return true
	})
	return owner, owner != ""
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
