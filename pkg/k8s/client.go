package k8s

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		panic(err.Error())
	}
	return clientset, nil
}

func GetVolumeCapacity(volumeId string) (int64, error) {
	client, err := newInCluster()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if volume, err := client.CoreV1().PersistentVolumes().Get(ctx, volumeId, metav1.GetOptions{}); err != nil {
		return 0, err
	} else {
		storage := volume.Spec.Capacity.Storage()
		capacity, _ := storage.AsInt64()
		return capacity, nil
	}
}
