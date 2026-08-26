package driver

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func newTopologyNodeServer(nodeID string, keys []string, labels map[string]string, err error) *NodeServer {
	return &NodeServer{
		Driver: &SeaweedFsDriver{nodeID: nodeID, TopologyKeys: keys},
		nodeLabelsFn: func(string) (map[string]string, error) {
			return labels, err
		},
	}
}

func TestNodeGetInfoWithoutTopologyKeys(t *testing.T) {
	ns := newTopologyNodeServer("node-1", nil, map[string]string{"topology.kubernetes.io/zone": "zone-a"}, nil)

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.GetNodeId() != "node-1" {
		t.Errorf("node id = %q, want %q", resp.GetNodeId(), "node-1")
	}
	if resp.GetAccessibleTopology() != nil {
		t.Errorf("accessible topology = %v, want nil", resp.GetAccessibleTopology())
	}
}

func TestNodeGetInfoReportsConfiguredNodeLabels(t *testing.T) {
	ns := newTopologyNodeServer("node-1", []string{"topology.kubernetes.io/zone", "seaweedfs-csi"}, map[string]string{
		"topology.kubernetes.io/zone":   "zone-a",
		"topology.kubernetes.io/region": "region-1",
		"seaweedfs-csi":                 "true",
	}, nil)

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	want := map[string]string{
		"topology.kubernetes.io/zone": "zone-a",
		"seaweedfs-csi":               "true",
	}
	if got := resp.GetAccessibleTopology().GetSegments(); !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %v, want %v", got, want)
	}
}

func TestNodeGetInfoSkipsMissingNodeLabels(t *testing.T) {
	ns := newTopologyNodeServer("node-1", []string{"topology.kubernetes.io/zone", "missing"}, map[string]string{
		"topology.kubernetes.io/zone": "zone-a",
	}, nil)

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	want := map[string]string{"topology.kubernetes.io/zone": "zone-a"}
	if got := resp.GetAccessibleTopology().GetSegments(); !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %v, want %v", got, want)
	}
}

func TestNodeGetInfoFailsWhenNodeLabelsUnavailable(t *testing.T) {
	ns := newTopologyNodeServer("node-1", []string{"topology.kubernetes.io/zone"}, nil, errors.New("api unavailable"))

	if _, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{}); err == nil {
		t.Fatal("expected NodeGetInfo to fail when node labels cannot be read")
	}
}

func hasAccessibilityConstraints(t *testing.T, keys []string) bool {
	t.Helper()
	ids := &IdentityServer{Driver: &SeaweedFsDriver{TopologyKeys: keys}}

	resp, err := ids.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	for _, capability := range resp.GetCapabilities() {
		if capability.GetService().GetType() == csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS {
			return true
		}
	}
	return false
}

func TestGetPluginCapabilitiesWithTopologyKeys(t *testing.T) {
	if !hasAccessibilityConstraints(t, []string{"topology.kubernetes.io/zone"}) {
		t.Error("expected VOLUME_ACCESSIBILITY_CONSTRAINTS to be advertised when topology keys are configured")
	}
}

func TestGetPluginCapabilitiesWithoutTopologyKeys(t *testing.T) {
	if hasAccessibilityConstraints(t, nil) {
		t.Error("expected VOLUME_ACCESSIBILITY_CONSTRAINTS not to be advertised without topology keys")
	}
}

func TestAccessibleTopology(t *testing.T) {
	zoneA := &csi.Topology{Segments: map[string]string{"topology.kubernetes.io/zone": "zone-a"}}
	zoneB := &csi.Topology{Segments: map[string]string{"topology.kubernetes.io/zone": "zone-b"}}

	if got := accessibleTopology(nil); got != nil {
		t.Errorf("accessibleTopology(nil) = %v, want nil", got)
	}

	requisite := &csi.TopologyRequirement{
		Requisite: []*csi.Topology{zoneA, zoneB},
		Preferred: []*csi.Topology{zoneB},
	}
	if got := accessibleTopology(requisite); !reflect.DeepEqual(got, requisite.Requisite) {
		t.Errorf("accessibleTopology = %v, want all requisite segments %v", got, requisite.Requisite)
	}

	preferredOnly := &csi.TopologyRequirement{Preferred: []*csi.Topology{zoneA}}
	if got := accessibleTopology(preferredOnly); !reflect.DeepEqual(got, preferredOnly.Preferred) {
		t.Errorf("accessibleTopology = %v, want %v", got, preferredOnly.Preferred)
	}
}

func TestParseTopologyKeys(t *testing.T) {
	tests := []struct {
		keys string
		want []string
	}{
		{"", nil},
		{" , ", nil},
		{"topology.kubernetes.io/zone", []string{"topology.kubernetes.io/zone"}},
		{" a , b ,,c ", []string{"a", "b", "c"}},
		{"kubernetes.io/hostname,topology.kubernetes.io/zone", []string{"kubernetes.io/hostname", "topology.kubernetes.io/zone"}},
	}
	for _, test := range tests {
		if got := ParseTopologyKeys(test.keys); !reflect.DeepEqual(got, test.want) {
			t.Errorf("ParseTopologyKeys(%q) = %v, want %v", test.keys, got, test.want)
		}
	}
}
