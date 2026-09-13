package driver

import (
	"slices"
	"testing"
)

func TestBuildMountArgsIncludesInitialCollectionQuota(t *testing.T) {
	mounter := &mountServiceMounter{
		driver:   &SeaweedFsDriver{},
		volumeID: "/buckets/pvc-1234",
		volContext: map[string]string{
			volumeCapacityKey: "5368709120",
		},
	}

	args, err := mounter.buildMountArgs(
		"/staging",
		"/cache",
		"/socket",
		[]string{"filer:8888"},
	)
	if err != nil {
		t.Fatalf("buildMountArgs: %v", err)
	}
	if !slices.Contains(args, "-collectionQuotaMB=5120") {
		t.Fatalf("mount args do not contain initial quota: %v", args)
	}
}

func TestInitialCollectionQuotaMBRoundsUp(t *testing.T) {
	if got, want := initialCollectionQuotaMB("1048577"), "2"; got != want {
		t.Fatalf("initialCollectionQuotaMB = %q, want %q", got, want)
	}
}

func TestInitialCollectionQuotaMBDisablesOneByteSentinel(t *testing.T) {
	if got := initialCollectionQuotaMB("1"); got != "" {
		t.Fatalf("initialCollectionQuotaMB = %q, want empty", got)
	}
}

func TestBuildMountArgsPreservesExtraArgs(t *testing.T) {
	extra := []string{"-debug", "-debug.port=6062", "-readerCacheSizeMB=256", "-memoryLimitMB=768", "-volumeName=name with spaces"}
	mounter := &mountServiceMounter{driver: &SeaweedFsDriver{MountExtraArgs: extra}, volumeID: "/buckets/test", readOnly: true}
	args, err := mounter.buildMountArgs("/staging", "/cache", "/socket", []string{"filer:8888"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args[len(args)-len(extra):], extra) {
		t.Fatalf("extra args changed: %v", args)
	}
	for _, required := range []string{"-dir=/staging", "-localSocket=/socket", "-readOnly"} {
		if !slices.Contains(args, required) {
			t.Errorf("missing %s in %v", required, args)
		}
	}
}

func TestBuildMountArgsRejectsManagedOrPositionalExtraArgs(t *testing.T) {
	for _, arg := range []string{"", "mount", "--", "-", "-debug.port 6062", "---debug", "-dir=/other", "-readOnly=false", "-filer.path=/other", "-collectionQuotaMB=0"} {
		mounter := &mountServiceMounter{driver: &SeaweedFsDriver{MountExtraArgs: []string{arg}}, volumeID: "/buckets/test"}
		if _, err := mounter.buildMountArgs("/staging", "/cache", "/socket", []string{"filer:8888"}); err == nil {
			t.Errorf("accepted %q", arg)
		}
	}
}

func TestBuildMountArgsDataCenterFromVolumeContext(t *testing.T) {
	mounter := &mountServiceMounter{
		driver:   &SeaweedFsDriver{},
		volumeID: "/buckets/pvc-test",
		volContext: map[string]string{
			"dataLocality": "write_preferlocaldc",
			"dataCenter":   "dc1",
		},
	}
	args, err := mounter.buildMountArgs("/staging", "/cache", "/socket", []string{"filer:8888"})
	if err != nil {
		t.Fatalf("buildMountArgs with context dataCenter: %v", err)
	}
	if !slices.Contains(args, "-dataCenter=dc1") {
		t.Fatalf("mount args missing -dataCenter=dc1: %v", args)
	}
}
