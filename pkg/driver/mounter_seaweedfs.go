package driver

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/util"
)

// Implements Mounter
type seaweedFsMounter struct {
	volumeID   string
	path       string
	collection string
	readOnly   bool
	driver     *SeaweedFsDriver
	volContext map[string]string
}

const (
	seaweedFsCmd = "weed"
)

func newSeaweedFsMounter(volumeID string, path string, collection string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
	return &seaweedFsMounter{
		volumeID:   volumeID,
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
		"-logtostderr=true",
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", target),
		fmt.Sprintf("-collection=%s", seaweedFs.collection),
		fmt.Sprintf("-filer=%s", strings.Join(filers, ",")),
		fmt.Sprintf("-filer.path=%s", seaweedFs.path),
		fmt.Sprintf("-cacheCapacityMB=%d", seaweedFs.driver.CacheSizeMB),
		fmt.Sprintf("-localSocket=%s", GetLocalSocket(seaweedFs.volumeID)),
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
		case "diskType":
			args = append(args, fmt.Sprintf("-disk=%s", value))
		case "volumeCapacity":
			capacityMB := parseVolumeCapacity(value)
			args = append(args, fmt.Sprintf("-collectionQuotaMB=%d", capacityMB))
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

func GetLocalSocket(volumeID string) string {
	montDirHash := util.HashToInt32([]byte(volumeID))
	if montDirHash < 0 {
		montDirHash = -montDirHash
	}

	socket := fmt.Sprintf("/tmp/seaweedfs-mount-%d.sock", montDirHash)
	return socket
}

func parseVolumeCapacity(volumeCapacity string) int64 {
	var capacity int64

	if vCap, err := strconv.ParseInt(volumeCapacity, 10, 64); err != nil {
		glog.Errorf("volumeCapacity %s can not be parsed to Int64, err is: %v", volumeCapacity, err)
	} else {
		capacity = vCap
	}

	capacityMB := capacity / 1024 / 1024
	return capacityMB
}
