package driver

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
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

func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	// mount the fs here
	stagingTargetPath := req.GetStagingTargetPath()

	glog.Infof("node stage volume %s to %s", volumeID, stagingTargetPath)

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

	// The volume has been staged and is in memory cache.
	if _, ok := ns.volumes.Load(volumeID); ok {
		glog.Infof("volume %s has been already staged", volumeID)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Phase 2 Enhancement: Check if staging path exists but is stale/corrupted
	// This handles cases where:
	// 1. The CSI driver restarted and lost its in-memory state
	// 2. The FUSE process died leaving a stale mount
	if isStagingPathHealthy(stagingTargetPath) {
		// The staging path is healthy - rebuild the cache from the existing mount
		// This preserves the existing FUSE mount and avoids disrupting any published volumes
		glog.Infof("volume %s has existing healthy mount at %s, rebuilding cache", volumeID, stagingTargetPath)
		volume := ns.rebuildVolumeFromStaging(volumeID, stagingTargetPath)
		ns.volumes.Store(volumeID, volume)
		glog.Infof("volume %s cache rebuilt from existing staging at %s", volumeID, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Check if there's a stale/corrupted mount that needs cleanup
	if _, err := os.Stat(stagingTargetPath); err == nil || mount.IsCorruptedMnt(err) {
		glog.Infof("volume %s has stale staging path at %s, cleaning up", volumeID, stagingTargetPath)
		if err := cleanupStaleStagingPath(stagingTargetPath); err != nil {
			ns.removeVolumeMutex(volumeID)
			return nil, status.Errorf(codes.Internal, "failed to cleanup stale staging path %s: %v", stagingTargetPath, err)
		}
	}

	volContext := req.GetVolumeContext()
	readOnly := isVolumeReadOnly(req)

	volume, err := ns.stageNewVolume(volumeID, stagingTargetPath, volContext, readOnly)
	if err != nil {
		// node stage is unsuccessful
		ns.removeVolumeMutex(volumeID)

		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	ns.volumes.Store(volumeID, volume)
	glog.Infof("volume %s successfully staged to %s", volumeID, stagingTargetPath)

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	stagingTargetPath := req.GetStagingTargetPath()

	glog.Infof("node publish volume %s to %s", volumeID, targetPath)

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
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	volume, ok := ns.volumes.Load(volumeID)
	if !ok {
		// Phase 1: Self-healing for missing volume cache
		// This handles the case where the CSI driver restarted and lost its in-memory state,
		// but kubelet thinks the volume is already staged and directly calls NodePublishVolume.
		glog.Infof("volume %s not found in cache, attempting self-healing", volumeID)

		if isStagingPathHealthy(stagingTargetPath) {
			// The staging path is healthy - rebuild volume cache from existing mount
			glog.Infof("volume %s has healthy staging at %s, rebuilding cache", volumeID, stagingTargetPath)
			volume = ns.rebuildVolumeFromStaging(volumeID, stagingTargetPath)
			ns.volumes.Store(volumeID, volume)
		} else {
			// The staging path is not healthy - we need to re-stage the volume
			// This requires volume context which we have from the request
			glog.Infof("volume %s staging path %s is not healthy, re-staging", volumeID, stagingTargetPath)

			// Clean up stale staging path if it exists
			if err := cleanupStaleStagingPath(stagingTargetPath); err != nil {
				ns.removeVolumeMutex(volumeID)
				return nil, status.Errorf(codes.Internal, "failed to cleanup stale staging path %s: %v", stagingTargetPath, err)
			}

			// Re-stage the volume using the shared helper
			volContext := req.GetVolumeContext()
			readOnly := isPublishVolumeReadOnly(req)

			newVolume, err := ns.stageNewVolume(volumeID, stagingTargetPath, volContext, readOnly)
			if err != nil {
				ns.removeVolumeMutex(volumeID)
				return nil, status.Errorf(codes.Internal, "failed to re-stage volume: %v", err)
			}

			ns.volumes.Store(volumeID, newVolume)
			volume = newVolume
			glog.Infof("volume %s successfully re-staged to %s", volumeID, stagingTargetPath)
		}
	}

	// When pod uses a volume in read-only mode, k8s will automatically
	// mount the volume as a read-only file system.
	if err := volume.(*Volume).Publish(stagingTargetPath, targetPath, req.GetReadonly()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	glog.Infof("volume %s successfully published to %s", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

// rebuildVolumeFromStaging creates a Volume struct from an existing healthy staging mount.
// This is used for self-healing when the CSI driver restarts but the FUSE mount is still active.
// Note: The returned Volume won't have an unmounter, so Unstage will need special handling.
func (ns *NodeServer) rebuildVolumeFromStaging(volumeID string, stagingPath string) *Volume {
	return &Volume{
		VolumeId:    volumeID,
		StagedPath:  stagingPath,
		driver:      ns.Driver,
		localSocket: mountmanager.LocalSocketPath(ns.Driver.volumeSocketDir, volumeID),
		// mounter and unmounter are nil - this is intentional
		// The FUSE process is already running, we just need to track the volume
		// The mount service will have the mount tracked if it's still alive
	}
}

// isPublishVolumeReadOnly determines if a volume should be mounted read-only based on the publish request.
func isPublishVolumeReadOnly(req *csi.NodePublishVolumeRequest) bool {
	if req.GetReadonly() {
		return true
	}
	cap := req.GetVolumeCapability()
	if cap == nil || cap.GetAccessMode() == nil {
		return false
	}
	return isReadOnlyAccessMode(cap.GetAccessMode().Mode)
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	glog.Infof("node unpublish volume %s from %s", volumeID, targetPath)

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

		// make sure there is no any garbage
		_ = mount.CleanupMountPoint(targetPath, mountutil, true)

		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err := volume.(*Volume).Unpublish(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	glog.Infof("volume %s successfully unpublished from %s", volumeID, targetPath)

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
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
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

func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	glog.Infof("node unstage volume %s from %s", volumeID, stagingTargetPath)

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

		// make sure there is no any garbage
		_ = mount.CleanupMountPoint(stagingTargetPath, mountutil, true)

		// Also clean up cache directory and socket if they exist
		CleanupVolumeResources(ns.Driver, volumeID)
	} else {
		if err := volume.(*Volume).Unstage(stagingTargetPath); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		} else {
			ns.volumes.Delete(volumeID)
		}
	}

	// remove mutex on successfull unstage
	ns.volumeMutexes.RemoveMutex(volumeID)

	glog.Infof("volume %s successfully unstaged from %s", volumeID, stagingTargetPath)

	return &csi.NodeUnstageVolumeResponse{}, nil
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

	if volume, ok := ns.volumes.Load(volumeID); ok {
		if err := volume.(*Volume).Quota(requiredBytes); err != nil {
			return nil, err
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

func (ns *NodeServer) NodeCleanup() {
	glog.Infof("node cleanup skipped; mount service retains mounts across restarts")
}

func (ns *NodeServer) getVolumeMutex(volumeID string) *sync.Mutex {
	return ns.volumeMutexes.GetMutex(volumeID)
}

func (ns *NodeServer) removeVolumeMutex(volumeID string) {
	ns.volumeMutexes.RemoveMutex(volumeID)
}

// stageNewVolume creates and stages a new volume with the given parameters.
// This is a helper method used by both NodeStageVolume and NodePublishVolume (for re-staging).
func (ns *NodeServer) stageNewVolume(volumeID, stagingTargetPath string, volContext map[string]string, readOnly bool) (*Volume, error) {
	mounter, err := newMounter(volumeID, readOnly, ns.Driver, volContext)
	if err != nil {
		return nil, err
	}

	volume := NewVolume(volumeID, mounter, ns.Driver)
	if err := volume.Stage(stagingTargetPath); err != nil {
		return nil, err
	}

	// Apply quota if available
	if capacity, err := k8s.GetVolumeCapacity(volumeID); err == nil {
		if err := volume.Quota(capacity); err != nil {
			glog.Warningf("failed to apply quota for volume %s: %v", volumeID, err)
			// Clean up the staged mount since we're returning an error
			if unstageErr := volume.Unstage(stagingTargetPath); unstageErr != nil {
				glog.Errorf("failed to unstage volume %s after quota failure: %v", volumeID, unstageErr)
			}
			return nil, err
		}
	} else {
		glog.V(4).Infof("orchestration system is not compatible with the k8s api, error is: %s", err)
	}

	return volume, nil
}

// isReadOnlyAccessMode checks if the given access mode is read-only.
func isReadOnlyAccessMode(mode csi.VolumeCapability_AccessMode_Mode) bool {
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

func isVolumeReadOnly(req *csi.NodeStageVolumeRequest) bool {
	cap := req.GetVolumeCapability()
	if cap == nil || cap.GetAccessMode() == nil {
		return false
	}
	return isReadOnlyAccessMode(cap.GetAccessMode().Mode)
}
