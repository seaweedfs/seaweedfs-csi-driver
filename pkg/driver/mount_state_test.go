//go:build linux

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"
)

// A restarted node plugin can re-stage a static volume with its original
// settings and re-bind only its surviving publishes with their original modes.
func TestMountStateRecoversExistingPublishesAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	volumeID := "static-models"
	readWrite := filepath.Join(root, "read-write")
	readOnly := filepath.Join(root, "read-only")
	detached := filepath.Join(root, "detached")
	replaced := filepath.Join(root, "replaced")
	capability := &csi.VolumeCapability{}

	original := newTestNodeServer(t, &fakeMounter{})
	original.Driver.MountStateDir = filepath.Join(root, "state")
	original.vacLoader = func(context.Context, string) (map[string]string, error) {
		return map[string]string{"concurrentReaders": "64"}, nil
	}
	if _, err := original.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath + "/",
		VolumeCapability:  capability,
		VolumeContext:     map[string]string{"path": "/models/team-a", "collection": "models", "concurrentReaders": "128"},
	}); err != nil {
		t.Fatalf("stage static volume: %v", err)
	}
	for _, target := range []string{readWrite, readOnly, detached, replaced} {
		if _, err := original.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagingPath + "/",
			TargetPath:        target + "/.",
			VolumeCapability:  capability,
			Readonly:          target == readOnly,
		}); err != nil {
			t.Fatalf("publish %s: %v", target, err)
		}
	}
	if _, err := original.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "/buckets/detached-volume",
		StagingTargetPath: filepath.Join(root, "detached-stage"),
		VolumeCapability:  capability,
	}); err != nil {
		t.Fatalf("stage second volume: %v", err)
	}

	mounter := &fakeMounter{}
	restarted := newTestNodeServer(t, mounter)
	restarted.Driver.MountStateDir = original.Driver.MountStateDir
	restarted.Driver.mountEndpoint = "unix:///tmp/unused-mount-service.sock"
	if err := restarted.restoreMountState(func() (map[string]string, error) {
		return map[string]string{
			stagingPath: "0:12",
			readWrite:   "0:12",
			readOnly:    "0:12",
			replaced:    "0:24",
		}, nil
	}); err != nil {
		t.Fatalf("restore after restart: %v", err)
	}
	if _, ok := restarted.volumes.Load("/buckets/detached-volume"); ok {
		t.Fatal("restored a volume without a surviving mount")
	}
	tracked, ok := restarted.volumes.Load(volumeID)
	if !ok {
		t.Fatal("lost the staged volume across restart")
	}

	// Drive the existing monitor's recovery with fake mount operations.
	tracked.(*Volume).unmounter = &fakeUnmounter{parent: mounter}
	restarted.isHealthyFn = func(string) bool { return false }
	var recoveredContext map[string]string
	restarted.mounterFactory = func(_ string, _ bool, _ *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
		recoveredContext = volContext
		return mounter, nil
	}
	recoveredPublishes := map[string]bool{}
	restarted.bindMountFn = func(source, target string, isReadOnly bool) error {
		if source != stagingPath {
			t.Errorf("bound from %q, want %q", source, stagingPath)
		}
		recoveredPublishes[target] = isReadOnly
		return nil
	}
	restarted.recoverVolume(volumeID)

	wantContext := map[string]string{"path": "/models/team-a", "collection": "models", "concurrentReaders": "64"}
	if !maps.Equal(recoveredContext, wantContext) {
		t.Errorf("mount context = %v, want %v", recoveredContext, wantContext)
	}
	wantPublishes := map[string]bool{readWrite: false, readOnly: true}
	if !maps.Equal(recoveredPublishes, wantPublishes) {
		t.Errorf("recovered publishes = %v, want %v", recoveredPublishes, wantPublishes)
	}
}

