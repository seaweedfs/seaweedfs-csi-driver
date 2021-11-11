package driver

import (
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"strings"
)

// Implements Mounter
type seaweedFsMounter struct {
	path       string
	collection string
	readOnly   bool
	driver     *SeaweedFsDriver
	volContext map[string]string
}

const (
	seaweedFsCmd = "weed"
)

func newSeaweedFsMounter(path string, collection string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
	return &seaweedFsMounter{
		path:       path,
		collection: collection,
		readOnly:   readOnly,
		driver:     driver,
		volContext: volContext,
	}, nil
}

func (seaweedFs *seaweedFsMounter) Mount(target string) error {
	glog.V(0).Infof("mounting %v %s to %s", seaweedFs.driver.filers, seaweedFs.path, target)

	var filers []string
	for _, address := range seaweedFs.driver.filers {
		filers = append(filers, string(address))
	}

	args := []string{
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", target),
		fmt.Sprintf("-collection=%s", seaweedFs.collection),
		fmt.Sprintf("-filer=%s", strings.Join(filers, ",")),
		fmt.Sprintf("-filer.path=%s", seaweedFs.path),
		fmt.Sprintf("-cacheCapacityMB=%d", seaweedFs.driver.CacheSizeMB),
	}

	// came from https://github.com/seaweedfs/seaweedfs-csi-driver/pull/12
	// preferring explicit settings
	// keeping this for backward compatibility
	for arg, value := range seaweedFs.volContext {
		switch arg {
		case "map.uid":
			args = append(args, fmt.Sprintf("-map.uid=%s", value))
		case "map.gid":
			args = append(args, fmt.Sprintf("-map.gid=%s", value))
		case "replication":
			args = append(args, fmt.Sprintf("-replication=%s", value))
		}
	}

	if seaweedFs.readOnly {
		args = append(args, "-readOnly")
	}

	if seaweedFs.driver.ConcurrentWriters > 0 {
		args = append(args, fmt.Sprintf("-concurrentWriters=%d", seaweedFs.driver.ConcurrentWriters))
	}
	if seaweedFs.driver.CacheDir != "" {
		args = append(args, fmt.Sprintf("-cacheDir=%s", seaweedFs.driver.CacheDir))
	}
	if seaweedFs.driver.UidMap != "" {
		args = append(args, fmt.Sprintf("-map.uid=%s", seaweedFs.driver.UidMap))
	}
	if seaweedFs.driver.GidMap != "" {
		args = append(args, fmt.Sprintf("-map.gid=%s", seaweedFs.driver.GidMap))
	}

	err := fuseMount(target, seaweedFsCmd, args)
	if err != nil {
		glog.Errorf("mount %v %s to %s: %s", seaweedFs.driver.filers, seaweedFs.path, target, err)
	}
	return err
}
