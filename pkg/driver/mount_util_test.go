package driver

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/mount-utils"
)

// isStagingPathHealthy must not require the mount root to be listable.
// seaweedfs mount answers a readdir of the root by fetching the *entire*
// directory listing from the filer before returning anything to the
// kernel; on a bucket with a large root that can take many minutes even
// though the mount is perfectly healthy. Simulating "unlistable" with a
// 0-permission directory: os.Stat still succeeds (stat only needs search
// permission on the parent), but os.ReadDir would fail with EACCES. If
// isStagingPathHealthy regresses to calling ReadDir again, this test
// catches it (as root, os.ReadDir ignores permission bits, so this test
// must not be run as root).
func TestIsStagingPathHealthy_DoesNotRequireListableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}

	dir := t.TempDir()
	stagingPath := filepath.Join(dir, "staging")
	if err := os.Mkdir(stagingPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(stagingPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() {
		// t.TempDir cleanup needs it readable again
		if err := os.Chmod(stagingPath, 0o755); err != nil {
			t.Errorf("chmod cleanup: %v", err)
		}
	}()

	if _, err := os.ReadDir(stagingPath); err == nil {
		t.Fatalf("test setup invalid: ReadDir on a 0-permission dir should fail")
	}

	origMountutil := mountutil
	mountutil = mount.NewFakeMounter([]mount.MountPoint{{Path: stagingPath}})
	defer func() { mountutil = origMountutil }()

	if !isStagingPathHealthy(stagingPath) {
		t.Fatal("expected staging path to be healthy: liveness must not depend on the root directory being listable")
	}
}

func TestIsStagingPathHealthy_MissingPath(t *testing.T) {
	if isStagingPathHealthy(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("expected missing staging path to be unhealthy")
	}
}

func TestIsStagingPathHealthy_NotAMountPoint(t *testing.T) {
	dir := t.TempDir()

	origMountutil := mountutil
	mountutil = mount.NewFakeMounter(nil) // no mount points registered
	defer func() { mountutil = origMountutil }()

	if isStagingPathHealthy(dir) {
		t.Fatal("expected a plain directory that isn't a mount point to be unhealthy")
	}
}
