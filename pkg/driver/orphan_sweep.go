package driver

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

// Orphan globalmount sweep.
//
// Background: the CSI "globalmount" directory is created by the KUBELET
// (attacher.MountDevice), not by this driver. When a mount service pod or a
// node plugin restarts while volumes are staged, the FUSE daemon dies but the
// kernel mount and the globalmount directory survive. The kubelet then retries
// MountDevice forever:
//
//	MountVolume.MountDevice failed ... mkdir .../globalmount: file exists
//
// The driver is never called in that path, so neither NodeStageVolume nor the
// per-volume health monitor can repair it: the health monitor only walks
// ns.volumes, and after a restart that table is empty.
//
// This sweep closes that gap. It scans the kubelet plugin directory for
// globalmount paths that no tracked volume owns, probes each one, and removes
// only the ones whose FUSE daemon is provably dead.

const (
	// defaultOrphanSweepInterval is deliberately slower than the per-volume
	// health interval: an orphan globalmount only blocks NEW pod starts, so a
	// minute of latency is acceptable and keeps statfs traffic low.
	defaultOrphanSweepInterval = 60 * time.Second

	// sweepGracePeriod skips residual directories created moments ago: the
	// kubelet creates the globalmount dir before the FUSE mount lands, and
	// NodeStageVolume only registers the volume in ns.volumes after the mount
	// completes, so a freshly created directory may belong to an in-flight
	// stage rather than to a dead mount service.
	sweepGracePeriod = 2 * defaultOrphanSweepInterval
)

// globalMountSuffix is the leaf directory the kubelet creates per volume.
const globalMountSuffix = "globalmount"

// defaultDriverName matches the --driverName default in cmd/; used when the
// driver was constructed without a name (tests).
const defaultDriverName = "seaweedfs-csi-driver"

// globalMountDirs returns the per-volume globalmount paths the kubelet uses for
// this driver. Depending on how the driver registered, the kubelet creates them
// under one of two layouts:
//
//	<root>/kubernetes.io/csi/<driverName>/<hash>/globalmount
//	<root>/<driverName>/<hash>/globalmount
//
// Restricting the scan to this driver's own directory is a safety property:
// other CSI drivers (cinder, etc.) create their own globalmounts under the same
// plugins dir and this sweep must never touch them.
func globalMountDirs(root, driverName string) []string {
	if driverName == "" {
		driverName = defaultDriverName
	}
	var out []string
	for _, base := range []string{
		filepath.Join(root, "kubernetes.io", "csi", driverName),
		filepath.Join(root, driverName),
	} {
		matches, err := filepath.Glob(filepath.Join(base, "*", globalMountSuffix))
		if err != nil {
			glog.Warningf("orphan sweep: glob under %s failed: %v", base, err)
			continue
		}
		out = append(out, matches...)
	}
	return out
}

// isMountPoint reports whether path is a mount point, using the injectable
// seam when set (tests) and the real mount table otherwise.
func (ns *NodeServer) isMountPoint(path string) (bool, error) {
	if ns.isMountPointFn != nil {
		return ns.isMountPointFn(path)
	}
	return mountutil.IsMountPoint(path)
}

// sweepOrphanGlobalMounts removes globalmount directories that are not owned by
// any tracked volume and whose mount is dead.
//
// Safety invariants (mirroring cleanupStaleStagingPath):
//   - never remove a path that is still a mount point after unmount attempts;
//   - never remove a path owned by a volume present in ns.volumes;
//   - a statfs probe timeout is inconclusive and treated as "live".
func (ns *NodeServer) sweepOrphanGlobalMounts() {
	root := ns.Driver.KubeletPluginsDir
	if root == "" {
		return
	}

	// Only this driver's own subtree is scanned, so globalmounts belonging to
	// other CSI drivers sharing the plugins dir are never considered.
	for _, gm := range globalMountDirs(root, ns.Driver.name) {
		ns.sweepGlobalMount(gm)
	}
}

