package driver

import (
	"fmt"
	"os"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

const (
	driverName = "seaweedfs-csi-driver"
)

var (
	version = "1.0.0"
)

type SeaweedFsDriver struct {
	name    string
	nodeID  string
	version string

	endpoint string

	vcap  []*csi.VolumeCapability_AccessMode
	cscap []*csi.ControllerServiceCapability

	filers            []pb.ServerAddress
	filerIndex        int
	grpcDialOption    grpc.DialOption
	ConcurrentWriters int
	CacheSizeMB       int64
	CacheDir          string
	UidMap            string
	GidMap            string
}

func NewSeaweedFsDriver(filer, nodeID, endpoint string) *SeaweedFsDriver {

	glog.Infof("Driver: %v version: %v", driverName, version)

	util.LoadConfiguration("security", false)

	n := &SeaweedFsDriver{
		endpoint:       endpoint,
		nodeID:         nodeID,
		name:           driverName,
		version:        version,
		filers:         pb.ServerAddresses(filer).ToAddresses(),
		grpcDialOption: security.LoadClientTLS(util.GetViper(), "grpc.client"),
	}

	n.AddVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	})
	n.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	})

	return n
}

func (n *SeaweedFsDriver) initClient() error {
	_, err := rest.InClusterConfig()
	if err != nil {
		klog.Errorf("Failed to get cluster config with error: %v\n", err)
		os.Exit(1)
	}
	return nil
}

func (n *SeaweedFsDriver) Run() {
	s := NewNonBlockingGRPCServer()
	s.Start(n.endpoint,
		NewIdentityServer(n),
		NewControllerServer(n),
		NewNodeServer(n))
	s.Wait()
}

func (n *SeaweedFsDriver) AddVolumeCapabilityAccessModes(vc []csi.VolumeCapability_AccessMode_Mode) []*csi.VolumeCapability_AccessMode {
	var vca []*csi.VolumeCapability_AccessMode
	for _, c := range vc {
		glog.Infof("Enabling volume access mode: %v", c.String())
		vca = append(vca, &csi.VolumeCapability_AccessMode{Mode: c})
	}
	n.vcap = vca
	return vca
}

func (n *SeaweedFsDriver) AddControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) {
	var csc []*csi.ControllerServiceCapability

	for _, c := range cl {
		glog.Infof("Enabling controller service capability: %v", c.String())
		csc = append(csc, NewControllerServiceCapability(c))
	}

	n.cscap = csc

	return
}

func (d *SeaweedFsDriver) ValidateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}

	for _, cap := range d.cscap {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Error(codes.InvalidArgument, fmt.Sprintf("%s", c))
}

var _ = filer_pb.FilerClient(&SeaweedFsDriver{})

func (d *SeaweedFsDriver) WithFilerClient(streamingMode bool, fn func(filer_pb.SeaweedFilerClient) error) error {

	return util.Retry("filer grpc", func() error {

		i := d.filerIndex
		n := len(d.filers)
		var err error
		for x := 0; x < n; x++ {

			err = pb.WithGrpcClient(streamingMode, func(grpcConnection *grpc.ClientConn) error {
				client := filer_pb.NewSeaweedFilerClient(grpcConnection)
				return fn(client)
			}, d.filers[i].ToGrpcAddress(), d.grpcDialOption)

			if err != nil {
				glog.V(0).Infof("WithFilerClient %d %v: %v", x, d.filers[i], err)
			} else {
				d.filerIndex = i
				return nil
			}

			i++
			if i >= n {
				i = 0
			}

		}
		return err
	})

}
func (d *SeaweedFsDriver) AdjustedUrl(location *filer_pb.Location) string {
	return location.Url
}
