package k8s

import (
	"context"
	"fmt"
	"path"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func newInCluster() (*kubernetes.Clientset, error) {
	//creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %v", err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster client: %v", err)
	}
	return clientset, nil
}

func GetVolumeCapacity(driverName, volumeId string) (int64, error) {
	client, err := newInCluster()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return getVolumeCapacity(ctx, client, driverName, volumeId)
}

func getVolumeCapacity(ctx context.Context, client kubernetes.Interface, driverName, volumeId string) (int64, error) {
	// Fast path: avoid listing every PersistentVolume in the cluster on each
	// stage. Legacy dynamic volumes used the PV name directly as the CSI volume
	// handle, while newer ones use a full filer path (e.g. "/buckets/pvc-xxxx")
	// whose last component is still the PV name. In both cases a direct Get by
	// that name resolves the volume, and we confirm the match before trusting it.
	pvName := path.Base(volumeId)
	if len(validation.IsDNS1123Subdomain(pvName)) == 0 {
		if volume, err := client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{}); err == nil &&
			volume.Spec.CSI != nil && volume.Spec.CSI.Driver == driverName &&
			(volume.Spec.CSI.VolumeHandle == volumeId || volume.Name == volumeId) {
			return persistentVolumeCapacity(volume)
		}
	}

	// Fallback: the handle does not map to a PV name (e.g. a static volume whose
	// handle is an arbitrary filer path), so match by CSI volume handle instead.
	volumes, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list persistent volumes for CSI volume handle %q: %w", volumeId, err)
	}

	var matched *corev1.PersistentVolume
	for i := range volumes.Items {
		volume := &volumes.Items[i]
		if volume.Spec.CSI == nil ||
			volume.Spec.CSI.Driver != driverName ||
			volume.Spec.CSI.VolumeHandle != volumeId {
			continue
		}
		if matched != nil {
			return 0, fmt.Errorf("multiple persistent volumes use CSI volume handle %q", volumeId)
		}
		matched = volume
	}
	if matched == nil {
		return 0, fmt.Errorf("persistent volume with name or CSI volume handle %q not found", volumeId)
	}

	return persistentVolumeCapacity(matched)
}

func persistentVolumeCapacity(volume *corev1.PersistentVolume) (int64, error) {
	storage := volume.Spec.Capacity.Storage()
	if storage == nil {
		return 0, fmt.Errorf("persistent volume %q has no storage capacity", volume.Name)
	}
	capacity, ok := storage.AsInt64()
	if !ok {
		return 0, fmt.Errorf("persistent volume %q storage capacity does not fit in int64", volume.Name)
	}
	return capacity, nil
}
