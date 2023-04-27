package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
	"k8s.io/utils/mount"
)

func NewNodeServer(n *SeaweedFsDriver) *NodeServer {
	if err := removeDirContent(n.CacheDir); err != nil {
		glog.Warning("error cleaning up cache dir")
	}

	return &NodeServer{
		Driver:        n,
		volumeMutexes: NewKeyMutex(),
	}
}

func NewIdentityServer(d *SeaweedFsDriver) *IdentityServer {
	return &IdentityServer{
		Driver: d,
	}
}

func NewControllerServer(d *SeaweedFsDriver) (*ControllerServer, error) {

	// Get the Kubernetes configuration
	c, err := rest.InClusterConfig()
	if err != nil {
		fmt.Errorf("failed to get Kubernetes config: %v", err)
	}

	return &ControllerServer{
		Driver: d,
		config: c,
	}, nil
}

func NewControllerServiceCapability(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

func ParseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("Invalid endpoint: %v", ep)
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	glog.V(3).Infof("GRPC %s request %+v", info.FullMethod, req)
	resp, err := handler(ctx, req)
	if err != nil {
		glog.Errorf("GRPC error: %v", err)
	}
	glog.V(3).Infof("GRPC %s response %+v", info.FullMethod, resp)
	return resp, err
}

func checkMount(targetPath string) (bool, error) {
	mounter := mount.New("")
	notMnt, err := mount.IsNotMountPoint(mounter, targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err = os.MkdirAll(targetPath, 0750); err != nil {
				return false, err
			}
			notMnt = true
		} else if mount.IsCorruptedMnt(err) {
			if err := mounter.Unmount(targetPath); err != nil {
				return false, err
			}
			notMnt, err = mount.IsNotMountPoint(mounter, targetPath)
		} else {
			return false, err
		}
	}
	return notMnt, nil
}

func removeDirContent(path string) error {
	files, err := filepath.Glob(filepath.Join(path, "*"))
	if err != nil {
		return err
	}

	for _, file := range files {
		err = os.RemoveAll(file)
		if err != nil {
			return err
		}
	}

	return nil
}

type KeyMutex struct {
	mutexes sync.Map
}

func NewKeyMutex() *KeyMutex {
	return &KeyMutex{}
}

func (km *KeyMutex) GetMutex(key string) *sync.Mutex {
	m, _ := km.mutexes.LoadOrStore(key, &sync.Mutex{})

	return m.(*sync.Mutex)
}

func (km *KeyMutex) RemoveMutex(key string) {
	km.mutexes.Delete(key)
}

func CheckDataLocality(dataLocality *datalocality.DataLocality, dataCenter *string) error {
	if *dataLocality != datalocality.None && *dataCenter == "" {
		return fmt.Errorf("dataLocality set, but not all locality-definitions were set")
	}
	return nil
}