// sweepGlobalMount handles one globalmount: a residual directory is removed, a
// dead mount is detached and removed, and a live or unclassifiable one is left.
func (ns *NodeServer) sweepGlobalMount(gm string) {
	if ns.isGlobalMountOwned(gm) {
		return
	}
	// statfs reaches the FUSE daemon and is bounded, so it runs before the
	// mount-table check and a hung daemon never stalls the sweep.
	statfsErr, probed := probeStatfs(gm)
	if !probed {
		glog.Warningf("orphan sweep: statfs probe for %s timed out, treating the mount as live", gm)
		return
	}
	if statfsErr == nil {
		isMnt, err := ns.isMountPoint(gm)
		switch {
		case err == nil && isMnt:
			return
		case err == nil:
			// Grace period: a freshly created dir may belong to a stage still
			// in flight (kubelet mkdirs the globalmount before the FUSE mount
			// lands, and the volume is only tracked after it succeeds).
			if info, statErr := os.Stat(gm); statErr == nil && time.Since(info.ModTime()) < sweepGracePeriod {
				glog.V(2).Infof("orphan sweep: skipping recent residual %s (within grace period)", gm)
				return
			}
			glog.Warningf("orphan sweep: removing residual globalmount dir %s (not mounted, no tracked volume owns it)", gm)
			if err := os.RemoveAll(gm); err != nil {
				glog.Errorf("orphan sweep: failed to remove residual %s: %v", gm, err)
			}
			return
		case !mount.IsCorruptedMnt(err):
			glog.Warningf("orphan sweep: cannot classify %s: %v", gm, err)
			return
		}
	} else if !mount.IsCorruptedMnt(statfsErr) {
		glog.Warningf("orphan sweep: cannot classify %s: %v", gm, statfsErr)
		return
	}
	glog.Warningf("orphan sweep: removing dead globalmount %s (no tracked volume owns it)", gm)
	if err := ns.removeOrphanGlobalMount(gm); err != nil {
		glog.Errorf("orphan sweep: failed to remove %s: %v", gm, err)
	}
}

// isGlobalMountOwned reports whether gm belongs to a volume currently tracked in
// ns.volumes. Ownership is matched on the volume hash directory name, which is
// the same sha256(volumeID) the kubelet and the driver both derive.
func (ns *NodeServer) isGlobalMountOwned(gm string) bool {
	hashDir := filepath.Base(filepath.Dir(gm))
	owned := false
	ns.volumes.Range(func(_, value interface{}) bool {
		vol, ok := value.(*Volume)
		if !ok || vol == nil {
			return true
		}
		if vol.StagedPath == "" {
			return true
		}
		// The staging path contains the same hash directory the kubelet uses.
		if strings.Contains(vol.StagedPath, hashDir) {
			owned = true
			return false
		}
		return true
	})
	return owned
}

// removeOrphanGlobalMount unmounts (lazily if needed) then removes a dead
// globalmount directory. It refuses to delete anything still mounted.
func (ns *NodeServer) removeOrphanGlobalMount(gm string) error {
	if err := ns.unmountFn(gm); err != nil {
		glog.Warningf("orphan sweep: unmount %s failed: %v, trying lazy detach", gm, err)
		if lazyErr := mountmanager.LazyUnmount(gm); lazyErr != nil {
			glog.Warningf("orphan sweep: lazy unmount %s failed: %v", gm, lazyErr)
		}
	}

	// Re-check: never RemoveAll through a live FUSE mount.
	if isMnt, err := ns.isMountPoint(gm); err == nil && isMnt {
		// One more attempt, then give up rather than risk deleting user data.
		if lazyErr := mountmanager.LazyUnmount(gm); lazyErr != nil {
			return nil // leave it for the next sweep
		}
		if isMnt2, err2 := ns.isMountPoint(gm); err2 == nil && isMnt2 {
			glog.Warningf("orphan sweep: %s still mounted after lazy detach, skipping removal", gm)
			return nil
		}
	}

	if err := os.RemoveAll(gm); err != nil {
		return err
	}
	glog.Infof("orphan sweep: removed %s", gm)
	return nil
}

// startOrphanSweeper runs the orphan sweep on its own ticker, independent of the
// per-volume health monitor.
func (ns *NodeServer) startOrphanSweeper(interval time.Duration) {
	if ns.Driver.KubeletPluginsDir == "" {
		glog.Infof("orphan sweeper disabled: kubelet plugins dir not configured")
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		glog.Infof("orphan globalmount sweeper started with interval %v (root=%s)", interval, ns.Driver.KubeletPluginsDir)
		// Run once shortly after start so a restart repairs leftover zombies
		// without waiting a full interval.
		time.Sleep(10 * time.Second)
		ns.runOrphanSweepTick()
		for {
			select {
			case <-ticker.C:
				ns.runOrphanSweepTick()
			case <-ns.stopCh:
				glog.Infof("orphan globalmount sweeper stopped")
				return
			}
		}
	}()
}

// runOrphanSweepTick wraps the sweep with panic recovery so a single bad path
// cannot disable self-healing for the process lifetime.
func (ns *NodeServer) runOrphanSweepTick() {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("orphan sweep: recovered from panic: %v", r)
		}
	}()
	ns.sweepOrphanGlobalMounts()
}
