package driver

import (
	"fmt"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

type Unmounter interface {
	Unmount() error
}

type Mounter interface {
	Mount(target string) (Unmounter, error)
}

type mountServiceMounter struct {
	driver     *SeaweedFsDriver
	volumeID   string
	readOnly   bool
	volContext map[string]string
	client     *mountmanager.Client
}

type mountServiceUnmounter struct {
	client   *mountmanager.Client
	volumeID string
}

func newMounter(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
	client, err := mountmanager.NewClient(driver.mountEndpoint)
	if err != nil {
		return nil, err
	}

	contextCopy := make(map[string]string, len(volContext))
	for k, v := range volContext {
		contextCopy[k] = v
	}

	return &mountServiceMounter{
		driver:     driver,
		volumeID:   volumeID,
		readOnly:   readOnly,
		volContext: contextCopy,
		client:     client,
	}, nil
}

func (m *mountServiceMounter) Mount(target string) (Unmounter, error) {
	if target == "" {
		return nil, fmt.Errorf("target path is required")
	}

	filers := make([]string, len(m.driver.filers))
	for i, address := range m.driver.filers {
		filers[i] = string(address)
	}

	req := &mountmanager.MountRequest{
		VolumeID:          m.volumeID,
		TargetPath:        target,
		ReadOnly:          m.readOnly,
		Filers:            filers,
		CacheDir:          m.driver.CacheDir,
		CacheCapacityMB:   m.driver.CacheCapacityMB,
		ConcurrentWriters: m.driver.ConcurrentWriters,
		UidMap:            m.driver.UidMap,
		GidMap:            m.driver.GidMap,
		DataCenter:        m.driver.DataCenter,
		DataLocality:      m.driver.DataLocality.String(),
		VolumeContext:     m.volContext,
	}

	resp, err := m.client.Mount(req)
	if err != nil {
		return nil, err
	}

	expectedSocket := mountmanager.LocalSocketPath(m.volumeID)
	if resp.LocalSocket != "" && resp.LocalSocket != expectedSocket {
		glog.Warningf("mount service returned socket %s for volume %s (expected %s)", resp.LocalSocket, m.volumeID, expectedSocket)
	}

	return &mountServiceUnmounter{
		client:   m.client,
		volumeID: m.volumeID,
	}, nil
}

func (u *mountServiceUnmounter) Unmount() error {
	_, err := u.client.Unmount(&mountmanager.UnmountRequest{VolumeID: u.volumeID})
	return err
}
