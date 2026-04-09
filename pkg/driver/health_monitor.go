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

func (ns *NodeServer) checkAndRecoverVolumes() {
	ns.volumes.Range(func(key, value interface{}) bool {
		volumeID := key.(string)
		vol := value.(*Volume)

		if vol.StagedPath == "" {
			return true
		}

		if ns.isHealthyFn(vol.StagedPath) {
			return true
		}

		glog.Warningf("health monitor: detected unhealthy mount for volume %s at %s", volumeID, vol.StagedPath)
		ns.recoverVolume(volumeID)
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
	// registered on the new volume — a subsequent health tick will see them
	// as unhealthy and retry recovery, instead of losing them forever.
	var failed []publishInfo
	for _, p := range publishes {
		glog.Infof("health monitor: re-publishing %s for volume %s", p.path, volumeID)
		if err := newVol.Publish(stagingPath, p.path, p.readOnly); err != nil {
			glog.Errorf("health monitor: failed to re-publish %s for volume %s: %v", p.path, volumeID, err)
			failed = append(failed, p)
		}
		// Always re-register the path so it is retried on the next sweep
		// if it failed, and so it is unmounted correctly on Unpublish.
		newVol.AddPublishPath(p.path, p.readOnly)
	}

	// Step 5: Replace the volume in the map
	ns.volumes.Store(volumeID, newVol)
	if len(failed) > 0 {
		glog.Warningf("health monitor: volume %s recovered with %d publish path failure(s); will retry on next sweep", volumeID, len(failed))
		return
	}
	glog.Infof("health monitor: volume %s successfully recovered", volumeID)
}
