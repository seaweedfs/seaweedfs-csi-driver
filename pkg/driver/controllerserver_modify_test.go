package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// memVacStore is an in-memory vacStore fake for tests.
type memVacStore struct {
	data      map[string]map[string]string
	writeErr  error
	writtenID string
}

func newMemVacStore() *memVacStore {
	return &memVacStore{data: map[string]map[string]string{}}
}

func (m *memVacStore) Read(_ context.Context, volumeID string) (map[string]string, error) {
	if params, ok := m.data[volumeID]; ok {
		return params, nil
	}
	return nil, nil
}

func (m *memVacStore) Write(_ context.Context, volumeID string, params map[string]string) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.writtenID = volumeID
	cp := make(map[string]string, len(params))
	for k, v := range params {
		cp[k] = v
	}
	m.data[volumeID] = cp
	return nil
}

func (m *memVacStore) Delete(_ context.Context, volumeID string) error {
	delete(m.data, volumeID)
	return nil
}

func modifyTestDriver() (*ControllerServer, *memVacStore) {
	driver := &SeaweedFsDriver{name: "test"}
	driver.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	})
	store := newMemVacStore()
	cs := &ControllerServer{Driver: driver, vacStore: store}
	// Default to a PV carrying no static mutable attributes, so tests do
	// not depend on an in-cluster k8s client. Tests that exercise the
	// merged-set validation override pvAttributesFn.
	cs.pvAttributesFn = func(_ context.Context, _ string) (map[string]string, error) {
		return map[string]string{}, nil
	}
	return cs, store
}

func hasModifyVolumeCapability(cs *ControllerServer) bool {
	resp, err := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		return false
	}
	for _, cap := range resp.GetCapabilities() {
		if cap.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_MODIFY_VOLUME {
			return true
		}
	}
	return false
}

func TestControllerModifyVolume_CapabilityAdvertised(t *testing.T) {
	cs, _ := modifyTestDriver()
	if !hasModifyVolumeCapability(cs) {
		t.Fatal("MODIFY_VOLUME capability not advertised")
	}
}

func TestControllerModifyVolume_AcceptsMutableParameters(t *testing.T) {
	cs, _ := modifyTestDriver()
	resp, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "pvc-abc",
		MutableParameters: map[string]string{
			"diskType":     "ssd",
			"replication":  "000",
			"dataLocality": "none",
		},
	})
	if err != nil {
		t.Fatalf("modify with mutable parameters failed: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
}

func TestControllerModifyVolume_RejectsUnknownParameter(t *testing.T) {
	cs, _ := modifyTestDriver()
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"collection": "other"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if !strings.Contains(err.Error(), "collection") {
		t.Errorf("error should name the rejected parameter: %v", err)
	}
}

func TestControllerModifyVolume_RejectsIgnoredQuotaParameter(t *testing.T) {
	cs, _ := modifyTestDriver()
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"collectionQuotaMB": "1024"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestControllerModifyVolume_RejectsInvalidValues(t *testing.T) {
	cs, _ := modifyTestDriver()
	for key, value := range map[string]string{
		"dataLocality":      "bogus",
		"concurrentReaders": "not-a-number",
		"cacheCapacityMB":   "-",
		"chunkSizeLimitMB":  "1.5",
	} {
		_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
			VolumeId:          "pvc-abc",
			MutableParameters: map[string]string{key: value},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s=%q: code = %v, want InvalidArgument", key, value, status.Code(err))
		}
	}
}

func TestControllerModifyVolume_AcceptsValidValues(t *testing.T) {
	cs, _ := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "pvc-abc",
		MutableParameters: map[string]string{
			"dataLocality":      "none",
			"concurrentReaders": "64",
			"cacheCapacityMB":   "0",
		},
	}); err != nil {
		t.Fatalf("valid values rejected: %v", err)
	}
}

