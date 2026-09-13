package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func modifyTestDriver() *ControllerServer {
	driver := &SeaweedFsDriver{name: "test"}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	})
	return &ControllerServer{Driver: driver}
}

func hasModifyVolumeCapability(cs *ControllerServer) bool {
	resp, err := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		return false
	}
	for _, cap := range resp.GetCapabilities() {
		if cap.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_MODIFY_VOLUME {
			return true
		}
	}
	return false
}

func TestControllerModifyVolume_CapabilityAdvertised(t *testing.T) {
	cs := modifyTestDriver()
	if !hasModifyVolumeCapability(cs) {
		t.Fatal("MODIFY_VOLUME capability not advertised")
	}
}

func TestControllerModifyVolume_AcceptsMutableParameters(t *testing.T) {
	cs := modifyTestDriver()
	resp, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "pvc-abc",
		MutableParameters: map[string]string{
			"diskType":     "ssd",
			"replication":  "000",
			"dataLocality": "none",
		},
	})
	if err != nil {
		t.Fatalf("modify with mutable parameters failed: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
}

func TestControllerModifyVolume_RejectsUnknownParameter(t *testing.T) {
	cs := modifyTestDriver()
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"collection": "other"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if !strings.Contains(err.Error(), "collection") {
		t.Errorf("error should name the rejected parameter: %v", err)
	}
}

func TestControllerModifyVolume_RejectsIgnoredQuotaParameter(t *testing.T) {
	// collectionQuotaMB is listed among the mounter's ignored context keys —
	// accepting it as modifiable would promise a change that never applies.
	cs := modifyTestDriver()
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"collectionQuotaMB": "1024"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestControllerModifyVolume_RejectsInvalidValues(t *testing.T) {
	cs := modifyTestDriver()
	for key, value := range map[string]string{
		"dataLocality":      "bogus",
		"concurrentReaders": "not-a-number",
		"cacheCapacityMB":   "-",
		"chunkSizeLimitMB":  "1.5",
	} {
		_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
			VolumeId:          "pvc-abc",
			MutableParameters: map[string]string{key: value},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s=%q: code = %v, want InvalidArgument", key, value, status.Code(err))
		}
	}
}

func TestControllerModifyVolume_AcceptsValidValues(t *testing.T) {
	cs := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "pvc-abc",
		MutableParameters: map[string]string{
			"dataLocality":      "none",
			"concurrentReaders": "64",
			"cacheCapacityMB":   "0",
		},
	}); err != nil {
		t.Fatalf("valid values rejected: %v", err)
	}
}

func TestControllerModifyVolume_RejectsStructuralParameter(t *testing.T) {
	cs := modifyTestDriver()
	for _, key := range []string{"parentDir", "path", "volumeName", "filer.path"} {
		_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
			VolumeId:          "pvc-abc",
			MutableParameters: map[string]string{key: "whatever"},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("structural key %q: code = %v, want InvalidArgument", key, status.Code(err))
		}
	}
}

func TestControllerModifyVolume_RequiresVolumeIdAndParameters(t *testing.T) {
	cs := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{VolumeId: "pvc-abc"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty parameters: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{MutableParameters: map[string]string{"diskType": "ssd"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty volume id: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestControllerModifyVolume_MounterConsumesModifiedParameters(t *testing.T) {
	// The contract that makes MODIFY_VOLUME meaningful: a modified diskType
	// must reach the weed mount command line on the next publish.
	cs := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"diskType": "ssd"},
	}); err != nil {
		t.Fatalf("modify failed: %v", err)
	}

	d := &SeaweedFsDriver{name: "test", CacheCapacityMB: 0, CacheMetaTtlSec: 60}
	m := &mountServiceMounter{driver: d, volContext: map[string]string{
		"diskType":   "ssd",
		"parentDir":  "/buckets",
		"volumeName": "pvc-abc",
	}, volumeID: "/buckets/pvc-abc", readOnly: false}
	args, err := m.buildMountArgs("/tmp/target", "/var/cache/x", "/var/lib/seaweedfs-mount.sock", []string{"127.0.0.1:8888"})
	if err != nil {
		t.Fatalf("buildMountArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-disk=ssd") {
		t.Errorf("mount args missing -disk=ssd: %s", joined)
	}
}
