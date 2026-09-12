package driver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3bucket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var unsafeVolumeIdChars = regexp.MustCompile(`[^-.a-zA-Z0-9]`)

// listChildrenFn lists the names of the direct children of a filer path.
// It is a field on ControllerServer so tests can substitute a fake that does
// not talk to a real filer.
type listChildrenFn func(ctx context.Context, filerPath string) ([]string, error)

// deleteEntryFn deletes a single entry (file or directory) under parentPath,
// reclaiming its data. It is a field so tests can record the deletes that a
// DeleteVolume issues without a real filer.
type deleteEntryFn func(ctx context.Context, parentPath, name string) error

type ControllerServer struct {
	csi.UnimplementedControllerServer

	Driver *SeaweedFsDriver

	// Injectable seams (overridden in tests).
	listChildrenFn listChildrenFn
	deleteEntryFn  deleteEntryFn
}

var _ = csi.ControllerServer(&ControllerServer{})

func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	glog.Infof("create volume req: %v", req.GetName())

	params := req.GetParameters()
	if params == nil {
		params = make(map[string]string)
	}
	glog.V(4).Infof("params:%v", params)

	// Check arguments
	requestedVolumeId := req.GetName()
	if requestedVolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}

	// Resolving path for volume
	volumePath := params["path"]
	var parentDir, volumeName string
	if volumePath == "" {
		// If path is implicit, use provided parentDir, or default to creating buckets

		// FIXME: need to use bucketDir in Filer config since it can be set to alternative paths
		parentDir = params["parentDir"]
		if parentDir == "" {
			parentDir = "/buckets"
		}

		// Detect if this volume is a bucket by checking parentDir
		if parentDir == "/buckets" {
			volumeName = sanitizeVolumeIdS3(requestedVolumeId)
		} else {
			volumeName = requestedVolumeId
		}
		volumePath = path.Join(parentDir, volumeName)
	} else {
		// if path is explicit, extract parentDir and volumeName out of it
		volumePath = path.Clean(volumePath)
		parentDir = path.Dir(volumePath)
		volumeName = path.Base(volumePath)
	}

	// Store resolved names back to volume context
	params["parentDir"] = parentDir
	params["volumeName"] = volumeName

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	capacity := req.GetCapacityRange().GetRequiredBytes()
	if capacity > 0 {
		params[volumeCapacityKey] = strconv.FormatInt(capacity, 10)
	}

	if err := filer_pb.Mkdir(ctx, cs.Driver, parentDir, volumeName, nil); err != nil {
		return nil, fmt.Errorf("error creating volume: %v", err)
	}

	glog.V(4).Infof("volume created %s at %s", requestedVolumeId, volumePath)

	// Use full paths as VolumeID
	// This keeps everything stateless
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           volumePath,
			CapacityBytes:      capacity,
			VolumeContext:      params,
			AccessibleTopology: accessibleTopology(req.GetAccessibilityRequirements()),
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

	var parentDir, volumeName string
	if path.IsAbs(volumeId) {
		parentDir = path.Dir(volumeId)
		volumeName = path.Base(volumeId)
	} else {
		// Backward-compatibility with legacy volume ID
		parentDir = "/buckets"
		volumeName = volumeId
	}

	// A volume provisioned under /buckets is an S3 bucket directory. When the
	// filer deletes a bucket it does not delete chunks by fileId; it asks the
	// master to drop the collection that shares the bucket's name. That holds
	// only when the collection was never renamed. If the StorageClass set a
	// "collection" parameter (e.g. "nextcloud"), the data lives in that
	// collection, not in one named after the bucket, and the collection delete
	// is a silent no-op — the space is never reclaimed.
	//
	// To reclaim the space regardless of the collection name, empty the bucket
	// first: deleting a child of the bucket is not a bucket-scoped delete, so
	// the filer enqueues the child's chunks by fileId in whichever collection
	// they live in. The recursion into each child stays bulk on the filer
	// side, so only the bucket's direct children cost one delete per entry.
	//
	// This is idempotent: an interrupted delete leaves some children gone and
	// the rest to sweep, and the final bucket delete keeps its not-found
	// tolerance.
	bucketPath := path.Join(parentDir, volumeName)
	if err := cs.emptyBucketChildren(ctx, bucketPath); err != nil {
		return nil, fmt.Errorf("error emptying volume %s: %v", volumeId, err)
	}

	if err := cs.deleteEntry(ctx, parentDir, volumeName); err != nil {
		return nil, fmt.Errorf("error deleting volume %s: %v", volumeId, err)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// emptyBucketChildren deletes the direct children of a bucket volume so their
// chunks are reclaimed by fileId before the bucket itself is dropped. Only
// buckets (a directory directly under the filer's bucket root) need this; for
// any other path the caller's single recursive delete already reclaims the
// data the standard way.
func (cs *ControllerServer) emptyBucketChildren(ctx context.Context, bucketPath string) error {
	if path.Dir(bucketPath) != "/buckets" {
		return nil
	}

	children, err := cs.listChildren(ctx, bucketPath)
	if err != nil {
		// A bucket that is already gone has nothing to empty; the caller's
		// root delete stays idempotent (the CSI spec wants re-deletes to
		// succeed). Normalise not-found to "no children" here.
		if isNotFoundError(err) {
			return nil
		}
		return err
	}
	glog.V(4).Infof("emptying bucket %s: %d children", bucketPath, len(children))

	for _, child := range children {
		if err := cs.deleteEntry(ctx, bucketPath, child); err != nil {
			return fmt.Errorf("delete child %s of volume %s: %w", child, bucketPath, err)
		}
	}
	return nil
}

func (cs *ControllerServer) listChildren(ctx context.Context, filerPath string) ([]string, error) {
	if cs.listChildrenFn != nil {
		return cs.listChildrenFn(ctx, filerPath)
	}
	return listFilerChildren(ctx, cs.Driver, filerPath)
}

func (cs *ControllerServer) deleteEntry(ctx context.Context, parentPath, name string) error {
	if cs.deleteEntryFn != nil {
		return cs.deleteEntryFn(ctx, parentPath, name)
	}
	return filer_pb.Remove(ctx, cs.Driver, parentPath, name, true, true, true, false, nil)
}

// listFilerChildren returns the names of the direct children of a filer
// directory, without descending into them. The listing is paged: the filer
// caps one response at its -dirListLimit, and a truncated sweep would leak
// the children beyond the cap.
func listFilerChildren(ctx context.Context, filerClient filer_pb.FilerClient, dirPath string) ([]string, error) {
	const pageSize = uint32(1000)

	var names []string
	after := ""
	for {
		var page []string
		var lastRaw string
		err := filerClient.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
			return filer_pb.SeaweedList(ctx, client, dirPath, "", func(entry *filer_pb.Entry, isLast bool) error {
				lastRaw = entry.GetName()
				if lastRaw != "" {
					page = append(page, lastRaw)
				}
				return nil
			}, after, false, pageSize)
		})
		if err != nil {
			// A missing directory has nothing to empty; keep what was already
			// collected so an already-deleted volume does not fail the delete.
			if isNotFoundError(err) {
				return names, nil
			}
			return nil, err
		}
		names = append(names, page...)
		if len(page) < int(pageSize) || lastRaw == "" {
			return names, nil
		}
		after = lastRaw
	}
}

