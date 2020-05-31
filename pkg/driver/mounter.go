package driver

import (
	"fmt"
	"time"

	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/golang/glog"
	"google.golang.org/grpc"
	"os/exec"
	"k8s.io/utils/mount"
)

// Config holds values to configure the driver
type Config struct {
	// Region          string
	Filer string
}

type Mounter interface {
	Mount(target string) error
}

func newMounter(bucketName string, cfg *Config) (Mounter, error) {
	return newSeaweedFsMounter(bucketName, cfg)
}

func fuseMount(path string, command string, args []string) error {
	cmd := exec.Command(command, args...)
	glog.V(3).Infof("Mounting fuse with command: %s and args: %s", command, args)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error fuseMount command: %s\nargs: %s\noutput: %s", command, args, out)
	}

	return waitForMount(path, 10*time.Second)
}

func fuseUnmount(path string) error {
	if err := mount.New("").Unmount(path); err != nil {
		return err
	}
	// as fuse quits immediately, we will try to wait until the process is done
	process, err := findFuseMountProcess(path)
	if err != nil {
		glog.Errorf("Error getting PID of fuse mount: %s", err)
		return nil
	}
	if process == nil {
		glog.Warningf("Unable to find PID of fuse mount %s, it must have finished already", path)
		return nil
	}
	glog.Infof("Found fuse pid %v of mount %s, checking if it still runs", process.Pid, path)
	return waitForProcess(process, 1)
}

func newConfigFromSecrets(secrets map[string]string) *Config {
	t := &Config{
		Filer: secrets["filer"],
	}
	return t
}

var _ = filer_pb.FilerClient(&Config{})

func (cfg *Config) WithFilerClient(fn func(filer_pb.SeaweedFilerClient) error) error {

	filerGrpcAddress, parseErr := pb.ParseServerToGrpcAddress(cfg.Filer)
	if parseErr != nil {
		return fmt.Errorf("failed to parse filer %v: %v", filerGrpcAddress, parseErr)
	}

	grpcDialOption := security.LoadClientTLS(util.GetViper(), "grpc.client")

	return pb.WithCachedGrpcClient(func(grpcConnection *grpc.ClientConn) error {
		client := filer_pb.NewSeaweedFilerClient(grpcConnection)
		return fn(client)
	}, filerGrpcAddress, grpcDialOption)

}
func (cfg *Config) AdjustedUrl(hostAndPort string) string {
	return hostAndPort
}
