package driver

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// newSweepTestNodeServer builds a NodeServer wired for orphan-sweep tests with
// an isolated plugin root, an injectable unmount behaviour and an injectable
// mount-point classifier (so tests never depend on the host mount table).
func newSweepTestNodeServer(t *testing.T, pluginRoot string, unmountErr error) *NodeServer {
	t.Helper()
	return &NodeServer{
		Driver: &SeaweedFsDriver{
			KubeletPluginsDir: pluginRoot,
		},
		volumeMutexes: NewKeyMutex(),
		stopCh:        make(chan struct{}),
		unmountFn: func(path string) error {
			return unmountErr
		},
		// Default: nothing is a mount point (the residual-directory case).
		isMountPointFn: func(path string) (bool, error) { return false, nil },
	}
}

// makeGlobalMount creates <root>/<driver>/<hash>/globalmount and returns the
// globalmount path. These are plain directories (not real mounts), which is
// exactly the "residual directory, nothing mounted" case the sweep must handle.
func makeGlobalMount(t *testing.T, root, driverName, hash string) string {
	t.Helper()
	gm := filepath.Join(root, "kubernetes.io", "csi", driverName, hash, globalMountSuffix)
	if err := os.MkdirAll(gm, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", gm, err)
	}
	// Backdate past the sweep grace period: a freshly created dir may belong
	// to an in-flight stage and is left alone (see sweepGracePeriod).
	old := time.Now().Add(-sweepGracePeriod - time.Minute)
	if err := os.Chtimes(gm, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", gm, err)
	}
	return gm
}

func withStatfs(t *testing.T, fn func(string) error) {
	t.Helper()
	orig := statfsFn
	statfsFn = fn
	t.Cleanup(func() { statfsFn = orig })
}

// fakeDeadMount keeps gm classified as a mount until unmountFn detaches it;
// mntErr is what the mount-point check reports while it is attached.
func fakeDeadMount(ns *NodeServer, mntErr error) {
	var attached atomic.Bool
	attached.Store(true)
	ns.unmountFn = func(string) error {
		attached.Store(false)
		return nil
	}
	ns.isMountPointFn = func(string) (bool, error) {
		if !attached.Load() {
			return false, nil
		}
		return mntErr == nil, mntErr
	}
}

// Without openat2 the mount-point check stats the mount root, so a dead FUSE
// mount (the one that fails the kubelet's mkdir) reports ENOTCONN there.
func TestSweepRemovesDeadMountReportedByMountCheck(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "enotconn")

	ns := newSweepTestNodeServer(t, root, nil)
	fakeDeadMount(ns, syscall.ENOTCONN)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); !os.IsNotExist(err) {
		t.Fatalf("dead globalmount %s must be removed, stat err = %v", gm, err)
	}
}

// Cached inode attributes keep a dead mount looking attached; statfs reaches
// the daemon and reports it dead.
func TestSweepRemovesMountWithDeadStatfs(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "deadstatfs")
	withStatfs(t, func(string) error { return syscall.ENOTCONN })

	ns := newSweepTestNodeServer(t, root, nil)
	fakeDeadMount(ns, nil)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); !os.IsNotExist(err) {
		t.Fatalf("dead globalmount %s must be removed, stat err = %v", gm, err)
	}
}

func TestSweepSkipsMountWhoseStatfsHangs(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "hung")
	origStatfs, origTimeout := statfsFn, statfsProbeTimeout
	release := make(chan struct{})
	statfsFn = func(string) error {
		<-release
		return nil
	}
	statfsProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		probeStatfs(gm) // joins the blocked probe before statfsFn is restored
		statfsFn, statfsProbeTimeout = origStatfs, origTimeout
	})

	ns := newSweepTestNodeServer(t, root, nil)
	ns.isMountPointFn = func(string) (bool, error) {
		t.Error("mount-point check ran after the statfs probe timed out")
		return false, nil
	}
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); err != nil {
		t.Fatalf("globalmount %s with a hung probe must be preserved, stat err = %v", gm, err)
	}
}

// TestSweepRemovesUnownedResidualGlobalMount is the core regression: a leftover
// globalmount directory with nothing mounted and no owning volume must be
// removed, otherwise the kubelet fails forever with
// "mkdir .../globalmount: file exists".
func TestSweepRemovesUnownedResidualGlobalMount(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "abc123")

	ns := newSweepTestNodeServer(t, root, nil)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); !os.IsNotExist(err) {
		t.Fatalf("expected residual globalmount %s to be removed, stat err = %v", gm, err)
	}
}

