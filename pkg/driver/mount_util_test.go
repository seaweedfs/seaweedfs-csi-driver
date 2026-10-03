package driver

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"syscall"

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
// tempDirResolved returns a temp dir with symlinks resolved. The fake
// mounter's IsLikelyNotMountPoint compares EvalSymlinks(path) against the
// registered mount paths, so registering an unresolved path misses on
// platforms where the temp dir contains a symlink (e.g. /var ->
// /private/var on macOS).
func tempDirResolved(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

func TestIsStagingPathHealthy_DoesNotRequireListableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}

	dir := tempDirResolved(t)
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

// A dead FUSE daemon can keep answering os.Stat and the mount-point check
// from cached inode attributes; statfs always reaches the daemon, so its
// ENOTCONN is the reliable dead-mount signal.
func TestIsStagingPathHealthy_DeadMount(t *testing.T) {
	dir := tempDirResolved(t)
	stagingPath := filepath.Join(dir, "staging")
	if err := os.Mkdir(stagingPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	origMountutil := mountutil
	origStatfs := statfsFn
	mountutil = mount.NewFakeMounter([]mount.MountPoint{{Path: stagingPath}})
	defer func() {
		mountutil = origMountutil
		statfsFn = origStatfs
	}()

	statfsFn = func(string) error { return syscall.ENOTCONN }
	if isStagingPathHealthy(stagingPath) {
		t.Fatal("dead FUSE mount must not report healthy")
	}

	statfsFn = func(string) error { return nil }
	if !isStagingPathHealthy(stagingPath) {
		t.Fatal("live mount must report healthy")
	}
}

// A hung FUSE daemon can keep answering the cached stat checks while
// blocking statfs forever. The probe must stay bounded so callers holding
// the per-volume lock are not stalled, and repeated callers must reuse
// the single in-flight syscall instead of accumulating blocked
// goroutines.
func TestIsStagingPathHealthy_StatfsProbeTimeout(t *testing.T) {
	dir := tempDirResolved(t)
	stagingPath := filepath.Join(dir, "staging")
	if err := os.Mkdir(stagingPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	origMountutil := mountutil
	origStatfs := statfsFn
	origTimeout := statfsProbeTimeout
	mountutil = mount.NewFakeMounter([]mount.MountPoint{{Path: stagingPath}})
	statfsProbeTimeout = 50 * time.Millisecond
	defer func() {
		mountutil = origMountutil
		statfsFn = origStatfs
		statfsProbeTimeout = origTimeout
		resetStatfsProbe(stagingPath)
	}()

	release := make(chan struct{})
	defer close(release)
	var statfsCalls atomic.Int32
	statfsFn = func(string) error {
		statfsCalls.Add(1)
		<-release
		return nil
	}

	if isStagingPathHealthy(stagingPath) {
		t.Fatal("a mount whose statfs probe times out must not report healthy")
	}
	// While the first syscall is still blocked, a second caller must
	// attach to it rather than spawn another.
	if isStagingPathHealthy(stagingPath) {
		t.Fatal("expected unhealthy while the probe is still blocked")
	}
	if got := statfsCalls.Load(); got != 1 {
		t.Fatalf("expected 1 statfs syscall across two probes, got %d", got)
	}
}
