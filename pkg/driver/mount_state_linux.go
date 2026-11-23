package driver

import "os"

func currentFuseMounts() (map[string]string, error) {
	entries, err := parseMountInfo(os.Getpid())
	if err != nil {
		return nil, err
	}
	mounts := make(map[string]string)
	for _, entry := range entries {
		// Drop an earlier FUSE entry if the path has since been covered
		// by a different filesystem.
		if isFuseFS(entry.fstype) {
			mounts[entry.mountpoint] = entry.device
		} else {
			delete(mounts, entry.mountpoint)
		}
	}
	return mounts, nil
}
