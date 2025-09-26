package driver

import (
	"os"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

type Volume struct {
	VolumeId   string
	StagedPath string

	mounter   Mounter
	unmounter Unmounter
	driver    *SeaweedFsDriver
}

func NewVolume(volumeID string, mounter Mounter, driver *SeaweedFsDriver) *Volume {
	return &Volume{
		VolumeId: volumeID,
		mounter:  mounter,
		driver:   driver,
	}
}

func (vol *Volume) Stage(stagingTargetPath string) error {
	// check whether it can be mounted
	if isMnt, err := checkMount(stagingTargetPath); err != nil {
		return err
	} else if isMnt {
		// try to unmount before mounting again
		_ = mountutil.Unmount(stagingTargetPath)
	}

	if u, err := vol.mounter.Mount(stagingTargetPath); err == nil {
		if vol.StagedPath != "" {
			if vol.StagedPath == stagingTargetPath {
				glog.Warningf("staged path is already set to %s for volume %s", vol.StagedPath, vol.VolumeId)
			} else {
				glog.Warningf("staged path is already set to %s and differs from %s for volume %s", vol.StagedPath, stagingTargetPath, vol.VolumeId)
			}
		}

		vol.StagedPath = stagingTargetPath
		vol.unmounter = u

		return nil
	} else {
		return err
	}
}

func (vol *Volume) Publish(stagingTargetPath string, targetPath string, readOnly bool) error {
	// check whether it can be mounted
	if isMnt, err := checkMount(targetPath); err != nil {
		return err
	} else if isMnt {
		// maybe already mounted?
		return nil
	}

	// Use bind mount to create an alias of the real mount point.
	mountOptions := []string{"bind"}
	if readOnly {
		mountOptions = append(mountOptions, "ro")
	}

	if err := mountutil.Mount(stagingTargetPath, targetPath, "", mountOptions); err != nil {
		return err
	}

	return nil
}

func (vol *Volume) Quota(sizeByte int64) error {
	client, err := mountmanager.NewClient(vol.driver.mountEndpoint)
	if err != nil {
		return err
	}

	// We can't create PV of zero size, so we're using quota of 1 byte to define no quota.
	if sizeByte == 1 {
		sizeByte = 0
	}

	_, err = client.Configure(&mountmanager.ConfigureRequest{
		VolumeID:           vol.VolumeId,
		CollectionCapacity: sizeByte,
	})
	return err
}

func (vol *Volume) Unpublish(targetPath string) error {
	// Try unmounting target path and deleting it.
	if err := mount.CleanupMountPoint(targetPath, mountutil, true); err != nil {
		return err
	}

	return nil
}

func (vol *Volume) Unstage(stagingTargetPath string) error {
	glog.V(0).Infof("unmounting volume %s from %s", vol.VolumeId, stagingTargetPath)

	if vol.unmounter == nil {
		glog.Errorf("volume is not mounted: %s, path: %s", vol.VolumeId, stagingTargetPath)
		return nil
	}

	if stagingTargetPath != vol.StagedPath {
		glog.Warningf("staging path %s differs for volume %s at %s", stagingTargetPath, vol.VolumeId, vol.StagedPath)
	}

	if err := vol.unmounter.Unmount(); err != nil {
		glog.Errorf("error unmounting volume during unstage: %s, err: %v", stagingTargetPath, err)
		return err
	}

	if err := os.Remove(stagingTargetPath); err != nil && !os.IsNotExist(err) {
		glog.Errorf("error removing staging path for volume %s at %s, err: %v", vol.VolumeId, stagingTargetPath, err)
		return err
	}

	return nil
}
