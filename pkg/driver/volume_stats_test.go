//go:build linux
// +build linux

package driver

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeGetVolumeStats(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	staged := filepath.Join(t.TempDir(), "staging")
	podPath := kubeletPublishPath(t, "pv-1")

	vol := NewVolume("vol-1", &fakeMounter{}, ns.Driver)
	vol.StagedPath = staged
	vol.AddPublishPath(podPath, false)
	ns.volumes.Store("vol-1", vol)

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	assertStatfsUsage(t, resp, podPath)

	unknownPodPath := kubeletPublishPath(t, "pv-unknown")
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-unknown",
		VolumePath: unknownPodPath,
	}); err != nil {
		t.Errorf("expected restart-style request to use a valid publish path, got %v", err)
	}

	restartedPodPath := kubeletPublishPath(t, "pv-restarted")
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: restartedPodPath,
	}); err != nil {
		t.Errorf("expected untracked restart-style publish path to be accepted, got %v", err)
	}

	otherPodPath := kubeletPublishPath(t, "pv-2")
	otherVol := NewVolume("vol-2", &fakeMounter{}, ns.Driver)
	otherVol.AddPublishPath(otherPodPath, false)
	ns.volumes.Store("vol-2", otherVol)
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: otherPodPath,
	}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for another tracked volume's path, got %v", err)
	}

	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: t.TempDir(),
	}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for untracked path, got %v", err)
	}

	missingPath := filepath.Join(t.TempDir(), "pods", "pod-a", "volumes", "kubernetes.io~csi", "pv-1", "mount")
	vol.AddPublishPath(missingPath, false)
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: missingPath,
	}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for missing path, got %v", err)
	}

	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumePath: staged,
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for empty volume ID, got %v", err)
	}
}

func TestNodeGetVolumeStatsRefusesUnhealthyPublishPath(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	podPath := kubeletPublishPath(t, "pv-1")
	vol := NewVolume("vol-1", &fakeMounter{}, ns.Driver)
	vol.AddPublishPath(podPath, false)
	ns.volumes.Store("vol-1", vol)

	var readCalled atomic.Bool
	ns.isHealthyFn = func(path string) bool { return false }
	ns.readVolumeUsageFn = func(path string) (*volumeUsage, error) {
		readCalled.Store(true)
		return &volumeUsage{}, nil
	}

	_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for unhealthy publish path, got %v", err)
	}
	if readCalled.Load() {
		t.Fatal("readVolumeUsage must not run after mount validation fails")
	}
}

func TestNodeGetVolumeStatsInodeSentinel(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	podPath := kubeletPublishPath(t, "pv-1")
	vol := NewVolume("vol-1", &fakeMounter{}, ns.Driver)
	vol.AddPublishPath(podPath, false)
	ns.volumes.Store("vol-1", vol)

	ns.readVolumeUsageFn = func(path string) (*volumeUsage, error) {
		return &volumeUsage{
			capacityBytes:  10,
			availableBytes: 7,
			inodes:         math.MaxInt64 - 1,
			inodesUsed:     2,
			inodesFree:     3,
		}, nil
	}
	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := findUsage(resp, csi.VolumeUsage_INODES); got == nil || got.Total != math.MaxInt64-1 {
		t.Fatalf("expected large real inode count to be reported, got %v", got)
	}

	ns.readVolumeUsageFn = func(path string) (*volumeUsage, error) {
		return &volumeUsage{capacityBytes: 10, availableBytes: 7, inodes: math.MaxInt64}, nil
	}
	resp, err = ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := findUsage(resp, csi.VolumeUsage_INODES); got != nil {
		t.Fatalf("expected MaxInt64 inode sentinel to be omitted, got %v", got)
	}
}

func TestNodeGetVolumeStatsDeduplicatesBlockedCollection(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	podPath := kubeletPublishPath(t, "pv-1")
	vol := NewVolume("vol-1", &fakeMounter{}, ns.Driver)
	vol.AddPublishPath(podPath, false)
	ns.volumes.Store("vol-1", vol)

	started := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	var readCalls atomic.Int32
	ns.readVolumeUsageFn = func(path string) (*volumeUsage, error) {
		readCalls.Add(1)
		once.Do(func() { close(started) })
		<-unblock
		return &volumeUsage{capacityBytes: 1, availableBytes: 1}, nil
	}
	defer close(unblock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err := ns.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	cancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("stats collection did not start")
	}

	secondCtx, secondCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err = ns.NodeGetVolumeStats(secondCtx, &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	secondCancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded while first collection is still active, got %v", err)
	}
	if got := readCalls.Load(); got != 1 {
		t.Fatalf("expected one stats collection, got %d", got)
	}
}

