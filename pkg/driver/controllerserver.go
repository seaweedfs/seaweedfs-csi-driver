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
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3bucket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var unsafeVolumeIdChars = regexp.MustCompile(`[^-.a-zA-Z0-9]`)

const (
	defaultBucketDir       = "/buckets"
	bucketChildrenPageSize = uint32(1000)
)

type listChildrenFn func(ctx context.Context, filerPath, after string, limit uint32) (page []string, last string, hasMore bool, err error)

type deleteEntryFn func(ctx context.Context, parentPath, name string, ignoreRecursiveError bool) error

type bucketDirFn func(ctx context.Context) (string, error)

type ControllerServer struct {
	csi.UnimplementedControllerServer

	Driver *SeaweedFsDriver

	listChildrenFn listChildrenFn
	deleteEntryFn  deleteEntryFn
	bucketDirFn    bucketDirFn

	// vacStore persists VolumeAttributesClass parameters accepted by
	// ControllerModifyVolume. Nil means the default filer-backed store.
	vacStore vacStore
}

func (cs *ControllerServer) store() vacStore {
	if cs.vacStore != nil {
		return cs.vacStore
	}
	return newFilerVacStore(cs.Driver.filers)
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

	// Merge VolumeAttributesClass mutable parameters into the volume context
	// so initial class settings reach the mount. Mutable values take
	// precedence over static StorageClass parameters.
	if mutableParams := req.GetMutableParameters(); len(mutableParams) > 0 {
		if err := validateMutableParameters(mutableParams); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		for key, value := range mutableParams {
			params[key] = value
		}
	}

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

	bucketPath := path.Join(parentDir, volumeName)
	if err := cs.emptyBucketChildren(ctx, bucketPath); err != nil {
		return nil, fmt.Errorf("error emptying volume %s: %v", volumeId, err)
	}

	if err := cs.deleteEntry(ctx, parentDir, volumeName, true); err != nil {
		return nil, fmt.Errorf("error deleting volume %s: %v", volumeId, err)
	}

	if err := cs.store().Delete(ctx, volumeId); err != nil {
		glog.Warningf("could not delete persisted volume attributes for %s: %v", volumeId, err)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// emptyBucketChildren deletes the direct children of a bucket volume so their
// chunks are reclaimed by fileId before the bucket itself is dropped. The filer
// treats a bucket-rooted delete as a collection drop (which is a no-op when the
// collection was renamed via the StorageClass "collection" parameter), so
// children must be removed individually first. Children are listed and deleted
// page by page to bound memory for large buckets.
func (cs *ControllerServer) emptyBucketChildren(ctx context.Context, bucketPath string) error {
	bucketDir, err := cs.filerBucketDir(ctx)
	if err != nil {
		return err
	}
	if path.Dir(bucketPath) != bucketDir {
		return nil
	}

	after := ""
	for {
		page, last, hasMore, err := cs.listChildren(ctx, bucketPath, after, bucketChildrenPageSize)
		if err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return err
		}
		for _, child := range page {
			if err := cs.deleteEntry(ctx, bucketPath, child, false); err != nil {
				return fmt.Errorf("delete child %s of volume %s: %w", child, bucketPath, err)
			}
		}
		if !hasMore || last == "" {
			return nil
		}
		after = last
	}
}

func (cs *ControllerServer) listChildren(ctx context.Context, filerPath, after string, limit uint32) ([]string, string, bool, error) {
	if cs.listChildrenFn != nil {
		return cs.listChildrenFn(ctx, filerPath, after, limit)
	}
	return listFilerChildrenPage(ctx, cs.Driver, filerPath, after, limit)
}

func (cs *ControllerServer) deleteEntry(ctx context.Context, parentPath, name string, ignoreRecursiveError bool) error {
	if cs.deleteEntryFn != nil {
		return cs.deleteEntryFn(ctx, parentPath, name, ignoreRecursiveError)
	}
	return filer_pb.Remove(ctx, cs.Driver, parentPath, name, true, true, ignoreRecursiveError, false, nil)
}

func (cs *ControllerServer) filerBucketDir(ctx context.Context) (string, error) {
	if cs.bucketDirFn != nil {
		return cs.bucketDirFn(ctx)
	}
	var dir string
	err := cs.Driver.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		resp, err := client.GetFilerConfiguration(ctx, &filer_pb.GetFilerConfigurationRequest{})
		if err != nil {
			return err
		}
		dir = resp.GetDirBuckets()
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("get filer bucket directory: %w", err)
	}
	if dir == "" {
		return defaultBucketDir, nil
	}
	return dir, nil
}