// TestSweepKeepsGlobalMountOwnedByTrackedVolume guards the safety invariant:
// a globalmount belonging to a volume present in ns.volumes must never be
// touched, even when the unmount hook would happily "succeed".
func TestSweepKeepsGlobalMountOwnedByTrackedVolume(t *testing.T) {
	root := t.TempDir()
	hash := "ownedhash"
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", hash)

	ns := newSweepTestNodeServer(t, root, nil)
	ns.volumes.Store("/buckets/pvc-owned", &Volume{
		VolumeId:   "/buckets/pvc-owned",
		StagedPath: filepath.Join("/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi", hash, "mount"),
	})

	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); err != nil {
		t.Fatalf("owned globalmount %s must be preserved, stat err = %v", gm, err)
	}
}

// A custom kubelet root that happens to contain an orphan's directory name must
// not make every tracked volume look like its owner.
func TestSweepOwnershipMatchesWholeHashDir(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "kube")

	ns := newSweepTestNodeServer(t, root, nil)
	ns.volumes.Store("/buckets/pvc-other", &Volume{
		VolumeId:   "/buckets/pvc-other",
		StagedPath: "/var/lib/kube/plugins/kubernetes.io/csi/seaweedfs-csi-driver/cccc/globalmount",
	})
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); !os.IsNotExist(err) {
		t.Fatalf("orphan %s must be removed, stat err = %v", gm, err)
	}
}

func TestSweepKeepsGlobalMountNamedAfterTrackedVolumeID(t *testing.T) {
	root := t.TempDir()
	volumeID := "/buckets/pvc-hashed"
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", fmt.Sprintf("%x", sha256.Sum256([]byte(volumeID))))

	ns := newSweepTestNodeServer(t, root, nil)
	ns.volumes.Store(volumeID, &Volume{VolumeId: volumeID, StagedPath: "/staging/elsewhere/globalmount"})
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); err != nil {
		t.Fatalf("globalmount of tracked volume %s must be preserved, stat err = %v", volumeID, err)
	}
}

// TestSweepKeepsLiveMount is the data-safety test: an untracked globalmount
// that IS a live mount point must never be removed. RemoveAll through a live
// FUSE mount would unlink user data.
func TestSweepKeepsLiveMount(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "livehash")

	ns := newSweepTestNodeServer(t, root, nil)
	// This path is a mount point AND the statfs probe says it is alive.
	ns.isMountPointFn = func(path string) (bool, error) { return true, nil }

	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); err != nil {
		t.Fatalf("live globalmount %s must be preserved, stat err = %v", gm, err)
	}
}

// TestSweepHandlesMultipleVolumes covers a realistic node: several hashes, some
// owned, some residual.
func TestSweepHandlesMultipleVolumes(t *testing.T) {
	root := t.TempDir()
	ownedHash := "aaaa"
	residualHash := "bbbb"
	owned := makeGlobalMount(t, root, "seaweedfs-csi-driver", ownedHash)
	residual := makeGlobalMount(t, root, "seaweedfs-csi-driver", residualHash)

	ns := newSweepTestNodeServer(t, root, nil)
	ns.volumes.Store("/buckets/pvc-a", &Volume{
		VolumeId:   "/buckets/pvc-a",
		StagedPath: filepath.Join("/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi", ownedHash, "mount"),
	})

	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("owned globalmount must survive, stat err = %v", err)
	}
	if _, err := os.Stat(residual); !os.IsNotExist(err) {
		t.Fatalf("residual globalmount must be removed, stat err = %v", err)
	}
}

// TestSweepDisabledWithoutPluginDir asserts the sweep is a no-op unless the
// operator opts in by setting the kubelet plugins dir.
func TestSweepDisabledWithoutPluginDir(t *testing.T) {
	ns := newSweepTestNodeServer(t, "", nil)
	// Must not panic and must not attempt anything.
	ns.sweepOrphanGlobalMounts()
}

// TestSweepIgnoresNonGlobalMountDirs verifies the glob only targets the
// globalmount leaf and never sibling directories (e.g. vol_data.json siblings).
func TestSweepIgnoresNonGlobalMountDirs(t *testing.T) {
	root := t.TempDir()
	hashDir := filepath.Join(root, "seaweedfs-csi-driver", "somehash")
	if err := os.MkdirAll(hashDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sibling := filepath.Join(hashDir, "not-a-globalmount")
	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ns := newSweepTestNodeServer(t, root, nil)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling dir %s must be left alone, stat err = %v", sibling, err)
	}
}

