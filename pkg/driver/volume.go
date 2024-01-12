package driver

import (
	"context"
	"fmt"
	"os"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Volume struct {
	VolumeId   string
	TargetPath string

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

func (vol *Volume) Publish(targetPath string) error {
	// check whether it can be mounted
	if isMnt, err := checkMount(targetPath); err != nil {
		return err
	} else if isMnt {
		// try to unmount before mounting again
		_ = mountutil.Unmount(targetPath)
	}

	if u, err := vol.mounter.Mount(targetPath); err == nil {
		if vol.TargetPath != "" {
			if vol.TargetPath == targetPath {
				glog.Warningf("target path is already set to %s for volume %s", vol.TargetPath, vol.VolumeId)
			} else {
				glog.Warningf("target path is already set to %s and differs from %s for volume %s", vol.TargetPath, targetPath, vol.VolumeId)
			}
		}
		vol.TargetPath = targetPath
		vol.unmounter = u
		return nil
	} else {
		return err
	}
}

func (vol *Volume) Quota(sizeByte int64) error {
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
	glog.V(0).Infof("unmounting volume %s from %s", vol.VolumeId, targetPath)

	if vol.unmounter == nil {
		glog.Errorf("volume is not mounted: %s, path: %s", vol.VolumeId, targetPath)
		return nil
	}

	if targetPath != vol.TargetPath {
		glog.Warningf("staging path %s differs for volume %s at %s", targetPath, vol.VolumeId, vol.TargetPath)
	}

	if err := vol.unmounter.Unmount(); err != nil {
		glog.Errorf("error unmounting volume during unstage: %s, err: %v", targetPath, err)
		return err
	}

	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		glog.Errorf("error removing staging path for volume %s at %s, err: %v", vol.VolumeId, targetPath, err)
		return err
	}

	return nil
}
