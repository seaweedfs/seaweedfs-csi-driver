package driver

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"k8s.io/utils/mount"
)

func NewNodeServer(n *SeaweedFsDriver) *NodeServer {

	return &NodeServer{
		Driver:        n,
		volumeMutexes: NewKeyMutex(32),
	}
}

func NewIdentityServer(d *SeaweedFsDriver) *IdentityServer {
	return &IdentityServer{
		Driver: d,
	}
}

func NewControllerServer(d *SeaweedFsDriver) *ControllerServer {
	return &ControllerServer{
		Driver: d,
	}
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
	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err = os.MkdirAll(targetPath, 0750); err != nil {
				return false, err
			}
			notMnt = true
		} else {
			return false, err
		}
	}
	return notMnt, nil
}

type KeyMutex struct {
	mutexes []sync.RWMutex
	size    int32
}

func NewKeyMutex(size int32) *KeyMutex {
	return &KeyMutex{
		mutexes: make([]sync.RWMutex, size),
		size:    size,
	}
}

func (km *KeyMutex) GetMutex(key string) *sync.RWMutex {
	index := util.HashToInt32([]byte(key))
	if index < 0 {
		index = -index
	}

	return &km.mutexes[index%km.size]
}
