package driver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3bucket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var unsafeVolumeIdChars = regexp.MustCompile(`[^-.a-zA-Z0-9]`)

type ControllerServer struct {
	csi.UnimplementedControllerServer

	Driver *SeaweedFsDriver
}

var _ = csi.ControllerServer(&ControllerServer{})

func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	glog.Infof("create volume req: %v", req.GetName())

	// Check arguments
	suggestedVolumeId := req.GetName()
	if suggestedVolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	volumeId := sanitizeVolumeId(suggestedVolumeId)

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	params := req.GetParameters()
	if params == nil {
		params = make(map[string]string)
	}
	glog.V(4).Infof("params:%v", params)
	capacity := req.GetCapacityRange().GetRequiredBytes()

	if err := filer_pb.Mkdir(cs.Driver, "/buckets", volumeId, nil); err != nil {
		return nil, fmt.Errorf("error setting bucket metadata: %v", err)
	}

	glog.V(4).Infof("volume created %s", volumeId)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeId,
			CapacityBytes: capacity,
			VolumeContext: params,
		},
	}, nil
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	glog.Infof("delete volume req: %v", req.VolumeId)

	volumeId := req.VolumeId

	// Check arguments
	if len(volumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}
	glog.V(4).Infof("deleting volume %s", volumeId)

	if err := filer_pb.Remove(cs.Driver, "/buckets", volumeId, true, true, true, false, nil); err != nil {
		return nil, fmt.Errorf("error setting bucket metadata: %v", err)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume we need this just only for csi-attach, but we do nothing here generally
func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volumeId := req.VolumeId
	nodeId := req.NodeId

	glog.Infof("controller publish volume req, volume: %s, node: %s", volumeId, nodeId)

	// Check arguments
	if len(volumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if len(nodeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Node ID missing in request")
	}

	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume we need this just only for csi-attach, but we do nothing here generally
func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeId := req.VolumeId

	glog.Infof("controller unpublish volume req: %s", req.VolumeId)

	// Check arguments
	if len(volumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	glog.Infof("validate volume capabilities req: %v", req.GetVolumeId())

	// Check arguments
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	exists, err := filer_pb.Exists(cs.Driver, "/buckets", req.GetVolumeId(), true)
	if err != nil {
		return nil, fmt.Errorf("error checking bucket %s exists: %v", req.GetVolumeId(), err)
	}
	if !exists {
		// return an error if the volume requested does not exist
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume with id %s does not exist", req.GetVolumeId()))
	}

	// We currently only support RWO
	supportedAccessMode := &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "Only single node writer is supported"}, nil
		}
	}

	volCaps := req.GetVolumeCapabilities()
	if len(volCaps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities not provided")
	}
	var confirmed *csi.ValidateVolumeCapabilitiesResponse_Confirmed
	if isValidVolumeCapabilities(cs.Driver.vcap, volCaps) {
		confirmed = &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: volCaps}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: confirmed,
	}, nil

}

// ControllerGetCapabilities implements the default GRPC callout.
// Default supports all capabilities
func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	glog.V(3).Infof("get capabilities req")

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.Driver.cscap,
	}, nil
}

func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	capacity := req.GetCapacityRange().GetRequiredBytes()

	glog.Infof("expand volume req: %v, capacity: %v", req.GetVolumeId(), capacity)

	// We need to propagate resize requests to node servers
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         capacity,
		NodeExpansionRequired: true,
	}, nil
}

func sanitizeVolumeId(volumeId string) string {
	volumeId = strings.ToLower(volumeId)
	// NOTE: leave original length-only logic to ensure backward compatibility with volumes
	// that happened to work because their suggested volumeId was too long
	if len(volumeId) > 63 {
		h := sha1.New()
		io.WriteString(h, volumeId)
		volumeId = hex.EncodeToString(h.Sum(nil))
	}

	// check for a valid s3 bucket name according to the rules the filer uses
	if s3bucket.VerifyS3BucketName(volumeId) != nil {
		// The suggested volumeId can't be used directly. Use it to generate a new one
		// that is compatible with our filer's name restrictions.
		// generate a 40 hexidecimal character SHA1 hash to avoid name collisions
		h := sha1.New()
		io.WriteString(h, volumeId)
		// hexidecimal encoding of sha1 is 40 characters long
		hexhash := hex.EncodeToString(h.Sum(nil))
		// Use only lowercase letters
		volumeId = strings.ToLower(volumeId)
		sanitized := unsafeVolumeIdChars.ReplaceAllString(volumeId, "-")
		// 21 here is 62 - 40 characters for the hash - 1 more for the "-" we use join
		// the sanitized ID to the hash
		if len(sanitized) > 21 {
			sanitized = sanitized[0:21]
		}
		volumeId = fmt.Sprintf("%s.%s", sanitized, hexhash)
	}
	return volumeId
}

func isValidVolumeCapabilities(driverVolumeCaps []*csi.VolumeCapability_AccessMode, volCaps []*csi.VolumeCapability) bool {
	hasSupport := func(cap *csi.VolumeCapability) bool {
		for _, c := range driverVolumeCaps {
			if c.GetMode() == cap.AccessMode.GetMode() {
				return true
			}
		}
		return false
	}

	foundAll := true
	for _, c := range volCaps {
		if !hasSupport(c) {
			foundAll = false
		}
	}
	return foundAll
}