// isNotFoundError reports whether err, or its wire text, is the filer's
// not-found sentinel. The sentinel can lose its identity crossing gRPC, so the
// message marker is checked too, mirroring filer_pb's own tolerance.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, filer_pb.ErrNotFound) || strings.Contains(err.Error(), filer_pb.ErrNotFound.Error())
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
	volumeId := req.VolumeId

	glog.Infof("validate volume capabilities req: %v", volumeId)

	// Check arguments
	if volumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	var parentDir, volumeName string
	if path.IsAbs(volumeId) {
		parentDir = path.Dir(volumeId)
		volumeName = path.Base(volumeId)
	} else {
		// Backward-compatibility with legacy volume ID
		parentDir = "/buckets"
		volumeName = volumeId
	}

	exists, err := filer_pb.Exists(ctx, cs.Driver, parentDir, volumeName, true)
	if err != nil {
		return nil, fmt.Errorf("error checking bucket %s exists: %v", volumeId, err)
	}
	if !exists {
		// return an error if the volume requested does not exist
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume with id %s does not exist", volumeId))
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

// accessibleTopology reports where a created volume can be used. The filer is
// reachable from every node running the plugin, so the volume is accessible
// from all requested segments instead of a single preferred one.
func accessibleTopology(requirements *csi.TopologyRequirement) []*csi.Topology {
	if topologies := requirements.GetRequisite(); len(topologies) > 0 {
		return topologies
	}
	return requirements.GetPreferred()
}

func sanitizeVolumeIdS3(volumeId string) string {
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
