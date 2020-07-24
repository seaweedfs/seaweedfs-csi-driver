module github.com/seaweedfs/seaweedfs-csi-driver

go 1.14

require (
	github.com/chrislusf/seaweedfs v0.0.0-20200724171345-d40de39e7556
	github.com/container-storage-interface/spec v1.2.0
	github.com/coreos/bbolt v1.3.3 // indirect
	github.com/coreos/etcd v3.3.15+incompatible // indirect
	github.com/coreos/go-systemd v0.0.0-20190719114852-fd7a80b32e1f // indirect
	github.com/golang/glog v0.0.0-20160126235308-23def4e6c14b
	github.com/grpc-ecosystem/go-grpc-middleware v1.0.1-0.20190118093823-f849b5445de4 // indirect
	github.com/mitchellh/go-ps v1.0.0
	golang.org/x/net v0.0.0-20200301022130-244492dfa37a
	golang.org/x/time v0.0.0-20200416051211-89c76fbcd5d1 // indirect
	google.golang.org/grpc v1.29.1
	k8s.io/apimachinery v0.18.2 // indirect
	k8s.io/client-go v0.17.0
	k8s.io/klog v1.0.0
	k8s.io/utils v0.0.0-20200318093247-d1ab8797c558
)

replace (
	go.etcd.io/etcd => go.etcd.io/etcd v0.5.0-alpha.5.0.20200425165423-262c93980547
)
