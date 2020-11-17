package driver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/chrislusf/seaweedfs/weed/util/log"
	"golang.org/x/net/context"
)

type IdentityServer struct {
	Driver *SeaweedFsDriver
}

var _ = csi.IdentityServer(&IdentityServer{})

func (ids *IdentityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {

	return &csi.GetPluginInfoResponse{
		Name:          ids.Driver.name,
		VendorVersion: ids.Driver.version,
	}, nil
}

func (ids *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (ids *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	log.Tracef("Using default capabilities")
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			/* // TODO add later
				{
					Type: &csi.PluginCapability_VolumeExpansion_{
						VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
							Type: csi.PluginCapability_VolumeExpansion_ONLINE,
						},
					},
				},
			*/
		},
	}, nil
}
