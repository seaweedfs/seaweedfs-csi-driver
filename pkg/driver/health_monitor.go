package driver

import (
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
				ns.checkAndRecoverVolumes()
			case <-ns.stopCh:
				glog.Infof("health monitor stopped")
				return
			}
		}
	}()
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

	// Step 1: Unmount all stale bind (publish) mounts
	for _, p := range publishes {
		glog.Infof("health monitor: unmounting stale publish path %s for volume %s", p.path, volumeID)
		if err := ns.unmountFn(p.path); err != nil {
			glog.Warningf("health monitor: unmount publish path %s failed: %v, trying force cleanup", p.path, err)
			_ = mount.CleanupMountPoint(p.path, mountutil, true)
		}
	}

	// Step 2: Clean up stale staging path
	if err := ns.cleanupStagingFn(stagingPath); err != nil {
		glog.Errorf("health monitor: failed to cleanup stale staging for volume %s: %v", volumeID, err)
		return
	}

	// Step 3: Re-stage the volume with a fresh FUSE mount
	newVol, err := ns.stageNewVolume(volumeID, stagingPath, vol.volContext, vol.readOnly)
	if err != nil {
		glog.Errorf("health monitor: failed to re-stage volume %s: %v", volumeID, err)
		return
	}

	// Step 4: Preserve recovery metadata on the new volume
	newVol.volContext = vol.volContext
	newVol.readOnly = vol.readOnly

	// Step 5: Re-bind all publish paths
	for _, p := range publishes {
		glog.Infof("health monitor: re-publishing %s for volume %s", p.path, volumeID)
		if err := newVol.Publish(stagingPath, p.path, p.readOnly); err != nil {
			glog.Errorf("health monitor: failed to re-publish %s for volume %s: %v", p.path, volumeID, err)
		} else {
			newVol.AddPublishPath(p.path, p.readOnly)
		}
	}

	// Step 6: Replace the volume in the map
	ns.volumes.Store(volumeID, newVol)
	glog.Infof("health monitor: volume %s successfully recovered", volumeID)
}
