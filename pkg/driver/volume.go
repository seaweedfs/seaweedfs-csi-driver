package driver

import (
	"context"
	"fmt"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/mount-utils"
)

type VolumePath struct {
	path      string
	mounter   Mounter
	unmounter Unmounter
	volumeId  string
}

type Volume struct {
	VolumeId string

	// unix socket used to manage volume
	localSocket string
	volumePaths []*VolumePath
}

func NewVolume(volumeID string) *Volume {
	return &Volume{
		VolumeId:    volumeID,
		localSocket: GetLocalSocket(volumeID),
	}
}

func (vol *Volume) Publish(volumePath *VolumePath) error {
	// check whether it can be mounted
	if isMnt, err := checkMount(volumePath.path); err != nil {
		return err
	} else if isMnt {
		// try to unmount before mounting again
		_ = mountutil.Unmount(volumePath.path)
	}

	if unmounter, err := volumePath.mounter.Mount(volumePath.path); err == nil {
		volumePath.unmounter = unmounter
		vol.volumePaths = append(vol.volumePaths, volumePath)
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
	for index, volumePath := range vol.volumePaths {
		if volumePath.path == targetPath {
			vol.volumePaths = append(vol.volumePaths[:index], vol.volumePaths[index+1:]...)
			if volumePath.unmounter != nil {
				err := volumePath.unmounter.Unmount()
				if err != nil {
					glog.Errorf("error unmounting volume during unstage: %s, err: %v", targetPath, err)
				} else { // unmount success
					return nil
				}
			} else {
				glog.Errorf("volume %s is no mounter, path: %s", vol.VolumeId, targetPath)
			}
			break
		}
	}
	glog.Warningf("volume %s cannot use unmounter, use default cleanup mount point %s", targetPath, vol.VolumeId)
	return mount.CleanupMountPoint(targetPath, mountutil, true)
}
