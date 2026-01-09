# Container Storage Interface (CSI) for SeaweedFS

[Container storage interface](https://kubernetes-csi.github.io/docs/) is an [industry standard](https://github.com/container-storage-interface/spec/blob/master/spec.md) that enables storage vendors to develop a plugin once and have it work across a number of container orchestration systems.

[SeaweedFS](https://github.com/seaweedfs/seaweedfs) is a simple and highly scalable distributed file system, to store and serve billions of files fast!

See [seaweedfs-csi-driver](https://github.com/seaweedfs/seaweedfs-csi-driver) for the source for this CSI plugin.

## Installing

Add the Helm repository:

    helm repo add seaweedfs-csi-driver https://seaweedfs.github.io/seaweedfs-csi-driver/helm
    helm repo update
  
Install the chart. You will need to specify the location of the SeaweedFS filer URL by either running:

    helm install --set seaweedfsFiler=<filerHost:port> my-seaweedfs-csi-driver seaweedfs-csi-driver/seaweedfs-csi-driver
  
Or by configuring a seaweedfs-overrides.yaml file containing (for example):

  # For a SeaweedFS instance running locally under the "seaweed" namespace - adjust for your configuration
  seaweedfsFiler: "seaweedfs-filer.seaweed.svc.cluster.local:8888"

And running:

    helm install my-seaweedfs-csi-driver seaweedfs-csi-driver/seaweedfs-csi-driver -f seaweedfs-overrides.yaml


## Usage

See [Testing](https://github.com/seaweedfs/seaweedfs-csi-driver?tab=readme-ov-file#testing) on some usage examples.
