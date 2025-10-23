package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

var mountutil = mount.New("")

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

	cacheBase := m.driver.CacheDir
	if cacheBase == "" {
		cacheBase = os.TempDir()
	}
	cacheDir := filepath.Join(cacheBase, m.volumeID)
	localSocket := mountmanager.LocalSocketPath(m.volumeID)

	args, err := m.buildMountArgs(target, cacheDir, localSocket, filers)
	if err != nil {
		return nil, err
	}

	req := &mountmanager.MountRequest{
		VolumeID:    m.volumeID,
		TargetPath:  target,
		CacheDir:    cacheDir,
		MountArgs:   args,
		LocalSocket: localSocket,
	}

	_, err = m.client.Mount(req)
	if err != nil {
		return nil, err
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

func (m *mountServiceMounter) buildMountArgs(targetPath, cacheDir, localSocket string, filers []string) ([]string, error) {
	volumeContext := m.volContext
	if volumeContext == nil {
		volumeContext = map[string]string{}
	}

	path := volumeContext["path"]
	if path == "" {
		path = fmt.Sprintf("/buckets/%s", m.volumeID)
	}

	collection := volumeContext["collection"]
	if collection == "" {
		collection = m.volumeID
	}

	args := []string{
		"-logtostderr=true",
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", targetPath),
		fmt.Sprintf("-localSocket=%s", localSocket),
		fmt.Sprintf("-cacheDir=%s", cacheDir),
	}

	if m.readOnly {
		args = append(args, "-readOnly")
	}

	argsMap := map[string]string{
		"collection":         collection,
		"filer":              strings.Join(filers, ","),
		"filer.path":         path,
		"cacheCapacityMB":    strconv.Itoa(m.driver.CacheCapacityMB),
		"concurrentWriters":  strconv.Itoa(m.driver.ConcurrentWriters),
		"map.uid":            m.driver.UidMap,
		"map.gid":            m.driver.GidMap,
		"disk":               "",
		"dataCenter":         "",
		"replication":        "",
		"ttl":                "",
		"chunkSizeLimitMB":   "",
		"volumeServerAccess": "",
		"readRetryTime":      "",
	}

	dataLocality := m.driver.DataLocality
	if contextLocality, ok := volumeContext["dataLocality"]; ok && contextLocality != "" {
		if dl, ok := datalocality.FromString(contextLocality); ok {
			dataLocality = dl
		} else {
			return nil, fmt.Errorf("invalid volumeContext dataLocality: %s", contextLocality)
		}
	}

	dataCenter := m.driver.DataCenter
	if err := CheckDataLocality(&dataLocality, &dataCenter); err != nil {
		return nil, err
	}

	switch dataLocality {
	case datalocality.Write_preferLocalDc:
		argsMap["dataCenter"] = dataCenter
	}

	parameterArgMap := map[string]string{
		"uidMap":    "map.uid",
		"gidMap":    "map.gid",
		"filerPath": "filer.path",
		"diskType":  "disk",
	}

	ignoredArgs := map[string]struct{}{"dataLocality": {}}

	for key, value := range volumeContext {
		if _, ignored := ignoredArgs[key]; ignored {
			continue
		}
		if mapped, ok := parameterArgMap[key]; ok {
			key = mapped
		}
		if _, ok := argsMap[key]; !ok {
			glog.Warningf("VolumeContext '%s' ignored", key)
			continue
		}
		if value != "" {
			argsMap[key] = value
		}
	}

	for key, value := range argsMap {
		if value == "" {
			continue
		}
		args = append(args, fmt.Sprintf("-%s=%s", key, value))
	}

	return args, nil
}
