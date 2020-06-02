package driver

import (
	"fmt"
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
	args := []string{
		"mount",
		fmt.Sprintf("-dir=%s", target),
		fmt.Sprintf("-collection=%s", seaweedFs.bucketName),
		fmt.Sprintf("-filer=%s", seaweedFs.filerUrl),
		fmt.Sprintf("-filer.path=/buckets/%s", seaweedFs.bucketName),
	}
	return fuseMount(target, seaweedFsCmd, args)
}