func TestControllerModifyVolume_RejectsStructuralParameter(t *testing.T) {
	cs, _ := modifyTestDriver()
	for _, key := range []string{"parentDir", "path", "volumeName", "filer.path"} {
		_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
			VolumeId:          "pvc-abc",
			MutableParameters: map[string]string{key: "whatever"},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("structural key %q: code = %v, want InvalidArgument", key, status.Code(err))
		}
	}
}

func TestControllerModifyVolume_RequiresVolumeIdAndParameters(t *testing.T) {
	cs, _ := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{VolumeId: "pvc-abc"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty parameters: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{MutableParameters: map[string]string{"diskType": "ssd"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty volume id: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestControllerModifyVolume_MounterConsumesModifiedParameters(t *testing.T) {
	cs, _ := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"diskType": "ssd"},
	}); err != nil {
		t.Fatalf("modify failed: %v", err)
	}

	d := &SeaweedFsDriver{name: "test", CacheCapacityMB: 0, CacheMetaTtlSec: 60}
	m := &mountServiceMounter{driver: d, volContext: map[string]string{
		"diskType":   "ssd",
		"parentDir":  "/buckets",
		"volumeName": "pvc-abc",
	}, volumeID: "/buckets/pvc-abc", readOnly: false}
	args, err := m.buildMountArgs("/tmp/target", "/var/cache/x", "/var/lib/seaweedfs-mount.sock", []string{"127.0.0.1:8888"})
	if err != nil {
		t.Fatalf("buildMountArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-disk=ssd") {
		t.Errorf("mount args missing -disk=ssd: %s", joined)
	}
}

func createVolumeReq(name string, params, mutable map[string]string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:              name,
		Parameters:        params,
		MutableParameters: mutable,
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	}
}

func TestCreateVolume_MergesMutableParametersIntoContext(t *testing.T) {
	cs, _ := modifyTestDriver()
	resp, err := cs.CreateVolume(context.Background(), createVolumeReq("pvc-abc",
		map[string]string{"diskType": "hdd"},
		map[string]string{"diskType": "ssd", "replication": "001"},
	))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vc := resp.GetVolume().GetVolumeContext()
	if vc["diskType"] != "ssd" {
		t.Errorf("mutable diskType should take precedence: got %q", vc["diskType"])
	}
	if vc["replication"] != "001" {
		t.Errorf("mutable replication missing from context: got %q", vc["replication"])
	}
	if vc["parentDir"] != "/buckets" || vc["volumeName"] != "pvc-abc" {
		t.Errorf("structural keys clobbered: parentDir=%q volumeName=%q", vc["parentDir"], vc["volumeName"])
	}
}

func TestCreateVolume_RejectsInvalidMutableParameters(t *testing.T) {
	cs, _ := modifyTestDriver()
	for key, value := range map[string]string{
		"dataLocality":      "bogus",
		"concurrentReaders": "not-a-number",
	} {
		_, err := cs.CreateVolume(context.Background(), createVolumeReq("pvc-abc", nil, map[string]string{key: value}))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s=%q: code = %v, want InvalidArgument", key, value, status.Code(err))
		}
	}
}

func TestCreateVolume_RejectsStructuralMutableParameters(t *testing.T) {
	cs, _ := modifyTestDriver()
	for _, key := range []string{"parentDir", "path", "volumeName", "collection"} {
		_, err := cs.CreateVolume(context.Background(), createVolumeReq("pvc-abc", nil, map[string]string{key: "whatever"}))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("structural key %q: code = %v, want InvalidArgument", key, status.Code(err))
		}
	}
}

func TestControllerModifyVolume_PersistsAcceptedParameters(t *testing.T) {
	cs, store := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"diskType": "ssd", "concurrentReaders": "64"},
	}); err != nil {
		t.Fatalf("modify failed: %v", err)
	}
	if store.writtenID != "pvc-abc" {
		t.Fatalf("expected write for pvc-abc, got %q", store.writtenID)
	}
	params, err := store.Read(context.Background(), "pvc-abc")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if params["diskType"] != "ssd" || params["concurrentReaders"] != "64" {
		t.Fatalf("persisted parameters lost: %v", params)
	}
}