// An invalid journal record cannot make the driver adopt a live mount or
// prevent a valid record from being restored.
func TestMountStateRestoresValidRecordsAlongsideInvalidRecords(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.Driver.MountStateDir = t.TempDir()
	ns.Driver.mountEndpoint = "unix:///tmp/unused-mount-service.sock"
	stagingPath := filepath.Join(t.TempDir(), "staging")

	records := map[string]string{
		mountStateName("valid"): fmt.Sprintf(
			`{"volumeId":"valid","stagingPath":%q,"volumeContext":{}}`, stagingPath),
		mountStateName("wrong-name"): fmt.Sprintf(
			`{"volumeId":"mismatched","stagingPath":%q,"volumeContext":{}}`, stagingPath),
		mountStateName("null-context"): fmt.Sprintf(
			`{"volumeId":"null-context","stagingPath":%q,"volumeContext":null}`, stagingPath),
		mountStateName("corrupt"): `{"volumeId":"corrupt",`,
	}
	for name, contents := range records {
		if err := os.WriteFile(filepath.Join(ns.Driver.MountStateDir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ns.restoreMountState(func() (map[string]string, error) {
		return map[string]string{stagingPath: "0:12"}, nil
	}); err != nil {
		t.Fatalf("restore journal: %v", err)
	}
	if _, ok := ns.volumes.Load("valid"); !ok {
		t.Error("valid record was not restored")
	}
	ns.volumes.Range(func(key, _ any) bool {
		if key != "valid" {
			t.Errorf("unexpected restored volume %q", key)
		}
		return true
	})
}

// A successful retry after a journal write failure must record the mount
// that the first request already staged. Subsequent teardown clears it.
func TestMountStateStageRetryAndTeardown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	stateDir := filepath.Join(root, "state")
	volumeID := "read-only-models"
	mounter := &fakeMounter{}
	ns := newTestNodeServer(t, mounter)
	ns.Driver.MountStateDir = stateDir
	ns.Driver.CacheDir = filepath.Join(root, "cache")
	ns.Driver.volumeSocketDir = root
	if err := os.WriteFile(stateDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	capability := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
	}
	request := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  capability,
	}
	if _, err := ns.NodeStageVolume(ctx, request); status.Code(err) != codes.Internal {
		t.Fatalf("stage with unavailable state directory = %v, want Internal", err)
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := ns.NodeStageVolume(ctx, request); err != nil {
		t.Fatalf("retry stage: %v", err)
	}
	if mounter.mountCalls != 1 {
		t.Errorf("stage retry mounted %d times, want 1", mounter.mountCalls)
	}

	target := filepath.Join(root, "publish")
	restart := func() *NodeServer {
		t.Helper()
		restarted := newTestNodeServer(t, &fakeMounter{})
		restarted.Driver.MountStateDir = stateDir
		restarted.Driver.mountEndpoint = "unix:///tmp/unused-mount-service.sock"
		if err := restarted.restoreMountState(func() (map[string]string, error) {
			return map[string]string{stagingPath: "0:12", target: "0:12"}, nil
		}); err != nil {
			t.Fatalf("restore after restart: %v", err)
		}
		return restarted
	}
	tracked, ok := restart().volumes.Load(volumeID)
	if !ok {
		t.Fatal("stage retry did not record the existing mount")
	}
	if !tracked.(*Volume).readOnly {
		t.Error("volume was staged read-only before restart")
	}

	if _, err := ns.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        target,
		VolumeCapability:  capability,
		Readonly:          true,
	}); err != nil {
		t.Fatalf("publish volume: %v", err)
	}
	if _, err := ns.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: target + "/.",
	}); err != nil {
		t.Fatalf("unpublish volume: %v", err)
	}
	tracked, ok = restart().volumes.Load(volumeID)
	if !ok {
		t.Fatal("unpublish removed still-staged mount")
	}
	if tracked.(*Volume).HasPublishPath(target) {
		t.Error("unpublished target was still in the journal")
	}

	if _, err := ns.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("unstage volume: %v", err)
	}
	if _, ok := restart().volumes.Load(volumeID); ok {
		t.Fatal("unstaged volume was still in the journal")
	}
}

