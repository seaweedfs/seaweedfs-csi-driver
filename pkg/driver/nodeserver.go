package driver

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/resolver/passthrough"
	"google.golang.org/grpc/status"
	"k8s.io/utils/mount"
)

type NodeServer struct {
	Driver  *SeaweedFsDriver
	mounter mount.Interface

	// information about the managed volumes
	volumes       sync.Map
	volumeMutexes *KeyMutex
}

var _ = csi.NodeServer(&NodeServer{})

func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	// mount the fs here
	stagingTargetPath := req.GetStagingTargetPath()
	glog.V(0).Infof("node stage volume %s to %s", volumeID, stagingTargetPath)

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
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	// The volume has been staged.
	if _, ok := ns.volumes.Load(volumeID); ok {
		glog.V(0).Infof("volume %s has been staged", volumeID)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	volContext := req.GetVolumeContext()
	readOnly := isVolumeReadOnly(req)

	mounter, err := newMounter(volumeID, readOnly, ns.Driver, volContext)
	if err != nil {
		return nil, err
	}

	volume := NewVolume(volumeID, mounter)
	if err := volume.Stage(stagingTargetPath); err != nil {
		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	ns.volumes.Store(volumeID, volume)
	glog.V(0).Infof("volume %s successfully staged to %s", volumeID, stagingTargetPath)

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	glog.V(0).Infof("node publish volume %s to %s", volumeID, targetPath)

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

	volume, ok := ns.volumes.Load(volumeID)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "volume hasn't been staged yet")
	}

	// When pod uses a volume in read-only mode, k8s will automatically
	// mount the volume as a read-only file system.
	if err := volume.(*Volume).Publish(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	glog.V(0).Infof("volume %s successfully published to %s", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	glog.V(0).Infof("node unpublish volume %s from %s", volumeID, targetPath)

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	volume, ok := ns.volumes.Load(volumeID)
	if !ok {
		glog.Warningf("volume %s hasn't been published", volumeID)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err := volume.(*Volume).Unpublish(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	glog.V(3).Infof("Using default NodeGetInfo: nodeID %s", ns.Driver.nodeID)

	return &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.nodeID,
	}, nil
}

func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	glog.V(3).Infof("Using default NodeGetCapabilities")

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						// Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
						//Type: csi.NodeServiceCapability_RPC_UNKNOWN,
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, in *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	glog.V(0).Infof("node unstage volume %s from %s", volumeID, stagingTargetPath)

	// Check arguments
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	volume, ok := ns.volumes.Load(volumeID)
	if !ok {
		glog.Warningf("volume %s hasn't been staged", volumeID)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	if err := volume.(*Volume).Unstage(stagingTargetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	} else {
		ns.volumes.Delete(volumeID)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	glog.V(0).Infof("Node expand volume %s to %d bytes", volumeID, requiredBytes)

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	if volume, ok := ns.volumes.Load(volumeID); ok {
		if err := volume.(*Volume).Expand(requiredBytes); err != nil {
			return nil, err
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

func (ns *NodeServer) getVolumeMutex(volumeID string) *sync.RWMutex {
	return ns.volumeMutexes.GetMutex(volumeID)
}

func isVolumeReadOnly(req *csi.NodeStageVolumeRequest) bool {
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
