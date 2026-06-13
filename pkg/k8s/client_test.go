package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetVolumeCapacityByPersistentVolumeName(t *testing.T) {
	client := fake.NewSimpleClientset(newPersistentVolume("pvc-legacy", "pvc-legacy", "5Gi"))

	got, err := getVolumeCapacity(context.Background(), client, "seaweedfs-csi-driver", "pvc-legacy")
	if err != nil {
		t.Fatalf("getVolumeCapacity: %v", err)
	}
	if want := int64(5 * 1024 * 1024 * 1024); got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
}

func TestGetVolumeCapacityByCSIVolumeHandle(t *testing.T) {
	client := fake.NewSimpleClientset(newPersistentVolume(
		"pvc-1234",
		"/buckets/pvc-1234",
		"10Gi",
	))

	got, err := getVolumeCapacity(context.Background(), client, "seaweedfs-csi-driver", "/buckets/pvc-1234")
	if err != nil {
		t.Fatalf("getVolumeCapacity: %v", err)
	}
	if want := int64(10 * 1024 * 1024 * 1024); got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
}

func TestGetVolumeCapacityRejectsDuplicateHandles(t *testing.T) {
	client := fake.NewSimpleClientset(
		newPersistentVolume("pv-a", "/shared/path", "1Gi"),
		newPersistentVolume("pv-b", "/shared/path", "2Gi"),
	)

	if _, err := getVolumeCapacity(context.Background(), client, "seaweedfs-csi-driver", "/shared/path"); err == nil {
		t.Fatal("expected duplicate volume handles to return an error")
	}
}

func TestGetVolumeCapacityScopesHandleToDriver(t *testing.T) {
	otherDriverVolume := newPersistentVolume("other-pv", "/shared/path", "1Gi")
	otherDriverVolume.Spec.CSI.Driver = "other.csi.example"
	client := fake.NewSimpleClientset(
		otherDriverVolume,
		newPersistentVolume("seaweedfs-pv", "/shared/path", "2Gi"),
	)

	got, err := getVolumeCapacity(context.Background(), client, "seaweedfs-csi-driver", "/shared/path")
	if err != nil {
		t.Fatalf("getVolumeCapacity: %v", err)
	}
	if want := int64(2 * 1024 * 1024 * 1024); got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
}

func TestGetVolumeCapacityIgnoresNameCollisionOnFastPath(t *testing.T) {
	// A PV named "data" exists but belongs to a different volume handle. The
	// fast-path Get by basename ("data") must not match it; the handle lookup
	// has to fall back to the List scan and find the correct volume.
	decoy := newPersistentVolume("data", "/some/other/path", "1Gi")
	client := fake.NewSimpleClientset(
		decoy,
		newPersistentVolume("real-pv", "/mnt/data", "7Gi"),
	)

	got, err := getVolumeCapacity(context.Background(), client, "seaweedfs-csi-driver", "/mnt/data")
	if err != nil {
		t.Fatalf("getVolumeCapacity: %v", err)
	}
	if want := int64(7 * 1024 * 1024 * 1024); got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
}

func newPersistentVolume(name, handle, capacity string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(capacity),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "seaweedfs-csi-driver",
					VolumeHandle: handle,
				},
			},
		},
	}
}