// Teardown removes the mounts even if the driver cannot update or delete
// its journal. A restart ignores the detached publish in the old record.
func TestMountStateTeardownWithUnavailableJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	target := filepath.Join(root, "target")
	const volumeID = "models"
	capability := &csi.VolumeCapability{}
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.Driver.MountStateDir = filepath.Join(root, "state")
	ns.Driver.volumeSocketDir = root
	originalMountutil := mountutil
	localMounter := mount.NewFakeMounter(nil)
	mountutil = localMounter
	t.Cleanup(func() { mountutil = originalMountutil })
	ns.bindMountFn = defaultBindMount

	if _, err := ns.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stage, VolumeCapability: capability,
	}); err != nil {
		t.Fatalf("stage volume: %v", err)
	}
	// The fake mount service acknowledges Stage without mounting FUSE.
	if err := localMounter.Mount("0:12", stage, "fuse.seaweedfs", nil); err != nil {
		t.Fatalf("record the mount service's staging mount: %v", err)
	}
	if _, err := ns.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stage, TargetPath: target, VolumeCapability: capability,
	}); err != nil {
		t.Fatalf("publish volume: %v", err)
	}
	if mounted, err := localMounter.IsMountPoint(target); err != nil || !mounted {
		t.Fatalf("publish target was not mounted: %v", err)
	}
	oldStateDir := filepath.Join(root, "old-state")
	if err := os.Rename(ns.Driver.MountStateDir, oldStateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ns.Driver.MountStateDir, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := ns.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: target,
	}); err != nil {
		t.Fatalf("unpublish with unavailable journal: %v", err)
	}
	remaining, err := localMounter.List()
	if err != nil || len(remaining) != 1 || remaining[0].Path != stage {
		t.Fatalf("after unpublish only staging should remain: %v, list error: %v", remaining, err)
	}

	restarted := newTestNodeServer(t, &fakeMounter{})
	restarted.Driver.MountStateDir = oldStateDir
	restarted.Driver.mountEndpoint = "unix:///tmp/unused-mount-service.sock"
	if err := restarted.restoreMountState(func() (map[string]string, error) {
		return map[string]string{remaining[0].Path: remaining[0].Device}, nil
	}); err != nil {
		t.Fatalf("restore journal: %v", err)
	}
	tracked, ok := restarted.volumes.Load(volumeID)
	if !ok || tracked.(*Volume).HasPublishPath(target) {
		t.Fatal("restore did not keep the stage and discard the detached publish")
	}

	if _, err := ns.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stage,
	}); err != nil {
		t.Fatalf("unstage with unavailable journal: %v", err)
	}
	if remaining, err := localMounter.List(); err != nil || len(remaining) != 0 {
		t.Fatalf("mounts remained after teardown: %v, list error: %v", remaining, err)
	}
}

