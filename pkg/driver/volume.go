package driver

import (
	"context"
	"fmt"
	"os"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/utils/mount"
)

type Volume struct {
	VolumeId   string
	StagedPath string

	mounter   Mounter
	unmounter Unmounter

	// unix socket used to manage volume
	localSocket string
}

func NewVolume(volumeID string, mounter Mounter) *Volume {
	return &Volume{
		VolumeId:    volumeID,
		mounter:     mounter,
		localSocket: GetLocalSocket(volumeID),
	}
}

func (vol *Volume) Stage(stagingTargetPath string) error {
	vol.StagedPath = stagingTargetPath

	// check whether it can be mounted
	if notMnt, err := checkMount(stagingTargetPath); err != nil {
		return err
	} else if !notMnt {
		// try to unmount before mounting again
		_ = mount.New("").Unmount(stagingTargetPath)
	}

	if u, err := vol.mounter.Mount(stagingTargetPath); err == nil {
		vol.unmounter = u
		return nil
	} else {
		return err
	}
}

func (vol *Volume) Publish(stagingTargetPath string, targetPath string, readOnly bool) error {
	// check whether it can be mounted
	if notMnt, err := checkMount(targetPath); err != nil {
		return err
	} else if !notMnt {
		// maybe already mounted?
		return nil
	}

	// Use bind mount to create an alias of the real mount point.
	mountOptions := []string{"bind"}
	if readOnly {
		mountOptions = append(mountOptions, "ro")
	}

	mounter := mount.New("")
	if err := mounter.Mount(stagingTargetPath, targetPath, "", mountOptions); err != nil {
		return err
	}

	return nil
}

func (vol *Volume) Expand(sizeByte int64) error {
	target := fmt.Sprintf("passthrough:///unix://%s", vol.localSocket)
	dialOption := grpc.WithTransportCredentials(insecure.NewCredentials())

	clientConn, err := grpc.Dial(target, dialOption)
	if err != nil {
		return err
	}
	defer clientConn.Close()

	client := mount_pb.NewSeaweedMountClient(clientConn)
	_, err = client.Configure(context.Background(), &mount_pb.ConfigureRequest{
		CollectionCapacity: sizeByte,
	})
	return err
}

func (vol *Volume) Unpublish(targetPath string) error {
	// Try unmounting target path and deleting it.
	mounter := mount.New("")
	if err := mount.CleanupMountPoint(targetPath, mounter, true); err != nil {
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
		glog.Infof("error unmounting volume during unstage: %s, err: %v", err)
		return err
	}

	if err := os.Remove(stagingTargetPath); err != nil && !os.IsNotExist(err) {
		glog.Infof("error removing staging path for volume %s at %s, err: %v", vol.VolumeId, stagingTargetPath, err)
		return err
	}

	return nil
}
