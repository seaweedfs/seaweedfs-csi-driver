package driver

import (
	"fmt"

	"github.com/chrislusf/seaweedfs/weed/glog"
)

// Implements Mounter
type seaweedFsMounter struct {
	bucketName      string
	filerUrl        string
}

const (
	seaweedFsCmd = "weed"
)

func newSeaweedFsMounter(bucketName string, filer string) (Mounter, error) {
	return &seaweedFsMounter{
		bucketName:      bucketName,
		filerUrl:        filer,
	}, nil
}

func (seaweedFs *seaweedFsMounter) Mount(target string) error {
	glog.V(0).Infof("mounting %s%s to %s", seaweedFs.filerUrl, seaweedFs.bucketName, target)

	args := []string{
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", target),
		fmt.Sprintf("-collection=%s", seaweedFs.bucketName),
		fmt.Sprintf("-filer=%s", seaweedFs.filerUrl),
		fmt.Sprintf("-filer.path=/buckets/%s", seaweedFs.bucketName),
	}
	err := fuseMount(target, seaweedFsCmd, args)
	if err != nil {
		glog.Errorf("mount %s%s to %s: %s", seaweedFs.filerUrl, seaweedFs.bucketName, target, err)
	}
	return err
}
