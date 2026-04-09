//go:build linux
// +build linux

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeMountState tracks the behavior of a fake FUSE mount across a
// simulated crash-and-recover lifecycle. It is used to verify that the
// health monitor:
//  1. Detects a dead FUSE mount,
//  2. Re-stages with a fresh mounter, and
//  3. Re-binds all previously published paths.
type fakeMountState struct {
	mu sync.Mutex

	// healthy controls what isHealthyFn returns. Flip to false to
	// simulate the FUSE daemon dying.
	healthy atomic.Bool

	stageCalls       int
	unstageCalls     int
	cleanupCalls     int
	unmountCalls     int
	bindMountCalls   int
	bindMountTargets []string
}

func newFakeMountState() *fakeMountState {
	s := &fakeMountState{}
	s.healthy.Store(true)
	return s
}

// newMounter returns a Mounter that records Stage calls in this state.
func (s *fakeMountState) newMounter() Mounter {
	return &stateMounter{state: s}
}

type stateMounter struct{ state *fakeMountState }

func (m *stateMounter) Mount(target string) (Unmounter, error) {
	m.state.mu.Lock()
	m.state.stageCalls++
	m.state.mu.Unlock()
	// Create the target so any downstream checkMount sees a directory.
	if err := os.MkdirAll(target, 0755); err != nil {
		return nil, err
	}
	return &stateUnmounter{state: m.state}, nil
}

type stateUnmounter struct{ state *fakeMountState }

func (u *stateUnmounter) Unmount() error {
	u.state.mu.Lock()
	u.state.unstageCalls++
	u.state.mu.Unlock()
	return nil
}

// newNodeServerWithFakes wires a NodeServer to a fakeMountState, bypassing
// all real mount-service and mountutil interactions. The health monitor is
// not started — tests drive checkAndRecoverVolumes directly for determinism.
func newNodeServerWithFakes(t *testing.T, state *fakeMountState) *NodeServer {
	t.Helper()

	ns := &NodeServer{
		Driver:        &SeaweedFsDriver{},
		volumeMutexes: NewKeyMutex(),
		stopCh:        make(chan struct{}),
		mounterFactory: func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
			return state.newMounter(), nil
		},
		capacityFn: func(volumeID string) (int64, error) {
			return 0, errors.New("no capacity in tests")
		},
		isHealthyFn: func(path string) bool {
			return state.healthy.Load()
		},
		cleanupStagingFn: func(path string) error {
			state.mu.Lock()
			state.cleanupCalls++
			state.mu.Unlock()
			// Remove the staging directory so the next Mount recreates it,
			// mirroring real cleanup behavior.
			return os.RemoveAll(path)
		},
		unmountFn: func(path string) error {
			state.mu.Lock()
			state.unmountCalls++
			state.mu.Unlock()
			return nil
		},
		bindMountFn: func(source, target string, readOnly bool) error {
			state.mu.Lock()
			state.bindMountCalls++
			state.bindMountTargets = append(state.bindMountTargets, target)
			state.mu.Unlock()
			// Create the target directory so checkMount on it returns false,
			// letting Publish proceed.
			return os.MkdirAll(target, 0755)
		},
	}
	return ns
}

