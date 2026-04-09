package driver

import (
	"runtime/debug"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

const defaultHealthCheckInterval = 30 * time.Second

func (ns *NodeServer) startHealthMonitor(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		glog.Infof("health monitor started with interval %v", interval)
		for {
			select {
			case <-ticker.C:
				ns.runHealthCheckTick()
			case <-ns.stopCh:
				glog.Infof("health monitor stopped")
				return
			}
		}
	}()
}

// runHealthCheckTick runs one health check pass with panic recovery so
// that an unexpected crash inside checkAndRecoverVolumes or recoverVolume
// does not silently disable self-healing for the lifetime of the pod.
func (ns *NodeServer) runHealthCheckTick() {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("health monitor: recovered from panic: %v\n%s", r, debug.Stack())
		}
	}()
	ns.checkAndRecoverVolumes()
}

// checkAndRecoverVolumes inspects every staged volume and kicks off
// recovery work for any that have a dead staging mount or a dropped
// publish bind mount. Recovery is launched per-volume in its own goroutine
// so that a slow unmount or FUSE start for one volume does not stall the
// sweep for the others. The per-volume mutex taken inside recoverVolume /
// retryPublishPaths still prevents concurrent recoveries for the same id.
func (ns *NodeServer) checkAndRecoverVolumes() {
	ns.volumes.Range(func(key, value interface{}) bool {
		volumeID := key.(string)
		vol := value.(*Volume)

		if vol.StagedPath == "" {
			return true
		}

		if !ns.isHealthyFn(vol.StagedPath) {
			glog.Warningf("health monitor: detected unhealthy staging mount for volume %s at %s", volumeID, vol.StagedPath)
			ns.launchRecovery(func() { ns.recoverVolume(volumeID) }, volumeID, "full-recovery")
			return true
		}

		// Staging is alive; check whether any publish bind mounts have
		// been dropped (e.g. from a previous partial recovery) and need
		// to be re-bound without tearing down the FUSE mount.
		if ns.hasUnhealthyPublishPath(vol) {
			glog.Warningf("health monitor: detected unhealthy publish mount for volume %s", volumeID)
			ns.launchRecovery(func() { ns.retryPublishPaths(volumeID) }, volumeID, "publish-retry")
		}
		return true
	})
}

// hasUnhealthyPublishPath returns true if any of the Volume's tracked
// publish paths is not currently a live, readable mount point. It uses
// the same isHealthyFn as staging so behavior stays consistent across
// both mount levels.
func (ns *NodeServer) hasUnhealthyPublishPath(vol *Volume) bool {
	unhealthy := false
	vol.publishPaths.Range(func(k, _ interface{}) bool {
		if !ns.isHealthyFn(k.(string)) {
			unhealthy = true
			return false
		}
		return true
	})
	return unhealthy
}

