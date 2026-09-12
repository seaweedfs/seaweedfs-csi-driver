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

type deleteRecord struct {
	parent string
	name   string
}

func testDriverWithCaps() *SeaweedFsDriver {
	driver := &SeaweedFsDriver{name: "test"}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	})
	return driver
}

type fakeDeleter struct {
	deletes []deleteRecord
	failOn  string
}

func (f *fakeDeleter) fn() deleteEntryFn {
	return func(_ context.Context, parent, name string, _ bool) error {
		if f.failOn == parent+"/"+name {
			return fmt.Errorf("delete %s/%s: boom", parent, name)
		}
		f.deletes = append(f.deletes, deleteRecord{parent: parent, name: name})
		return nil
	}
}

func staticChildrenFn(children map[string][]string) listChildrenFn {
	return func(_ context.Context, dir, _ string, _ uint32) ([]string, string, bool, error) {
		return children[dir], "", false, nil
	}
}

func bucketDirFnFor(dir string) bucketDirFn {
	return func(_ context.Context) (string, error) { return dir, nil }
}

func newTestControllerServer(t *testing.T, children map[string][]string, deleter *fakeDeleter) *ControllerServer {
	t.Helper()
	driver := testDriverWithCaps()
	cs := &ControllerServer{
		Driver:         driver,
		listChildrenFn: staticChildrenFn(children),
		bucketDirFn:    bucketDirFnFor("/buckets"),
	}
	if deleter != nil {
		cs.deleteEntryFn = deleter.fn()
	}
	return cs
}

func TestDeleteVolumeBucketsAreEmptiedChildrenFirst(t *testing.T) {
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

func TestDeleteVolumeNonBucketPathSkipsEmptying(t *testing.T) {
	volumeID := "/data/volumes/myvol"
	listed := false
	driver := testDriverWithCaps()
	cs := &ControllerServer{
		Driver: driver,
		listChildrenFn: func(_ context.Context, _, _ string, _ uint32) ([]string, string, bool, error) {
			listed = true
			return nil, "", false, nil
		},
		bucketDirFn: bucketDirFnFor("/buckets"),
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
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, _, _ string, _ uint32) ([]string, string, bool, error) {
			return nil, "", false, filer_pb.ErrNotFound
		},
		bucketDirFn:   bucketDirFnFor("/buckets"),
		deleteEntryFn: func(_ context.Context, _, _ string, _ bool) error { return nil },
	}

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "/buckets/pvc-gone"}); err != nil {
		t.Fatalf("DeleteVolume on missing volume = %v, want nil", err)
	}
}

func TestDeleteVolumeListFailurePropagates(t *testing.T) {
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, _, _ string, _ uint32) ([]string, string, bool, error) {
			return nil, "", false, errors.New("filer unreachable")
		},
		bucketDirFn: bucketDirFnFor("/buckets"),
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

func TestDeleteVolumePagedChildrenAllDeleted(t *testing.T) {
	volumeID := "/buckets/pvc-paged"
	pages := [][]string{{"a", "b"}, {"c", "d"}}
	callCount := 0
	deleter := &fakeDeleter{}
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, dir, _ string, _ uint32) ([]string, string, bool, error) {
			if dir != volumeID || callCount >= len(pages) {
				return nil, "", false, nil
			}
			page := pages[callCount]
			callCount++
			return page, page[len(page)-1], callCount < len(pages), nil
		},
		bucketDirFn:   bucketDirFnFor("/buckets"),
		deleteEntryFn: deleter.fn(),
	}

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	want := []deleteRecord{
		{parent: volumeID, name: "a"},
		{parent: volumeID, name: "b"},
		{parent: volumeID, name: "c"},
		{parent: volumeID, name: "d"},
		{parent: "/buckets", name: "pvc-paged"},
	}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}

func TestDeleteVolumeCustomBucketDirIsEmptied(t *testing.T) {
	volumeID := "/tenant-buckets/pvc-x"
	deleter := &fakeDeleter{}
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, dir, _ string, _ uint32) ([]string, string, bool, error) {
			if dir == volumeID {
				return []string{"file.bin"}, "", false, nil
			}
			return nil, "", false, nil
		},
		bucketDirFn:   bucketDirFnFor("/tenant-buckets"),
		deleteEntryFn: deleter.fn(),
	}

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	want := []deleteRecord{
		{parent: volumeID, name: "file.bin"},
		{parent: "/tenant-buckets", name: "pvc-x"},
	}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}

func TestDeleteVolumeNonBucketPathWithCustomBucketDirSkipsEmptying(t *testing.T) {
	volumeID := "/data/vol"
	listed := false
	deleter := &fakeDeleter{}
	cs := &ControllerServer{
		Driver: testDriverWithCaps(),
		listChildrenFn: func(_ context.Context, _, _ string, _ uint32) ([]string, string, bool, error) {
			listed = true
			return nil, "", false, nil
		},
		bucketDirFn:   bucketDirFnFor("/tenant-buckets"),
		deleteEntryFn: deleter.fn(),
	}

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	if listed {
		t.Fatal("non-bucket volume must not list children")
	}
	want := []deleteRecord{{parent: "/data", name: "vol"}}
	if !slices.Equal(deleter.deletes, want) {
		t.Fatalf("deletes = %+v, want %+v", deleter.deletes, want)
	}
}
