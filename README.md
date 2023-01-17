# Container Storage Interface (CSI) for SeaweedFS

[![Docker Pulls](https://img.shields.io/docker/pulls/chrislusf/seaweedfs-csi-driver.svg?maxAge=4800)](https://hub.docker.com/r/chrislusf/seaweedfs-csi-driver/)

[Container storage interface](https://kubernetes-csi.github.io/docs/) is an [industry standard](https://github.com/container-storage-interface/spec/blob/master/spec.md) that enables storage vendors to develop a plugin once and have it work across a number of container orchestration systems.

[SeaweedFS](https://github.com/seaweedfs/seaweedfs) is a simple and highly scalable distributed file system, to store and serve billions of files fast!

<br>

- [Deployment](#deployment)
  - [Kubernetes (kubectl)](#kubernetes-kubectl)
  - [Kubernetes (helm)](#kubernetes-helm)
- [Update (Safe rollout)](#update-safe-rollout)
- [Testing](#testing)
- [Static and dynamic provisioning](#static-and-dynamic-provisioning)
- [DataLocality](#datalocality)
- [License](#license)
- [Code of conduct](#code-of-conduct)

<br>

# Deployment
## Kubernetes (kubectl)
### Prerequisites:
* Already have a working Kubernetes cluster (includes `kubectl`)
* Already have a working SeaweedFS cluster

### Install

1. Clone this repository 
```sh
git clone https://github.com/seaweedfs/seaweedfs-csi-driver.git
```

2. Adjust your SeaweedFS Filer address via variable SEAWEEDFS_FILER in `deploy/kubernetes/seaweedfs-csi.yaml` (2 places)

3. Apply the container storage interface for SeaweedFS for your cluster.  Use the '-pre-1.17' version for any cluster pre kubernetes version 1.17.

<br>

### To generate an up to date manifest from the helm chart, do:

```
$ helm template seaweedfs ./deploy/helm/seaweedfs-csi-driver > deploy/kubernetes/seaweedfs-csi.yaml
```
Then apply the manifest.
```
$ kubectl apply -f deploy/kubernetes/seaweedfs-csi.yaml
```
4. Ensure all the containers are ready and running
```
$ kubectl get po -n kube-system
```

<br>

### Uninstall

```
$ kubectl delete -f deploy/kubernetes/sample-busybox-pod.yaml
$ kubectl delete -f deploy/kubernetes/sample-seaweedfs-pvc.yaml
$ kubectl delete -f deploy/kubernetes/seaweedfs-csi.yaml
```

<br>

## Kubernetes (helm)

### Install

1. Clone project
```bash
git clone https://github.com/seaweedfs/seaweedfs-csi-driver.git
```
2. Edit `./seaweedfs-csi-driver/deploy/helm/values.yaml` if required and Install
```bash
helm install --set seaweedfsFiler=<filerHost:port> seaweedfs-csi-driver ./seaweedfs-csi-driver/deploy/helm/seaweedfs-csi-driver
```

<br>

### Uninstall

```bash
helm uninstall seaweedfs-csi-driver
```

<br>

# Update (Safe rollout)
Updating seaweed-csi-driver DaemonSet (DS) will break processeses who implement fuse mount:
newly created pods will not remount net device.

For safe update set `node.updateStrategy.type: OnDelete` for manual update. Steps:

  1. delete DS pods on the node where there is no seaweedfs PV
  2. cordon or taint node
  3. evict or delete pods with seaweedfs PV
  4. delete DS pod on node
  5. uncordon or remove taint on node
  6. repeat all steps on [all nodes]
  
<br>

# Testing

1. Create a persistant volume claim for 5GiB with name `seaweedfs-csi-pvc` with storage class `seaweedfs-storage`. The value, 5Gib does not have any significance as for SeaweedFS the whole filesystem is mounted into the container.
```
$ kubectl apply -f deploy/kubernetes/sample-seaweedfs-pvc.yaml
```
2. Verify if the persistant volume claim exists and wait until its the STATUS is `Bound`
```
$ kubectl get pvc
```
3. After its in `Bound` state, create a sample workload mounting that volume
```
$ kubectl apply -f deploy/kubernetes/sample-busybox-pod.yaml
```
4. Verify the storage mount of the busybox pod
```
$ kubectl exec my-csi-app -- df -h
```

<br>

# Static and dynamic provisioning

By default, driver will create separate folder (`/buckets/<volume-id>`) and will use separate collection (`volume-id`)
for each request. Sometimes we need to use exact collection name or change replication options.
It can be done via creating separate storage class with options:

```
kind: StorageClass
apiVersion: storage.k8s.io/v1
metadata:
  name: seaweedfs-special
provisioner: seaweedfs-csi-driver
parameters:
  collection: mycollection
  replication: "011"
  diskType: "ssd"
```

There is another use case when we need to access one folder from different pods with ro/rw access.
In this case we do not need additional StorageClass. We need to create PersistentVolume:

```
apiVersion: v1
kind: PersistentVolume
metadata:
  name: seaweedfs-static
spec:
  accessModes:
  - ReadWriteMany
  capacity:
    storage: 1Gi
  csi:
    driver: seaweedfs-csi-driver
    volumeHandle: dfs-test
    volumeAttributes:
      collection: default
      replication: "011"
      path: /path/to/files
      diskType: "ssd"
    readOnly: true
  persistentVolumeReclaimPolicy: Retain
  volumeMode: Filesystem
```

and bind PersistentVolumeClaim(s) to it:

```
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: seaweedfs-static
spec:
  storageClassName: ""
  volumeName: seaweedfs-static
  accessModes:
  - ReadWriteMany
  resources:
    requests:
      storage: 1Gi
```

<br>

# DataLocality

DataLocality (inspired by [Longhorn](https://longhorn.io/docs/latest/high-availability/data-locality/)) allows instructing the storage-driver which volume-locations will be used or preferred in Pods to read & write.

It auto-sets mount-options based on the location a pod is scheduled in and the locality-option wanted.
The option can be set and overridden in *Driver*, *StorageClass* and *PersistentVolume*.

<br>

## Setup

Change the type of locality

Level               | Location
------------------- | --------
Driver              | Helm: `values.yaml` -> `dataLocality` <br> Or `DaemonSet` -> Container `csi-seaweedfs-plugin` -> args `--dataLocality=`
StorageClass        | `parameter.dataLocality`
PersistentVolume    | `spec.csi.volumeAttributes.dataLocality`

Driver < StorageClass < PersistentVolume

<br>

## Available options

Option                  | Effect
----------------------- | ------
`none`*                 | Changes nothing
`write_preferLocalDc`   | Sets the `DataCenter`-mount-option to the current Node-DataCenter, making writes local and allowing reads to occur wherever read data is stored. [More Details](#`write_preferLocalDc`)

\* Default

<br>

## Requirements

Volume-Servers and the CSI-Driver-Node need to have the locality-option `DataCenter` correctly set (currently only this option is required).

This can be done manually (although quite tedious) or injected by the Container-Orchestration.

<br>

### Automatic injection

**Kubernetes**

Unfortunately Kubernetes doesnt allow grabbing node-labels, which contain well-known region-labels, and setting them as environment-variables.
The DownwardAPI is very limited in that regard. (see [#40610](https://github.com/kubernetes/kubernetes/issues/40610))

Therefore a workaround must be used. [KubeMod](https://github.com/kubemod/kubemod) can be used based on [this comment](https://github.com/kubernetes/kubernetes/issues/40610#issuecomment-1364368282). This of course requires KubeMod to be installed.

You can activate it in the Helm-Chart `values.yaml` -> `node.injectTopologyInfoFromNodeLabel.enabled`.
`node.injectTopologyInfoFromNodeLabel.labels` decides which labels are grabbed from the node.

It is recommended to use [well-known labels](https://kubernetes.io/docs/reference/labels-annotations-taints/#topologykubernetesioregion) to avoid confusion.

<br>

# License
[Apache v2 license](https://www.apache.org/licenses/LICENSE-2.0)

<br>

# Code of conduct
Participation in this project is governed by [Kubernetes/CNCF code of conduct](https://github.com/kubernetes/community/blob/master/code-of-conduct.md)
