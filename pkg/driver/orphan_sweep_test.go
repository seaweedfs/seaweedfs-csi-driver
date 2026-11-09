package driver

import (
	"os"
	"path/filepath"
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

// TestRemoveOrphanGlobalMountRefusesStillMountedPath documents the refusal
// contract: a path that stays a mount point after unmount attempts is left in
// place rather than deleted through a live mount. Behavioural coverage lives in
// TestSweepKeepsLiveMount.
func TestRemoveOrphanGlobalMountRefusesStillMountedPath(t *testing.T) {
	t.Skip("covered structurally by TestSweepKeepsLiveMount")
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
