//go:build linux
// +build linux

package driver

import (
	"errors"
	"path/filepath"
	"testing"
)

// fakeMounter records Mount/Unmount calls and reports success without
// touching the real mount service.
type fakeMounter struct {
	mountCalls   int
	unmountCalls int
	lastTarget   string
	mountErr     error
}

func (f *fakeMounter) Mount(target string) (Unmounter, error) {
	f.mountCalls++
	f.lastTarget = target
	if f.mountErr != nil {
		return nil, f.mountErr
	}
	return &fakeUnmounter{parent: f}, nil
}

type fakeUnmounter struct {
	parent *fakeMounter
}

func (f *fakeUnmounter) Unmount() error {
	f.parent.unmountCalls++
	return nil
}

// newTestNodeServer returns a NodeServer with the mounter and capacity
// factories replaced so tests do not touch the mount service or k8s API.
// The health monitor is not started.
func newTestNodeServer(t *testing.T, fake *fakeMounter) *NodeServer {
	t.Helper()
	return &NodeServer{
		Driver:        &SeaweedFsDriver{},
		volumeMutexes: NewKeyMutex(),
		stopCh:        make(chan struct{}),
		mounterFactory: func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
			return fake, nil
		},
		capacityFn: func(volumeID string) (int64, error) {
			// Skip quota application in tests.
			return 0, errors.New("capacity not available in test")
		},
	}
}

func TestStageNewVolumeUsesInjectedFactories(t *testing.T) {
	fake := &fakeMounter{}
	ns := newTestNodeServer(t, fake)

	// Volume.Stage's checkMount will try to create the staging directory if
	// it does not exist, so point it at a tempdir.
	stagingPath := filepath.Join(t.TempDir(), "staging")

	vol, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{"collection": "c"}, false)
	if err != nil {
		t.Fatalf("stageNewVolume failed: %v", err)
	}
	if vol == nil {
		t.Fatal("stageNewVolume returned nil volume")
	}
	if fake.mountCalls != 1 {
		t.Errorf("expected 1 mount call, got %d", fake.mountCalls)
	}
	if fake.lastTarget != stagingPath {
		t.Errorf("expected mount target %q, got %q", stagingPath, fake.lastTarget)
	}
	if vol.VolumeId != "vol-1" {
		t.Errorf("expected volume id %q, got %q", "vol-1", vol.VolumeId)
	}
	if vol.StagedPath != stagingPath {
		t.Errorf("expected staged path %q, got %q", stagingPath, vol.StagedPath)
	}
}

func TestStageNewVolumePropagatesMounterError(t *testing.T) {
	wantErr := errors.New("mount refused")
	ns := newTestNodeServer(t, nil)
	ns.mounterFactory = func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
		return nil, wantErr
	}

	_, err := ns.stageNewVolume("vol-1", t.TempDir(), nil, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
}