func withLazyUnmount(t *testing.T, fn func(string) error) {
	t.Helper()
	orig := lazyUnmount
	lazyUnmount = fn
	t.Cleanup(func() { lazyUnmount = orig })
}

// makeVolumeFile puts a file in gm and backdates gm past the grace period.
func makeVolumeFile(t *testing.T, gm string) string {
	t.Helper()
	f := filepath.Join(gm, "data")
	if err := os.WriteFile(f, []byte("user data"), 0o600); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}
	old := time.Now().Add(-sweepGracePeriod - time.Minute)
	if err := os.Chtimes(gm, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", gm, err)
	}
	return f
}

func TestRemoveOrphanGlobalMountRefusesStillMountedPath(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "stuck")
	f := makeVolumeFile(t, gm)
	withLazyUnmount(t, func(string) error { return nil })

	ns := newSweepTestNodeServer(t, root, nil)
	ns.isMountPointFn = func(string) (bool, error) { return true, nil }

	if err := ns.removeOrphanGlobalMount(gm); err == nil {
		t.Fatal("expected an error for a path that stays mounted")
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("file under a still-mounted globalmount must survive, stat err = %v", err)
	}
}

func TestRemoveOrphanGlobalMountStopsOnRecheckError(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "recheck")
	withLazyUnmount(t, func(string) error { return syscall.EINVAL })

	ns := newSweepTestNodeServer(t, root, syscall.EBUSY)
	ns.isMountPointFn = func(string) (bool, error) { return false, syscall.EIO }

	if err := ns.removeOrphanGlobalMount(gm); err == nil {
		t.Fatal("expected the recheck error to be returned")
	}
	if _, err := os.Stat(gm); err != nil {
		t.Fatalf("globalmount with an unknown mount state must survive, stat err = %v", err)
	}
}

// A residual directory is removed with rmdir, so files in it (or under a mount
// that lands after classification) are never unlinked.
func TestSweepKeepsNonEmptyResidualGlobalMount(t *testing.T) {
	root := t.TempDir()
	gm := makeGlobalMount(t, root, "seaweedfs-csi-driver", "nonempty")
	f := makeVolumeFile(t, gm)

	ns := newSweepTestNodeServer(t, root, nil)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(f); err != nil {
		t.Fatalf("file under a residual globalmount must survive, stat err = %v", err)
	}
}

// TestGlobalMountDirsMatchesRealKubeletLayout pins the on-disk layout the
// kubelet actually uses, observed in production as:
//
//	/var/lib/kubelet/plugins/kubernetes.io/csi/seaweedfs-csi-driver/<hash>/globalmount
//
// A previous revision globbed root/*/*/globalmount, which silently matched
// nothing against that four-level path and made the whole sweep a no-op.
func TestGlobalMountDirsMatchesRealKubeletLayout(t *testing.T) {
	root := t.TempDir()
	real := makeGlobalMount(t, root, "seaweedfs-csi-driver", "deadbeef")

	got := globalMountDirs(root, "")
	if len(got) != 1 || got[0] != real {
		t.Fatalf("globalMountDirs missed the real kubelet layout: got %v, want [%s]", got, real)
	}
}

// TestSweepIgnoresOtherDriverGlobalMounts guards the safety property that the
// sweep never touches globalmounts belonging to a different CSI driver sharing
// the same plugins dir (for example the cinder driver in front of the control
// plane PVs).
func TestSweepIgnoresOtherDriverGlobalMounts(t *testing.T) {
	root := t.TempDir()
	foreign := makeGlobalMount(t, root, "cinder.csi.openstack.org", "foreignhash")

	ns := newSweepTestNodeServer(t, root, nil)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("sweep removed another driver's globalmount: %v", err)
	}
}

// TestSweepKeepsRecentResidualGlobalMount pins the race guard: a globalmount
// directory created moments ago may belong to a NodeStageVolume still in
// flight (the kubelet mkdirs the directory before the FUSE mount lands, and the
// driver only tracks the volume after the mount succeeds). Removing it would
// break a healthy concurrent mount.
func TestSweepKeepsRecentResidualGlobalMount(t *testing.T) {
	root := t.TempDir()
	gm := filepath.Join(root, "kubernetes.io", "csi", "seaweedfs-csi-driver", "freshhash", globalMountSuffix)
	if err := os.MkdirAll(gm, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ns := newSweepTestNodeServer(t, root, nil)
	ns.sweepOrphanGlobalMounts()

	if _, err := os.Stat(gm); err != nil {
		t.Fatalf("sweep removed a freshly created globalmount (grace period not honoured): %v", err)
	}
}
