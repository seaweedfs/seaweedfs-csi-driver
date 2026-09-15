//go:build linux
// +build linux

package driver

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestNodeGetVolumeStats(t *testing.T) {
	ns := NewNodeServer(&SeaweedFsDriver{})
	staged := t.TempDir()
	podPath := t.TempDir()

	ns.volumes.Store("vol-1", NewVolume("vol-1", &fakeMounter{}, ns.Driver))
	vol, _ := ns.volumes.Load("vol-1")
	vol.(*Volume).StagedPath = staged

	// The kubelet passes the per-pod published path, which differs from the
	// staging path: usage must be reported for that path.
	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: podPath,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	var bytesUsage, inodeUsage *csi.VolumeUsage
	for _, u := range resp.Usage {
		switch u.Unit {
		case csi.VolumeUsage_BYTES:
			bytesUsage = u
		case csi.VolumeUsage_INODES:
			inodeUsage = u
		}
	}
	if bytesUsage == nil {
		t.Fatal("expected BYTES usage entry, got none")
	}

	var sfs syscall.Statfs_t
	if err := syscall.Statfs(podPath, &sfs); err != nil {
		t.Fatalf("statfs %s failed: %v", podPath, err)
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
	if inodeUsage == nil {
		t.Fatal("expected INODES usage entry, got none")
	}
	if got, want := inodeUsage.Total, int64(sfs.Files); got != want {
		t.Errorf("inodes total = %d, want %d", got, want)
	}
	if inodeUsage.Used < 0 {
		t.Errorf("inodes used = %d, want >= 0", inodeUsage.Used)
	}

	// Unknown volume: kubelet keeps polling across plugin restarts, so stats
	// must still work off the kubelet-provided path (metric continuity).
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-unknown",
		VolumePath: podPath,
	}); err != nil {
		t.Errorf("expected success for unknown volume with valid path, got %v", err)
	}

	// Nonexistent path: must be NotFound.
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-1",
		VolumePath: filepath.Join(podPath, "missing"),
	}); err == nil {
		t.Error("expected NotFound for missing path, got nil")
	} else if got := err.Error(); !strings.Contains(got, "NotFound") {
		t.Errorf("expected NotFound, got %v", err)
	}

	// Missing argument: must be InvalidArgument.
	if _, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumePath: staged,
	}); err == nil {
		t.Error("expected InvalidArgument for empty volume ID, got nil")
	}
}