// launchRecovery runs fn in a goroutine with panic recovery. Tests can
// wait on ns.recoveryWg after driving a sweep to synchronize on
// in-flight recovery work.
func (ns *NodeServer) launchRecovery(fn func(), volumeID, kind string) {
	ns.recoveryWg.Add(1)
	go func() {
		defer ns.recoveryWg.Done()
		defer func() {
			if r := recover(); r != nil {
				glog.Errorf("health monitor: %s for volume %s panicked: %v\n%s", kind, volumeID, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// retryPublishPaths re-binds publish paths whose bind mount has gone
// missing while the underlying staging FUSE mount is still alive. This
// is the second-chance path for publish failures that happened during a
// previous recoverVolume sweep.
func (ns *NodeServer) retryPublishPaths(volumeID string) {
	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	val, ok := ns.volumes.Load(volumeID)
	if !ok {
		return
	}
	vol := val.(*Volume)

	// If staging has died since the sweep, let the next tick's
	// full-recovery path handle it instead of fighting the race here.
	if !ns.isHealthyFn(vol.StagedPath) {
		glog.Infof("health monitor: staging for volume %s became unhealthy before publish retry; deferring to full recovery", volumeID)
		return
	}

	vol.publishPaths.Range(func(k, v interface{}) bool {
		path := k.(string)
		readOnly := v.(bool)
		if ns.isHealthyFn(path) {
			return true
		}
		glog.Warningf("health monitor: re-binding publish path %s for volume %s", path, volumeID)
		// Clear any stale/corrupt mount first so Publish's checkMount
		// does not short-circuit on a leftover mount.
		if err := ns.unmountFn(path); err != nil {
			if cleanupErr := mount.CleanupMountPoint(path, mountutil, true); cleanupErr != nil {
				glog.Warningf("health monitor: force cleanup of publish path %s failed: %v", path, cleanupErr)
			}
		}
		if err := vol.Publish(vol.StagedPath, path, readOnly); err != nil {
			glog.Errorf("health monitor: failed to re-bind publish path %s for volume %s: %v", path, volumeID, err)
			return true
		}
		glog.Infof("health monitor: successfully re-bound publish path %s for volume %s", path, volumeID)
		return true
	})
}

func (ns *NodeServer) recoverVolume(volumeID string) {
	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	// Re-load from map after acquiring lock (another goroutine may have replaced it)
	val, ok := ns.volumes.Load(volumeID)
	if !ok {
		glog.Infof("health monitor: volume %s no longer exists, skipping recovery", volumeID)
		return
	}
	vol := val.(*Volume)

	// Re-check health after acquiring lock
	if ns.isHealthyFn(vol.StagedPath) {
		glog.Infof("health monitor: volume %s is now healthy, skipping recovery", volumeID)
		return
	}

	if vol.volContext == nil {
		glog.Warningf("health monitor: cannot recover volume %s - no volume context available (volume was rebuilt from existing mount after CSI driver restart)", volumeID)
		return
	}

	stagingPath := vol.StagedPath

	// Collect publish paths before recovery
	type publishInfo struct {
		path     string
		readOnly bool
	}
	var publishes []publishInfo
	vol.publishPaths.Range(func(k, v interface{}) bool {
		publishes = append(publishes, publishInfo{k.(string), v.(bool)})
		return true
	})

	glog.Infof("health monitor: recovering volume %s (%d publish paths)", volumeID, len(publishes))

	// Step 1: Unmount all stale bind (publish) mounts. Surface forced
	// cleanup errors — a leftover bind mount can fool Publish() into
	// short-circuiting and silently claim success for a still-broken path.
	for _, p := range publishes {
		glog.Infof("health monitor: unmounting stale publish path %s for volume %s", p.path, volumeID)
		if err := ns.unmountFn(p.path); err != nil {
			glog.Warningf("health monitor: unmount publish path %s failed: %v, trying force cleanup", p.path, err)
			if cleanupErr := mount.CleanupMountPoint(p.path, mountutil, true); cleanupErr != nil {
				glog.Errorf("health monitor: force cleanup of publish path %s for volume %s also failed: %v", p.path, volumeID, cleanupErr)
			}
		}
	}

	// Step 2: Clean up stale staging path
	if err := ns.cleanupStagingFn(stagingPath); err != nil {
		glog.Errorf("health monitor: failed to cleanup stale staging for volume %s: %v", volumeID, err)
		return
	}

	// Step 3: Re-stage the volume with a fresh FUSE mount. stageNewVolume
	// populates volContext/readOnly on the new Volume for us.
	newVol, err := ns.stageNewVolume(volumeID, stagingPath, vol.volContext, vol.readOnly)
	if err != nil {
		glog.Errorf("health monitor: failed to re-stage volume %s: %v", volumeID, err)
		return
	}

	// Step 4: Re-bind all publish paths. Track any failures so they remain
	// registered on the new volume — the next sweep will notice they are
	// unhealthy via hasUnhealthyPublishPath and drive retryPublishPaths.
	var failed []publishInfo
	for _, p := range publishes {
		glog.Infof("health monitor: re-publishing %s for volume %s", p.path, volumeID)
		if err := newVol.Publish(stagingPath, p.path, p.readOnly); err != nil {
			glog.Errorf("health monitor: failed to re-publish %s for volume %s: %v", p.path, volumeID, err)
			failed = append(failed, p)
		}
		// Always re-register the path so retryPublishPaths can see it
		// on the next sweep and so Unpublish can tear it down correctly.
		newVol.AddPublishPath(p.path, p.readOnly)
	}

	// Step 5: Replace the volume in the map
	ns.volumes.Store(volumeID, newVol)
	if len(failed) > 0 {
		glog.Warningf("health monitor: volume %s recovered with %d publish path failure(s); retryPublishPaths will retry on the next sweep", volumeID, len(failed))
		return
	}
	glog.Infof("health monitor: volume %s successfully recovered", volumeID)
}
