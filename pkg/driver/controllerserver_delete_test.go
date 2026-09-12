package driver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// deleteRecord captures one (parent, name) delete issued by DeleteVolume.
type deleteRecord struct {
	parent string
	name   string
}

// testDriverWithCaps returns a driver that accepts CREATE_DELETE_VOLUME requests.
func testDriverWithCaps() *SeaweedFsDriver {
	driver := &SeaweedFsDriver{name: "test"}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	})
	return driver
}

// fakeDeleter records deletes and can fail on a chosen entry.
type fakeDeleter struct {
	deletes []deleteRecord
	failOn  string // match "<parent>/<name>" to force an error
}

func (f *fakeDeleter) fn() deleteEntryFn {
	return func(_ context.Context, parent, name string) error {
		if f.failOn == parent+"/"+name {
			return fmt.Errorf("delete %s/%s: boom", parent, name)
		}
		f.deletes = append(f.deletes, deleteRecord{parent: parent, name: name})
		return nil
	}
}

func newTestControllerServer(t *testing.T, children map[string][]string, deleter *fakeDeleter) *ControllerServer {
	t.Helper()
	driver := &SeaweedFsDriver{name: "test"}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	})
	cs := &ControllerServer{
		Driver: driver,
		listChildrenFn: func(_ context.Context, dir string) ([]string, error) {
			return children[dir], nil
		},
	}
	if deleter != nil {
		cs.deleteEntryFn = deleter.fn()
	}
	return cs
}

func TestDeleteVolumeBucketsAreEmendedChildrenFirst(t *testing.T) {
	// The CSI provisioner creates volumes as buckets under /buckets. Deleting
	// the bucket entry alone leaks chunks when a custom collection is set, so
	// DeleteVolume must delete every direct child before the bucket itself.
	volumeID := "/buckets/pvc-abc"
	deleter := &fakeDeleter{}
	cs := newTestControllerServer(t, map[string][]string{
		volumeID: {"subdir", "file1.txt", "file2.txt"},
	}, deleter)

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	want := []deleteRecord{
		{parent: volumeID, name: "subdir"},
		{parent: volumeID, name: "file1.txt"},
		{parent: volumeID, name: "file2.txt"},
		{parent: "/buckets", name: "pvc-abc"},
	}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}

func TestDeleteVolumeLegacyIdIsEmptiedAsBucket(t *testing.T) {
	// Legacy volume IDs are bare names resolved under /buckets; they are
	// buckets too and must be emptied first.
	deleter := &fakeDeleter{}
	cs := newTestControllerServer(t, map[string][]string{
		"/buckets/pvc-legacy": {"data.bin"},
	}, deleter)

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc-legacy"}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	want := []deleteRecord{
		{parent: "/buckets/pvc-legacy", name: "data.bin"},
		{parent: "/buckets", name: "pvc-legacy"},
	}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}

func TestDeleteVolumeNonBucketPathSkipsEmpting(t *testing.T) {
	// Volumes mounted at an explicit path outside /buckets are not buckets;
	// the plain recursive filer delete reclaims their chunks by fileId, so no
	// child listing must happen.
	volumeID := "/data/volumes/myvol"
	listed := false
	driver := &SeaweedFsDriver{name: "test"}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	})
	cs := &ControllerServer{
		Driver: driver,
		listChildrenFn: func(_ context.Context, dir string) ([]string, error) {
			listed = true
			return nil, nil
		},
	}
	deleter := &fakeDeleter{}
	cs.deleteEntryFn = deleter.fn()

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	if listed {
		t.Fatal("non-bucket volume must not list children")
	}
	want := []deleteRecord{{parent: "/data/volumes", name: "myvol"}}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}

func TestDeleteVolumeEmptyBucketDeletesRootOnly(t *testing.T) {
	deleter := &fakeDeleter{}
	cs := newTestControllerServer(t, map[string][]string{}, deleter)

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "/buckets/pvc-empty"}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	want := []deleteRecord{{parent: "/buckets", name: "pvc-empty"}}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}

func TestDeleteVolumeChildFailureStopsBeforeRoot(t *testing.T) {
	// A failed child delete must abort: the CSI retry will come back for the
	// rest. Deleting the bucket root while children remain could leave their
	// chunks unreachable yet un-reclaimed.
	volumeID := "/buckets/pvc-partial"
	deleter := &fakeDeleter{failOn: volumeID + "/file1.txt"}
	cs := newTestControllerServer(t, map[string][]string{
		volumeID: {"file1.txt", "file2.txt"},
	}, deleter)

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err == nil {
		t.Fatal("expected error when a child delete fails")
	}

	for _, d := range deleter.deletes {
		if d.parent == "/buckets" {
			t.Fatalf("bucket root deleted despite failed child delete: %+v", deleter.deletes)
		}
	}
}

func TestDeleteVolumeMissingIdIsSuccess(t *testing.T) {
	// The CSI spec wants DeleteVolume to be idempotent: an unknown volume is
	// a success. The filer reports not-found through the root delete; the
	// child listing of a vanished bucket must be treated as empty.
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, dir string) ([]string, error) {
			return nil, filer_pb.ErrNotFound
		},
		deleteEntryFn: func(_ context.Context, parent, name string) error {
			// filer_pb.Remove swallows not-found; mirror that.
			return nil
		},
	}

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "/buckets/pvc-gone"}); err != nil {
		t.Fatalf("DeleteVolume on missing volume = %v, want nil", err)
	}
}

func TestDeleteVolumeListFailurePropagates(t *testing.T) {
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, dir string) ([]string, error) {
			return nil, errors.New("filer unreachable")
		},
	}

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "/buckets/pvc-x"}); err == nil {
		t.Fatal("expected error when listing children fails")
	} else if !strings.Contains(err.Error(), "filer unreachable") {
		t.Fatalf("error = %v, want it to wrap the listing failure", err)
	}
}

func TestDeleteVolumeValidatesVolumeId(t *testing.T) {
	cs := &ControllerServer{Driver: testDriverWithCaps()}
	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{}); err == nil {
		t.Fatal("expected InvalidArgument for empty volume id")
	}
}
