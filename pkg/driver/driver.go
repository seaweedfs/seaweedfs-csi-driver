package driver

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/util"
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
	CacheCapacityMB   int
	CacheDir          string
	UidMap            string
	GidMap            string
	signature         int32
	DataCenter        string
	DataLocality      datalocality.DataLocality
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
		signature:      util.RandomInt32(),
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
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
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
	glog.Info("starting")

	controller := NewControllerServer(n)
	node := NewNodeServer(n)

	s := NewNonBlockingGRPCServer()
	s.Start(n.endpoint,
		NewIdentityServer(n),
		controller,
		node)
	s.Wait()

	stopChan := make(chan os.Signal)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-stopChan

	glog.Infof("stopping")

	s.Stop()
	s.Wait()

	glog.Infof("node cleanup")
	node.NodeCleanup()

	glog.Infof("stopped")
}

func (n *SeaweedFsDriver) AddVolumeCapabilityAccessModes(vc []csi.VolumeCapability_AccessMode_Mode) {
	for _, c := range vc {
		glog.Infof("Enabling volume access mode: %v", c.String())
		n.vcap = append(n.vcap, &csi.VolumeCapability_AccessMode{Mode: c})
	}
}

func (n *SeaweedFsDriver) AddControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) {
	for _, c := range cl {
		glog.Infof("Enabling controller service capability: %v", c.String())
		n.cscap = append(n.cscap, NewControllerServiceCapability(c))
	}
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

			err = pb.WithGrpcClient(streamingMode, d.signature, func(grpcConnection *grpc.ClientConn) error {
				client := filer_pb.NewSeaweedFilerClient(grpcConnection)
				return fn(client)
			}, d.filers[i].ToGrpcAddress(), false, d.grpcDialOption)

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
func (d *SeaweedFsDriver) GetDataCenter() string {
	return d.DataCenter
}