// TestHealthMonitorRecoversStaleMount is the integration test for
// seaweedfs/seaweedfs-csi-driver#253. It walks through the full CSI
// lifecycle — stage → publish → simulated FUSE crash → recovery — and
// verifies that after recovery the volume has a fresh mount and all
// publish paths are re-bound.
func TestHealthMonitorRecoversStaleMount(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	root := t.TempDir()
	stagingPath := filepath.Join(root, "staging")
	publishA := filepath.Join(root, "podA", "mount")
	publishB := filepath.Join(root, "podB", "mount")

	volCtx := map[string]string{"collection": "c"}

	// --- Stage ---
	vol, err := ns.stageNewVolume("vol-1", stagingPath, volCtx, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = volCtx
	vol.readOnly = false
	ns.volumes.Store("vol-1", vol)

	if state.stageCalls != 1 {
		t.Fatalf("expected 1 stage call, got %d", state.stageCalls)
	}

	// --- Publish to two pods ---
	if err := vol.Publish(stagingPath, publishA, false); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	vol.AddPublishPath(publishA, false)

	if err := vol.Publish(stagingPath, publishB, true); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	vol.AddPublishPath(publishB, true)

	if state.bindMountCalls != 2 {
		t.Fatalf("expected 2 bind mount calls after publish, got %d", state.bindMountCalls)
	}

	// --- Simulate FUSE crash ---
	state.healthy.Store(false)

	// Sanity: health monitor should now consider the mount unhealthy.
	if ns.isHealthyFn(stagingPath) {
		t.Fatal("fake should report unhealthy after crash")
	}

	// --- Trigger one health check cycle ---
	ns.checkAndRecoverVolumes()

	// --- Verify recovery actions ---
	state.mu.Lock()
	defer state.mu.Unlock()

	// A second stage call means the FUSE mount was re-created.
	if state.stageCalls != 2 {
		t.Errorf("expected 2 stage calls after recovery, got %d", state.stageCalls)
	}
	// Staging was cleaned up before re-stage.
	if state.cleanupCalls != 1 {
		t.Errorf("expected 1 staging cleanup, got %d", state.cleanupCalls)
	}
	// Both stale bind mounts were unmounted.
	if state.unmountCalls != 2 {
		t.Errorf("expected 2 bind unmounts, got %d", state.unmountCalls)
	}
	// Both publish paths were re-bound (total 4: 2 initial + 2 recovery).
	if state.bindMountCalls != 4 {
		t.Errorf("expected 4 total bind mounts (2 initial + 2 recovery), got %d", state.bindMountCalls)
	}

	// --- Verify the replacement volume is tracked correctly ---
	got, ok := ns.volumes.Load("vol-1")
	if !ok {
		t.Fatal("volume missing from map after recovery")
	}
	newVol := got.(*Volume)
	if newVol == vol {
		t.Error("expected a replacement Volume instance after recovery")
	}
	if newVol.volContext["collection"] != "c" {
		t.Error("volContext not propagated to recovered volume")
	}

	// Both publish paths should be re-tracked on the new volume.
	seen := map[string]bool{}
	newVol.publishPaths.Range(func(k, v interface{}) bool {
		seen[k.(string)] = true
		return true
	})
	if !seen[publishA] || !seen[publishB] {
		t.Errorf("expected publish paths %q and %q tracked, got %v", publishA, publishB, seen)
	}
}

// TestHealthMonitorSkipsHealthyVolumes verifies the monitor does not
// disrupt volumes whose FUSE mount is still alive.
func TestHealthMonitorSkipsHealthyVolumes(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	vol, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{}, false)
	if err != nil {
		t.Fatalf("stageNewVolume: %v", err)
	}
	vol.volContext = map[string]string{}
	ns.volumes.Store("vol-1", vol)

	// Healthy throughout — one recovery sweep should be a no-op.
	ns.checkAndRecoverVolumes()

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stageCalls != 1 {
		t.Errorf("expected 1 stage call (no recovery), got %d", state.stageCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected 0 cleanup calls, got %d", state.cleanupCalls)
	}
}

// TestHealthMonitorSkipsVolumesWithoutContext verifies that volumes
// rebuilt from an existing mount (no volContext) are left alone — they
// cannot be auto-recovered and must be re-staged by kubelet.
func TestHealthMonitorSkipsVolumesWithoutContext(t *testing.T) {
	state := newFakeMountState()
	ns := newNodeServerWithFakes(t, state)

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Volume has a StagedPath but no volContext — mimics the rebuild path.
	vol := &Volume{
		VolumeId:   "vol-1",
		StagedPath: stagingPath,
		driver:     ns.Driver,
	}
	ns.volumes.Store("vol-1", vol)

	state.healthy.Store(false)
	ns.checkAndRecoverVolumes()

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stageCalls != 0 {
		t.Errorf("expected no stage calls for context-less volume, got %d", state.stageCalls)
	}
	if state.cleanupCalls != 0 {
		t.Errorf("expected no cleanup calls, got %d", state.cleanupCalls)
	}
}
