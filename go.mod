module github.com/seaweedfs/seaweedfs-csi-driver

go 1.14

require (
	github.com/chrislusf/seaweedfs v0.0.0-20200529171220-ea93d21641ca
	github.com/container-storage-interface/spec v1.2.0
	github.com/golang/glog v0.0.0-20160126235308-23def4e6c14b
	github.com/mitchellh/go-ps v1.0.0
	golang.org/x/net v0.0.0-20200301022130-244492dfa37a
	golang.org/x/time v0.0.0-20200416051211-89c76fbcd5d1 // indirect
	google.golang.org/grpc v1.28.0
	k8s.io/apimachinery v0.18.2 // indirect
	k8s.io/client-go v0.17.0
	k8s.io/klog v1.0.0
	k8s.io/utils v0.0.0-20200318093247-d1ab8797c558
)
