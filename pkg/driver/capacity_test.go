package driver

import (
	"errors"
	"testing"
)

func TestResolveVolumeCapacityUsesVolumeContextFallback(t *testing.T) {
	ns := &NodeServer{
		capacityFn: func(volumeID string) (int64, error) {
			return 0, errors.New("Kubernetes API unavailable")
		},
	}

	got, ok, err := ns.resolveVolumeCapacity("/buckets/pvc-1234", map[string]string{
		volumeCapacityKey: "5368709120",
	})
	if err != nil {
		t.Fatalf("resolveVolumeCapacity: %v", err)
	}
	if !ok {
		t.Fatal("expected volume capacity to be available")
	}
	if want := int64(5368709120); got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
}

func TestResolveVolumeCapacityPrefersOrchestratorValue(t *testing.T) {
	ns := &NodeServer{
		capacityFn: func(volumeID string) (int64, error) {
			return 10 * 1024 * 1024 * 1024, nil
		},
	}

	got, ok, err := ns.resolveVolumeCapacity("pvc-1234", map[string]string{
		volumeCapacityKey: "5368709120",
	})
	if err != nil {
		t.Fatalf("resolveVolumeCapacity: %v", err)
	}
	if !ok {
		t.Fatal("expected volume capacity to be available")
	}
	if want := int64(10 * 1024 * 1024 * 1024); got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
}

func TestResolveVolumeCapacityRejectsInvalidContextValue(t *testing.T) {
	ns := &NodeServer{
		capacityFn: func(volumeID string) (int64, error) {
			return 0, errors.New("Kubernetes API unavailable")
		},
	}

	if _, _, err := ns.resolveVolumeCapacity("pvc-1234", map[string]string{
		volumeCapacityKey: "not-a-size",
	}); err == nil {
		t.Fatal("expected invalid volume capacity to return an error")
	}
}

func TestResolveVolumeCapacityAllowsUnavailableCapacity(t *testing.T) {
	ns := &NodeServer{
		capacityFn: func(volumeID string) (int64, error) {
			return 0, errors.New("Kubernetes API unavailable")
		},
	}

	_, ok, err := ns.resolveVolumeCapacity("pvc-1234", nil)
	if err != nil {
		t.Fatalf("resolveVolumeCapacity: %v", err)
	}
	if ok {
		t.Fatal("expected volume capacity to be unavailable")
	}
}
