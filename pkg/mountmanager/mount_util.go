package mountmanager

import (
	"errors"
	"os"
	"time"

	"k8s.io/mount-utils"
)

var mountutil = mount.New("")

func waitForMount(path string, timeout time.Duration) error {
	var elapsed time.Duration
	var interval = 10 * time.Millisecond
	for {
		notMount, err := mountutil.IsLikelyNotMountPoint(path)
		if err != nil {
			return err
		}
		if !notMount {
			return nil
		}
		time.Sleep(interval)
		elapsed = elapsed + interval
		if elapsed >= timeout {
			return errors.New("timeout waiting for mount")
		}
	}
}

func ensureTargetClean(targetPath string) error {
	isMount, err := mountutil.IsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(targetPath, 0750)
		}
		if mount.IsCorruptedMnt(err) {
			if err := mountutil.Unmount(targetPath); err != nil {
				return err
			}
			return ensureTargetClean(targetPath)
		}
		return err
	}
	if isMount {
		if err := mountutil.Unmount(targetPath); err != nil {
			return err
		}
	}
	return nil
}
