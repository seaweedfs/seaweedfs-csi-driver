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
	volume, err := resolvePersistentVolume(ctx, client, driverName, volumeId)
	if err != nil {
		return 0, err
	}
	return persistentVolumeCapacity(volume)
}

// GetVolumeAttributes returns the CSI volume attributes of the persistent
// volume backing volumeId, resolved the same way capacity is. The supplied
// context bounds the lookup; a 30s ceiling is applied when ctx carries no
// deadline so a slow or cancelled caller does not block indefinitely.
func GetVolumeAttributes(ctx context.Context, driverName, volumeId string) (map[string]string, error) {
	client, err := newInCluster()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	volume, err := resolvePersistentVolume(ctx, client, driverName, volumeId)
	if err != nil {
		return nil, err
	}
	if volume.Spec.CSI == nil {
		return nil, fmt.Errorf("persistent volume %q has no CSI source", volume.Name)
	}
	attrs := make(map[string]string, len(volume.Spec.CSI.VolumeAttributes))
	for k, v := range volume.Spec.CSI.VolumeAttributes {
		attrs[k] = v
	}
	return attrs, nil
}

// resolvePersistentVolume finds the PV for a CSI volume id: direct Get by the
// PV name when the handle's last element is a DNS-1123 subdomain, otherwise a
// list matching the full CSI volume handle.
func resolvePersistentVolume(ctx context.Context, client kubernetes.Interface, driverName, volumeId string) (*corev1.PersistentVolume, error) {
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
			return volume, nil
		}
	}

	// Fallback: the handle does not map to a PV name (e.g. a static volume whose
	// handle is an arbitrary filer path), so match by CSI volume handle instead.
	volumes, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list persistent volumes for CSI volume handle %q: %w", volumeId, err)
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
			return nil, fmt.Errorf("multiple persistent volumes use CSI volume handle %q", volumeId)
		}
		matched = volume
	}
	if matched == nil {
		return nil, fmt.Errorf("persistent volume with name or CSI volume handle %q not found", volumeId)
	}
	return matched, nil
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

func GetNodeLabels(nodeName string) (map[string]string, error) {
	client, err := newInCluster()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return getNodeLabels(ctx, client, nodeName)
}

func getNodeLabels(ctx context.Context, client kubernetes.Interface, nodeName string) (map[string]string, error) {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %q: %w", nodeName, err)
	}
	return node.Labels, nil
}