func TestControllerModifyVolume_WriteFailureFailsModify(t *testing.T) {
	cs, store := modifyTestDriver()
	store.writeErr = errors.New("filer down")
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"diskType": "ssd"},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

func TestControllerModifyVolume_WritebackTunables(t *testing.T) {
	cs, store := modifyTestDriver()
	if _, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "pvc-abc",
		MutableParameters: map[string]string{
			"writebackCache":       "true",
			"metadataFlushSeconds": "300",
		},
	}); err != nil {
		t.Fatalf("writeback tunables rejected: %v", err)
	}
	params, err := store.Read(context.Background(), "pvc-abc")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if params["writebackCache"] != "true" || params["metadataFlushSeconds"] != "300" {
		t.Fatalf("persisted parameters lost: %v", params)
	}
}

func TestControllerModifyVolume_RejectsWritebackDlmCombo(t *testing.T) {
	cs, _ := modifyTestDriver()
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: "pvc-abc",
		MutableParameters: map[string]string{
			"writebackCache": "true",
			"dlm":            "true",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (mutually exclusive)", status.Code(err))
	}
}

func TestControllerModifyVolume_RejectsInvalidWritebackTunables(t *testing.T) {
	cs, _ := modifyTestDriver()
	for key, value := range map[string]string{
		"writebackCache":       "yes",
		"dlm":                  "maybe",
		"metadataFlushSeconds": "ten",
	} {
		_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
			VolumeId:          "pvc-abc",
			MutableParameters: map[string]string{key: value},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s=%q: code = %v, want InvalidArgument", key, value, status.Code(err))
		}
	}
}

func TestMounterConsumesWritebackTunables(t *testing.T) {
	d := &SeaweedFsDriver{name: "test", CacheCapacityMB: 0, CacheMetaTtlSec: 60}
	m := &mountServiceMounter{driver: d, volContext: map[string]string{
		"writebackCache":       "true",
		"metadataFlushSeconds": "300",
		"dlm":                  "true",
	}, volumeID: "/buckets/pvc-abc", readOnly: false}
	args, err := m.buildMountArgs("/tmp/target", "/var/cache/x", "/var/lib/seaweedfs-mount.sock", []string{"127.0.0.1:8888"})
	if err != nil {
		t.Fatalf("buildMountArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-writebackCache=true", "-metadataFlushSeconds=300", "-dlm=true"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mount args missing %s: %s", want, joined)
		}
	}
}

func TestControllerModifyVolume_ValidatesMergedPvAttributes(t *testing.T) {
	// PV carries static dlm=true; the class only flips writebackCache — the
	// combo is invisible to the mutable parameters alone, so the merged
	// effective set must be validated before persisting.
	cs, store := modifyTestDriver()
	cs.pvAttributesFn = func(_ context.Context, volumeID string) (map[string]string, error) {
		return map[string]string{"dlm": "true"}, nil
	}
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"writebackCache": "true"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument for the merged combo", status.Code(err))
	}
	if len(store.data) != 0 {
		t.Fatalf("rejected modification must not be persisted: %v", store.data)
	}
}

func TestControllerModifyVolume_FailsWhenPvAttributesUnreadable(t *testing.T) {
	// The PV lookup is the only source of the static half of the effective
	// set. When it fails the combo check is meaningless, so the modify must
	// fail with a retryable error and must not persist the unvalidated
	// parameters — otherwise a transient API outage could record a
	// combination that every later stage rejects.
	cs, store := modifyTestDriver()
	cs.pvAttributesFn = func(_ context.Context, volumeID string) (map[string]string, error) {
		return nil, errors.New("api server unreachable")
	}
	_, err := cs.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          "pvc-abc",
		MutableParameters: map[string]string{"concurrentReaders": "32"},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal (retryable)", status.Code(err))
	}
	if len(store.data) != 0 {
		t.Fatalf("failed modify must not be persisted: %v", store.data)
	}
}