func TestNodeGetVolumeStatsDoesNotDeduplicateDifferentPaths(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	firstPath := kubeletPublishPath(t, "pv-1")
	secondPath := kubeletPublishPath(t, "pv-1")
	vol := NewVolume("vol-1", &fakeMounter{}, ns.Driver)
	vol.AddPublishPath(firstPath, false)
	vol.AddPublishPath(secondPath, false)
	ns.volumes.Store("vol-1", vol)

	startedFirst := make(chan struct{})
	unblockFirst := make(chan struct{})
	var startedOnce sync.Once
	var secondCalls atomic.Int32
	ns.readVolumeUsageFn = func(path string) (*volumeUsage, error) {
		switch filepath.Clean(path) {
		case filepath.Clean(firstPath):
			startedOnce.Do(func() { close(startedFirst) })
			<-unblockFirst
			return &volumeUsage{capacityBytes: 1, availableBytes: 1}, nil
		case filepath.Clean(secondPath):
			secondCalls.Add(1)
			return &volumeUsage{capacityBytes: 2, availableBytes: 2}, nil
		default:
			t.Errorf("unexpected stats path %s", path)
			return &volumeUsage{}, nil
		}
	}
	defer close(unblockFirst)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err := ns.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: firstPath,
	})
	cancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	select {
	case <-startedFirst:
	default:
		t.Fatal("first stats collection did not start")
	}

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: secondPath,
	})
	if err != nil {
		t.Fatalf("expected second path stats to succeed, got %v", err)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("expected second path to start its own collection, got %d calls", got)
	}
	if got := findUsage(resp, csi.VolumeUsage_BYTES); got == nil || got.Total != 2 {
		t.Fatalf("expected second path usage, got %v", got)
	}
}

func TestNodeGetVolumeStatsMutexWaitHonorsDeadline(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	podPath := kubeletPublishPath(t, "pv-1")
	vol := NewVolume("vol-1", &fakeMounter{}, ns.Driver)
	vol.AddPublishPath(podPath, false)
	ns.volumes.Store("vol-1", vol)

	volumeMutex := ns.getVolumeMutex("vol-1")
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := ns.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded waiting for volume mutex, got %v", err)
	}
}

func kubeletPublishPath(t *testing.T, pv string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pods", "pod-a", "volumes", "kubernetes.io~csi", pv, "mount")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir publish path: %v", err)
	}
	return path
}

func assertStatfsUsage(t *testing.T, resp *csi.NodeGetVolumeStatsResponse, path string) {
	t.Helper()
	bytesUsage := findUsage(resp, csi.VolumeUsage_BYTES)
	if bytesUsage == nil {
		t.Fatal("expected BYTES usage entry")
	}
	inodeUsage := findUsage(resp, csi.VolumeUsage_INODES)
	if inodeUsage == nil {
		t.Fatal("expected INODES usage entry")
	}

	var sfs syscall.Statfs_t
	if err := syscall.Statfs(path, &sfs); err != nil {
		t.Fatalf("statfs %s failed: %v", path, err)
	}
	if got, want := bytesUsage.Total, int64(sfs.Blocks)*int64(sfs.Bsize); got != want {
		t.Errorf("bytes total = %d, want %d", got, want)
	}
	if got, want := bytesUsage.Used, int64(sfs.Blocks-sfs.Bfree)*int64(sfs.Bsize); got != want {
		t.Errorf("bytes used = %d, want %d", got, want)
	}
	if got, want := bytesUsage.Available, int64(sfs.Bavail)*int64(sfs.Bsize); got != want {
		t.Errorf("bytes available = %d, want %d", got, want)
	}
	if got, want := inodeUsage.Total, int64(sfs.Files); got != want {
		t.Errorf("inodes total = %d, want %d", got, want)
	}
	if inodeUsage.Used < 0 {
		t.Errorf("inodes used = %d, want >= 0", inodeUsage.Used)
	}
}

func findUsage(resp *csi.NodeGetVolumeStatsResponse, unit csi.VolumeUsage_Unit) *csi.VolumeUsage {
	for _, usage := range resp.GetUsage() {
		if usage.Unit == unit {
			return usage
		}
	}
	return nil
}
