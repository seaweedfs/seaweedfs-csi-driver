package driver

import (
	"fmt"

	"github.com/chrislusf/seaweedfs/weed/glog"
)

// Implements Mounter
type seaweedFsMounter struct {
	bucketName string
	driver     *SeaweedFsDriver
}

const (
	seaweedFsCmd = "weed"
)

func newSeaweedFsMounter(bucketName string, driver *SeaweedFsDriver) (Mounter, error) {
	return &seaweedFsMounter{
		bucketName: bucketName,
		driver:   driver,
	}, nil
}

func (seaweedFs *seaweedFsMounter) Mount(target string) error {
	glog.V(0).Infof("mounting %s%s to %s", seaweedFs.driver.filer, seaweedFs.bucketName, target)

	args := []string{
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", target),
		fmt.Sprintf("-collection=%s", seaweedFs.bucketName),
		fmt.Sprintf("-filer=%s", seaweedFs.driver.filer),
		fmt.Sprintf("-filer.path=/buckets/%s", seaweedFs.bucketName),
	}
	if seaweedFs.driver.ConcurrentWriters > 0 {
		args = append(args, fmt.Sprintf("-concurrentWriters=%d", seaweedFs.driver.ConcurrentWriters))
	}
	err := fuseMount(target, seaweedFsCmd, args)
	if err != nil {
		glog.Errorf("mount %s %s to %s: %s", seaweedFs.driver.filer, seaweedFs.bucketName, target, err)
	}
	return err
}
