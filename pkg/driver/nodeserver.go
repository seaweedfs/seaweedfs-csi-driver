package driver

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"
)

type NodeServer struct {
	csi.UnimplementedNodeServer

	Driver *SeaweedFsDriver

	// information about the managed volumes
	volumes       sync.Map
	volumeMutexes *KeyMutex
}

var _ = csi.NodeServer(&NodeServer{})

func (ns *NodeServer) getVolume(volumeID string) *Volume {
	if volume, ok := ns.volumes.Load(volumeID); ok {
		return volume.(*Volume)
	}
	return nil
}

func (ns *NodeServer) setVolume(volumeID string, volume *Volume) {
	ns.volumes.Store(volumeID, volume)
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	// mount the fs here
	targetPath := req.GetTargetPath()

	glog.Infof("node target volume %s to %s", volumeID, targetPath)

	// Check arguments
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	if !isValidVolumeCapabilities(ns.Driver.vcap, []*csi.VolumeCapability{req.GetVolumeCapability()}) {
		// return nil, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	volume := ns.getVolume(volumeID)
	if volume == nil {
		volume = NewVolume(volumeID)
	}
	// The volume has been publish.
	for _, volumePath := range volume.volumePaths {
		if volumePath.path == targetPath {
			glog.Infof("volume %s has been already published", volumeID)
			return &csi.NodePublishVolumeResponse{}, nil
		}
	}

	volumePath := &VolumePath{path: targetPath, volumeId: volumeID}
	if mounter, err := newMounter(volumeID, isVolumeReadOnly(req), ns.Driver, req.GetVolumeContext()); err != nil {
		ns.removeVolumeMutex(volumeID)
		return nil, err
	} else {
		volumePath.mounter = mounter
		volume.volumePaths = append(volume.volumePaths, volumePath)
	}

	if err := volume.Publish(volumePath); err != nil {
		// node publish is unsuccessfull
		ns.removeVolumeMutex(volumeID)

		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	//k8s api get Capacity
	if capacity, err := k8s.GetVolumeCapacity(volumeID); err == nil {
		if err := volume.Quota(capacity); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	ns.setVolume(volumeID, volume)
	glog.Infof("volume %s successfully publish to %s", volumeID, targetPath)

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	glog.Infof("node unpublish volume %s from %s", volumeID, targetPath)

	// Check arguments
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	if volume := ns.getVolume(volumeID); volume != nil {
		if err := volume.Unpublish(targetPath); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		} else {
			if len(volume.volumePaths) == 0 {
				ns.volumes.Delete(volumeID)
			}
		}
	} else {
		glog.Warningf("volume %s hasn't been published", volumeID)

		// make sure there is no any garbage
		_ = mount.CleanupMountPoint(targetPath, mountutil, true)
	}

	// remove mutex on successfull unpublish
	ns.volumeMutexes.RemoveMutex(volumeID)

	glog.Infof("volume %s successfully unpublish from %s", volumeID, targetPath)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	glog.V(3).Infof("node get info, node id: %s", ns.Driver.nodeID)

	return &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.nodeID,
	}, nil
}

func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	glog.V(3).Infof("node get capabilities")
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (ns *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	requiredBytes := req.GetCapacityRange().GetRequiredBytes()

	glog.Infof("node expand volume %s to %d bytes", req.GetVolumeId(), requiredBytes)

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume path missing in request")
	}

	// TODO Check if volume exists
	// TODO Check if node exists

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	if volume := ns.getVolume(volumeID); volume != nil {
		if err := volume.Quota(requiredBytes); err != nil {
			return nil, err
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

func (ns *NodeServer) NodeCleanup() {
	ns.volumes.Range(func(_, vol any) bool {
		v := vol.(*Volume)
		for _, volumePath := range v.volumePaths {
			glog.Infof("cleaning up volume %s at %s", v.VolumeId, volumePath.path)
			err := v.Unpublish(volumePath.path)
			if err != nil {
				glog.Warningf("error cleaning up volume %s at %s, err: %v", v.VolumeId, volumePath.path, err)
			}
		}
		return true
	})
}

func (ns *NodeServer) getVolumeMutex(volumeID string) *sync.Mutex {
	return ns.volumeMutexes.GetMutex(volumeID)
}

func (ns *NodeServer) removeVolumeMutex(volumeID string) {
	ns.volumeMutexes.RemoveMutex(volumeID)
}

func isVolumeReadOnly(req *csi.NodePublishVolumeRequest) bool {
	mode := req.GetVolumeCapability().GetAccessMode().Mode

	readOnlyModes := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	}

	for _, readOnlyMode := range readOnlyModes {
		if mode == readOnlyMode {
			return true
		}
	}

	return false
}
