package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// Implements Mounter
type seaweedFsMounter struct {
	volumeID   string
	path       string
	collection string
	readOnly   bool
	driver     *SeaweedFsDriver
	volContext map[string]string
}

type seaweedFsUnmounter struct {
	unmounter Unmounter
	cacheDir  string
}

const (
	seaweedFsCmd = "weed"
)

func newSeaweedFsMounter(volumeID string, path string, collection string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
	return &seaweedFsMounter{
		volumeID:   volumeID,
		path:       path,
		collection: collection,
		readOnly:   readOnly,
		driver:     driver,
		volContext: volContext,
	}, nil
}

func (seaweedFs *seaweedFsMounter) Mount(target string) (Unmounter, error) {
	glog.V(0).Infof("mounting %v %s to %s", seaweedFs.driver.filers, seaweedFs.path, target)

	var filers []string
	for _, address := range seaweedFs.driver.filers {
		filers = append(filers, string(address))
	}

	// CacheDir should be always defined - we use temp dir in case it is not defined
	// we need to use predictable cache path, because we need to clean it up on unstage
	cacheDir := filepath.Join(seaweedFs.driver.CacheDir, seaweedFs.volumeID)

	// Final args
	args := []string{
		"-logtostderr=true",
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", target),
		fmt.Sprintf("-localSocket=%s", GetLocalSocket(seaweedFs.volumeID)),
		fmt.Sprintf("-cacheDir=%s", cacheDir),
	}

	if seaweedFs.readOnly {
		args = append(args, "-readOnly")
	}

	// Handle volumeCapacity from controllerserver.go:51
	if value, ok := seaweedFs.volContext["volumeCapacity"]; ok {
		capacityMB := parseVolumeCapacity(value)
		args = append(args, fmt.Sprintf("-collectionQuotaMB=%d", capacityMB))
	}

	// Values for override-able args
	//  Whitelist for merging with volContext
	argsMap := map[string]string{
		"collection":         seaweedFs.collection,
		"filer":              strings.Join(filers, ","),
		"filer.path":         seaweedFs.path,
		"cacheCapacityMB":    fmt.Sprint(seaweedFs.driver.CacheCapacityMB),
		"concurrentWriters":  fmt.Sprint(seaweedFs.driver.ConcurrentWriters),
		"map.uid":            seaweedFs.driver.UidMap,
		"map.gid":            seaweedFs.driver.GidMap,
		"disk":               "",
		"dataCenter":         "",
		"replication":        "",
		"ttl":                "",
		"chunkSizeLimitMB":   "",
		"volumeServerAccess": "",
		"readRetryTime":      "",
	}

	// Handle DataLocality
	dataLocality := seaweedFs.driver.DataLocality
	// Try to override when set in context
	if dataLocalityStr, ok := seaweedFs.volContext["dataLocality"]; ok {
		// Convert to enum
		dataLocalityRes, ok := datalocality.FromString(dataLocalityStr)
		if !ok {
			glog.Warning("volumeContext 'dataLocality' invalid")
		} else {
			dataLocality = dataLocalityRes
		}
	}
	if err := CheckDataLocality(&dataLocality, &seaweedFs.driver.DataCenter); err != nil {
		return nil, err
	}
	// Settings based on type
	switch dataLocality {
	case datalocality.Write_preferLocalDc:
		argsMap["dataCenter"] = seaweedFs.driver.DataCenter
	}

	// volContext-parameter -> mount-arg
	parameterArgMap := map[string]string{
		"uidMap":    "map.uid",
		"gidMap":    "map.gid",
		"filerPath": "filer.path",
		// volumeContext has "diskType", but mount-option is "disk", converting for backwards compatability
		"diskType": "disk",
	}

	// Explicitly ignored volContext args e.g. handled somewhere else
	ignoreArgs := []string{
		"volumeCapacity",
		"dataLocality",
	}

	//	Merge volContext into argsMap with key-mapping
	for arg, value := range seaweedFs.volContext {
		if in_arr(ignoreArgs, arg) {
			continue
		}

		// Check if key-mapping exists
		newArg, ok := parameterArgMap[arg]
		if ok {
			arg = newArg
		}

		// Check if arg can be applied
		if _, ok := argsMap[arg]; !ok {
			glog.Warningf("VolumeContext '%s' ignored", arg)
			continue
		}

		// Write to args-map
		argsMap[arg] = value
	}

	// Convert Args-Map to args
	for arg, value := range argsMap {
		if value != "" { // ignore empty values
			args = append(args, fmt.Sprintf("-%s=%s", arg, value))
		}
	}

	u, err := fuseMount(target, seaweedFsCmd, args)
	if err != nil {
		glog.Errorf("mount %v %s to %s: %s", seaweedFs.driver.filers, seaweedFs.path, target, err)
	}

	return &seaweedFsUnmounter{unmounter: u, cacheDir: cacheDir}, err
}

func (su *seaweedFsUnmounter) Unmount() error {
	err := su.unmounter.Unmount()
	err2 := os.RemoveAll(su.cacheDir)
	if err2 != nil {
		glog.Warningf("error removing cache from: %s, err: %v", su.cacheDir, err2)
	}
	return err
}

func GetLocalSocket(volumeID string) string {
	montDirHash := util.HashToInt32([]byte(volumeID))
	if montDirHash < 0 {
		montDirHash = -montDirHash
	}

	socket := fmt.Sprintf("/tmp/seaweedfs-mount-%d.sock", montDirHash)
	return socket
}

func parseVolumeCapacity(volumeCapacity string) int64 {
	var capacity int64

	if vCap, err := strconv.ParseInt(volumeCapacity, 10, 64); err != nil {
		glog.Errorf("volumeCapacity %s can not be parsed to Int64, err is: %v", volumeCapacity, err)
	} else {
		capacity = vCap
	}

	capacityMB := capacity / 1024 / 1024
	return capacityMB
}

func in_arr(arr []string, val string) bool {
	for _, v := range arr {
		if val == v {
			return true
		}
	}
	return false
}
