package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/k8s"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
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

	// The volume has been staged.
	if _, ok := ns.volumes.Load(volumeID); ok && ns.isStagingPathHealthy(stagingTargetPath) {
		glog.Infof("volume %s has been already staged", volumeID)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	volContext := req.GetVolumeContext()
	readOnly := isVolumeReadOnly(req)

	mounter, err := newMounter(volumeID, readOnly, ns.Driver, volContext)
	if err != nil {
		// node stage is unsuccessfull
		ns.removeVolumeMutex(volumeID)
		return nil, err
	}

	volume := NewVolume(volumeID, mounter)
	if err := volume.Stage(stagingTargetPath); err != nil {
		// node stage is unsuccessfull
		ns.removeVolumeMutex(volumeID)

		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	// In seaweedfs quota is not configured on seaweedfs servers.
	// Quota is applied only per mount.
	// Previously we used to cmdline parameter to apply it, but such way does not allow dynamic resizing.
	if capacity, err := k8s.GetVolumeCapacity(volumeID); err == nil {
		if err := volume.Quota(capacity); err != nil {
			return nil, err
		}
	} else {
		glog.Infof("orchestration system is not compatible with the k8s api, error is: %s", err)
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
	// Self-healing: check if staging path is healthy
	if ns.isStagingPathHealthy(stagingTargetPath) && !ok {
		// Rebuild volume cache from healthy staging path
		glog.Infof("Staging path %s is healthy, rebuilding volume cache for %s", stagingTargetPath, volumeID)
		rebuiltVolume, err := ns.rebuildVolumeFromStaging(volumeID, stagingTargetPath, req.GetVolumeContext())
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to rebuild volume cache for %s: %v", volumeID, err))
		}
		ns.volumes.Store(volumeID, rebuiltVolume)
		volume = rebuiltVolume
		glog.Infof("Self-healing completed for volume %s", volumeID)
	} else if !ns.isStagingPathHealthy(stagingTargetPath) {
		// Need to re-stage the volume - release mutex to avoid deadlock
		glog.Infof("Staging path %s is unhealthy, re-staging volume %s", stagingTargetPath, volumeID)
		volumeMutex.Unlock() // Release the mutex before re-staging
		err := ns.restageVolume(ctx, req)
		volumeMutex.Lock() // Re-acquire the mutex
		if err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to re-stage volume %s: %v", volumeID, err))
		}

		// Load the newly staged volume
		volume, ok = ns.volumes.Load(volumeID)
		if !ok {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Volume %s not found in cache after re-staging", volumeID))
		}
		glog.Infof("Self-healing completed for volume %s", volumeID)
	}

	// When pod uses a volume in read-only mode, k8s will automatically
	// mount the volume as a read-only file system.
	if err := volume.(*Volume).Publish(stagingTargetPath, targetPath, req.GetReadonly()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	glog.Infof("volume %s successfully published to %s", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
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
	ns.volumes.Range(func(_, vol any) bool {
		v := vol.(*Volume)
		if len(v.StagedPath) > 0 {
			glog.Infof("cleaning up volume %s at %s", v.VolumeId, v.StagedPath)
			err := v.Unstage(v.StagedPath)
			if err != nil {
				glog.Warningf("error cleaning up volume %s at %s, err: %v", v.VolumeId, v.StagedPath, err)
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

// isStagingPathHealthy checks if the staging path is a healthy mount point
func (ns *NodeServer) isStagingPathHealthy(stagingPath string) bool {
	// Check if path exists
	if _, err := os.Stat(stagingPath); err != nil {
		glog.V(4).Infof("Staging path %s does not exist: %v", stagingPath, err)
		return false
	}

	// Check if it's a mount point
	isMnt, err := checkMount(stagingPath)
	if err != nil {
		glog.V(4).Infof("Failed to check if %s is mount point: %v", stagingPath, err)
		return false
	}

	if isMnt {
		// If it's a mount point, check if it's accessible (detect stale mounts)
		if _, err := os.Stat(filepath.Join(stagingPath, ".")); err != nil {
			glog.V(4).Infof("Staging path %s is stale mount point: %v", stagingPath, err)
			return false
		}
		glog.V(4).Infof("Staging path %s is healthy mount point", stagingPath)
		return true
	} else {
		// For SeaweedFS, staging path should always be a mount point
		glog.V(4).Infof("Staging path %s is not a mount point", stagingPath)
		return false
	}
}

// rebuildVolumeFromStaging rebuilds Volume object from healthy staging path
func (ns *NodeServer) rebuildVolumeFromStaging(volumeID string, stagingPath string, volContext map[string]string) (*Volume, error) {
	glog.Infof("Rebuilding volume %s from staging path %s", volumeID, stagingPath)

	// Create a new mounter with the same configuration
	readOnly := false // We'll assume read-write by default since we can't easily determine this
	mounter, err := newMounter(volumeID, readOnly, ns.Driver, volContext)
	if err != nil {
		return nil, fmt.Errorf("failed to create mounter for volume %s: %v", volumeID, err)
	}

	// Create Volume object
	volume := NewVolume(volumeID, mounter)
	volume.StagedPath = stagingPath

	// We don't have the original unmounter, but that's OK for our use case
	// The unmounter will be set when needed during unstage operations

	glog.Infof("Successfully rebuilt volume %s from staging path %s", volumeID, stagingPath)
	return volume, nil
}

// restageVolume re-stages a volume by cleaning up and re-mounting
// This function should be called without holding the volume mutex to avoid deadlock
func (ns *NodeServer) restageVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	glog.Infof("Re-staging volume %s to %s", req.GetVolumeId(), req.GetStagingTargetPath())
	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	// Clean up any stale mounts
	if err := ns.cleanupStaleMount(stagingPath); err != nil {
		return fmt.Errorf("failed to cleanup stale mount for %s: %v", stagingPath, err)
	}
	// Create stage request from publish request
	stageReq := &csi.NodeStageVolumeRequest{
		VolumeId:          req.GetVolumeId(),
		StagingTargetPath: req.GetStagingTargetPath(),
		VolumeCapability:  req.GetVolumeCapability(),
		VolumeContext:     req.GetVolumeContext(),
		Secrets:           req.GetSecrets(),
	}
	// Call NodeStageVolume to re-stage
	_, err := ns.NodeStageVolume(ctx, stageReq)
	if err != nil {
		return fmt.Errorf("failed to re-stage volume %s: %v", volumeID, err)
	}
	glog.Infof("Successfully re-staged volume %s to %s", volumeID, stagingPath)
	return nil
}

// cleanupStaleMount cleans up stale mount points
func (ns *NodeServer) cleanupStaleMount(stagingPath string) error {
	glog.V(4).Infof("Cleaning up stale mount at %s", stagingPath)

	// Force unmount if it's a stale mount point
	if isMnt, _ := checkMount(stagingPath); isMnt {
		glog.Infof("Force unmounting stale mount point %s", stagingPath)
		if err := mountutil.Unmount(stagingPath); err != nil {
			// Try lazy unmount
			glog.Warningf("Normal unmount failed for %s, trying lazy unmount: %v", stagingPath, err)
			if err := mount.CleanupMountPoint(stagingPath, mountutil, true); err != nil {
				glog.Warningf("Lazy unmount also failed for %s: %v", stagingPath, err)
			}
		}
	}

	// Remove directory if it exists
	if _, err := os.Stat(stagingPath); err == nil {
		if err := os.RemoveAll(stagingPath); err != nil {
			return fmt.Errorf("failed to remove directory %s: %v", stagingPath, err)
		}
	}

	// Recreate directory
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", stagingPath, err)
	}

	glog.V(4).Infof("Successfully cleaned up stale mount at %s", stagingPath)
	return nil
}
