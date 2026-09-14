//go:build linux
// +build linux

package driver

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// newTestNodeServer returns a NodeServer with all mount/filesystem
// factories replaced so tests do not touch the mount service or k8s API.
// The health monitor is not started.
func newTestNodeServer(t *testing.T, fake *fakeMounter) *NodeServer {
	t.Helper()
	ns := &NodeServer{
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
		isHealthyFn:      func(path string) bool { return true },
		cleanupStagingFn: func(path string) error { return nil },
		unmountFn:        func(path string) error { return nil },
		bindMountFn: func(source, target string, readOnly bool) error {
			// Create the target so follow-up checkMount calls see a directory.
			return nil
		},
	}
	return ns
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

func TestStageNewVolumeAppliesPersistedVolumeAttributes(t *testing.T) {
	var capturedVolContext map[string]string
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.mounterFactory = func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
		capturedVolContext = volContext
		return &fakeMounter{}, nil
	}
	ns.vacLoader = func(_ context.Context, volumeID string) (map[string]string, error) {
		return map[string]string{
			"diskType":          "ssd",
			"concurrentReaders": "64",
			"collection":        "must-not-apply",
		}, nil
	}

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if _, err := ns.stageNewVolume("/buckets/pvc-1", stagingPath, map[string]string{"collection": "c", "concurrentReaders": "128"}, false); err != nil {
		t.Fatalf("stageNewVolume failed: %v", err)
	}

	if capturedVolContext["diskType"] != "ssd" {
		t.Errorf("persisted diskType not applied: %v", capturedVolContext)
	}
	if capturedVolContext["concurrentReaders"] != "64" {
		t.Errorf("persisted concurrentReaders not overriding PV value: %v", capturedVolContext)
	}
	if capturedVolContext["collection"] != "c" {
		t.Errorf("structural key leaked from persisted store: %v", capturedVolContext)
	}
}

func TestStageNewVolumeFailsWhenVacStoreUnreadable(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.vacLoader = func(_ context.Context, volumeID string) (map[string]string, error) {
		return nil, errors.New("filer unreachable")
	}

	stagingPath := filepath.Join(t.TempDir(), "staging")
	_, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{"concurrentReaders": "128"}, false)
	if err == nil {
		t.Fatal("stageNewVolume must fail when the VAC store is unreadable")
	}
}

func TestStageNewVolumeFallsBackWhenNoVacEntry(t *testing.T) {
	var capturedVolContext map[string]string
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.mounterFactory = func(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
		capturedVolContext = volContext
		return &fakeMounter{}, nil
	}
	ns.vacLoader = func(_ context.Context, volumeID string) (map[string]string, error) {
		return nil, nil
	}

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if _, err := ns.stageNewVolume("vol-1", stagingPath, map[string]string{"concurrentReaders": "128"}, false); err != nil {
		t.Fatalf("stageNewVolume must proceed when no VAC entry exists: %v", err)
	}
	if capturedVolContext["concurrentReaders"] != "128" {
		t.Errorf("PV attributes must stay intact when no VAC entry exists: %v", capturedVolContext)
	}
}

func TestStageNewVolumeRejectsWritebackDlmCombo(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.vacLoader = func(_ context.Context, volumeID string) (map[string]string, error) {
		return map[string]string{"writebackCache": "true"}, nil
	}

	stagingPath := filepath.Join(t.TempDir(), "staging")
	_, err := ns.stageNewVolume("/buckets/pvc-1", stagingPath, map[string]string{"dlm": "true"}, false)
	if err == nil {
		t.Fatal("expected combo rejection")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error should name the exclusivity: %v", err)
	}
}

func TestStageNewVolumeValidatesMergedValues(t *testing.T) {
	ns := newTestNodeServer(t, &fakeMounter{})
	ns.vacLoader = func(_ context.Context, volumeID string) (map[string]string, error) {
		return map[string]string{"concurrentReaders": "64"}, nil
	}

	stagingPath := filepath.Join(t.TempDir(), "staging")
	// Static PV attribute with a typo: stage must fail with an invalid
	// argument error instead of a weed mount flag parse failure.
	_, err := ns.stageNewVolume("/buckets/pvc-1", stagingPath, map[string]string{"writebackCache": "yes"}, false)
	if err == nil || !strings.Contains(err.Error(), "invalid argument") {
		t.Fatalf("expected invalid argument error, got %v", err)
	}
}