// A plugin that adopts a surviving mount through either CSI entry point
// must release that volume through the mount service on unstage.
func TestAdoptedMountUnstage(t *testing.T) {
	ctx := context.Background()
	socket := filepath.Join(t.TempDir(), "mount.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unmounted := make(chan string, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mountmanager.UnmountRequest
		if r.Method != http.MethodPost || r.URL.Path != "/unmount" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "expected an unmount request", http.StatusBadRequest)
			return
		}
		unmounted <- req.VolumeID
		fmt.Fprint(w, "{}")
	})}
	t.Cleanup(func() { server.Close() })
	go server.Serve(listener)

	for _, entryPoint := range []string{"stage", "publish"} {
		t.Run(entryPoint, func(t *testing.T) {
			volumeID := "models-" + entryPoint
			stage := t.TempDir()
			originalMountutil := mountutil
			mountutil = mount.NewFakeMounter([]mount.MountPoint{{Path: stage}})
			t.Cleanup(func() { mountutil = originalMountutil })

			mounter := &fakeMounter{}
			ns := newTestNodeServer(t, mounter)
			ns.bindMountFn = defaultBindMount
			ns.Driver.mountEndpoint = "unix://" + socket
			ns.Driver.volumeSocketDir = t.TempDir()
			capability := &csi.VolumeCapability{}
			var err error
			if entryPoint == "stage" {
				_, err = ns.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
					VolumeId: volumeID, StagingTargetPath: stage, VolumeCapability: capability,
				})
			} else {
				target := filepath.Join(t.TempDir(), "target")
				_, err = ns.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
					VolumeId: volumeID, StagingTargetPath: stage, VolumeCapability: capability,
					TargetPath: target,
				})
				if err == nil {
					_, err = ns.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
						VolumeId: volumeID, TargetPath: target,
					})
				}
			}
			if err != nil || mounter.mountCalls != 0 {
				t.Fatalf("adopt surviving mount: %v, new mounts: %d", err, mounter.mountCalls)
			}
			if _, err := ns.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
				VolumeId: volumeID, StagingTargetPath: stage,
			}); err != nil {
				t.Fatalf("unstage adopted mount: %v", err)
			}
			select {
			case got := <-unmounted:
				if got != volumeID {
					t.Fatalf("unmounted volume %q, want %q", got, volumeID)
				}
			default:
				t.Fatal("unstage did not contact the mount service")
			}
		})
	}
}

// A replacement mount service can acknowledge Unmount without detaching a
// staging mount left by its predecessor. NodeUnstage must detach it only
// when the request names the correct staging path.
func TestMountStateUnstageDetachesMountAfterManagerRestart(t *testing.T) {
	ctx := context.Background()
	volumeID := "static-models"
	stage := t.TempDir()
	stateDir := t.TempDir()

	originalManager := &fakeMounter{}
	ns := newTestNodeServer(t, originalManager)
	ns.Driver.MountStateDir = stateDir
	ns.Driver.mountEndpoint = "unix:///tmp/unused-mount-service.sock"
	ns.Driver.volumeSocketDir = stateDir
	if _, err := ns.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stage, VolumeCapability: &csi.VolumeCapability{},
	}); err != nil {
		t.Fatalf("stage volume: %v", err)
	}
	if err := ns.restoreMountState(func() (map[string]string, error) {
		return map[string]string{stage: "0:12"}, nil
	}); err != nil {
		t.Fatalf("restore tracking: %v", err)
	}
	tracked, ok := ns.volumes.Load(volumeID)
	if !ok {
		t.Fatal("lost the staged volume across restart")
	}
	// Match the response from a mount service with no entry for the volume.
	replacementManager := &fakeMounter{}
	tracked.(*Volume).unmounter = &fakeUnmounter{parent: replacementManager}

	originalMountutil := mountutil
	localMounter := mount.NewFakeMounter([]mount.MountPoint{{Path: stage}})
	mountutil = localMounter
	t.Cleanup(func() { mountutil = originalMountutil })

	if _, err := ns.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: filepath.Join(t.TempDir(), "wrong-stage"),
	}); status.Code(err) != codes.Internal {
		t.Fatalf("unstage with wrong path = %v, want Internal", err)
	}
	if replacementManager.unmountCalls != 0 {
		t.Fatal("unstage with wrong path called the mount service")
	}
	if remaining, err := localMounter.List(); err != nil || len(remaining) != 1 {
		t.Fatalf("wrong-path unstage affected the staging mount: %v, list error: %v", remaining, err)
	}

	if _, err := ns.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stage + "/.",
	}); err != nil {
		t.Fatalf("unstage volume: %v", err)
	}
	if replacementManager.unmountCalls != 1 {
		t.Fatalf("mount service received %d unmounts, want 1", replacementManager.unmountCalls)
	}
	if remaining, err := localMounter.List(); err != nil || len(remaining) != 0 {
		t.Fatalf("staging mount remained attached: %v, list error: %v", remaining, err)
	}
}