func listFilerChildrenPage(ctx context.Context, filerClient filer_pb.FilerClient, dirPath, after string, limit uint32) ([]string, string, bool, error) {
	var page []string
	var last string
	var sawLast bool
	err := filerClient.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		return filer_pb.SeaweedList(ctx, client, dirPath, "", func(entry *filer_pb.Entry, isLast bool) error {
			name := entry.GetName()
			if name != "" {
				page = append(page, name)
				last = name
			}
			if isLast {
				sawLast = true
			}
			return nil
		}, after, false, limit)
	})
	if err != nil {
		return nil, "", false, err
	}
	hasMore := len(page) > 0 && !sawLast
	return page, last, hasMore, nil
}

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

// mutableMountParameters are volume context keys a VolumeAttributesClass may
// change. Structural keys (path, collection, volumeName, capacity) are
// excluded since they cannot change after provisioning; collectionQuotaMB is
// derived from capacity by the mounter.
var mutableMountParameters = map[string]struct{}{
	"diskType":           {},
	"replication":        {},
	"ttl":                {},
	"dataCenter":         {},
	"dataLocality":       {},
	"uidMap":             {},
	"gidMap":             {},
	"chunkSizeLimitMB":   {},
	"volumeServerAccess": {},
	"readRetryTime":      {},
	"concurrentReaders":  {},
	"concurrentWriters":  {},
	"cacheCapacityMB":    {},
	"cacheMetaTtlSec":    {},
}

func validateMutableParameterValues(key, value string) error {
	if value == "" {
		return nil
	}
	switch key {
	case "dataLocality":
		if _, ok := datalocality.FromString(value); !ok {
			return fmt.Errorf("invalid dataLocality %q", value)
		}
	case "concurrentReaders", "concurrentWriters", "cacheCapacityMB", "cacheMetaTtlSec", "chunkSizeLimitMB":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("%s must be an integer, got %q", key, value)
		}
	}
	return nil
}

// validateMutableParameters rejects structural or unknown keys and values the
// mount would refuse at publish time, so a bad class fails at modify/create
// instead of breaking the next mount.
func validateMutableParameters(params map[string]string) error {
	var unknown []string
	var invalid []string
	for key, value := range params {
		if _, ok := mutableMountParameters[key]; !ok {
			unknown = append(unknown, key)
			continue
		}
		if err := validateMutableParameterValues(key, value); err != nil {
			invalid = append(invalid, err.Error())
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("parameters are not modifiable on SeaweedFS volumes (structural or unknown): %s", strings.Join(unknown, ", "))
	}
	if len(invalid) > 0 {
		return fmt.Errorf("invalid parameter values: %s", strings.Join(invalid, "; "))
	}
	return nil
}

// ControllerModifyVolume validates VolumeAttributesClass parameter changes.
// The SeaweedFS backend stores no per-volume metadata, so accepted parameters
// take effect when kubelet re-publishes and rebuilds the mount from the volume
// context.
func (cs *ControllerServer) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id missing in request")
	}
	if len(req.GetMutableParameters()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "mutable parameters missing in request")
	}
	if err := validateMutableParameters(req.GetMutableParameters()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Kubernetes routes VolumeAttributesClass parameters only through this
	// RPC — the node publish context keeps coming from the immutable PV —
	// so persist the accepted values where NodeStageVolume can read them
	// back on every (re)stage. Storage errors fail the modify so the
	// resizer retries instead of recording a class that never applies.
	if err := cs.store().Write(ctx, volumeID, req.GetMutableParameters()); err != nil {
		return nil, status.Errorf(codes.Internal,
			"persisting modified parameters for %s: %v", volumeID, err)
	}

	glog.Infof("modify volume req: %v, parameters: %v", volumeID, req.GetMutableParameters())
	return &csi.ControllerModifyVolumeResponse{}, nil
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
