package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestCreateVolumeWithFilerOverride(t *testing.T) {
	driver := NewSeaweedFsDriver("seaweedfs-csi", "global-filer:8888", "node-1", "unix://csi.sock", "", false)
	cs := NewControllerServer(driver)

	req := &csi.CreateVolumeRequest{
		Name: "test-volume",
		Parameters: map[string]string{
			"filer": "custom-filer:9999",
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
	}

	_, err := cs.CreateVolume(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error from connecting to nonexistent filer, got nil")
	}

	// The error should mention the overridden filer address
	if !strings.Contains(err.Error(), "custom-filer") {
		t.Fatalf("expected error to contain overridden filer address 'custom-filer', got: %v", err)
	}
}

func TestDeleteVolumeWithFilerOverride(t *testing.T) {
	driver := NewSeaweedFsDriver("seaweedfs-csi", "global-filer:8888", "node-1", "unix://csi.sock", "", false)
	cs := NewControllerServer(driver)

	req := &csi.DeleteVolumeRequest{
		VolumeId: "filer://custom-filer:9999/buckets/test-volume",
	}

	_, err := cs.DeleteVolume(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error from connecting to nonexistent filer, got nil")
	}

	// The error should mention the overridden filer address from VolumeId
	if !strings.Contains(err.Error(), "custom-filer") {
		t.Fatalf("expected error to contain overridden filer address 'custom-filer', got: %v", err)
	}
}
