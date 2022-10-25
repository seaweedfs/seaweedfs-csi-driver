# Container Storage Interface (CSI) for SeaweedFS

[![Docker Pulls](https://img.shields.io/docker/pulls/chrislusf/seaweedfs-csi-driver.svg?maxAge=4800)](https://hub.docker.com/r/chrislusf/seaweedfs-csi-driver/)

[Container storage interface](https://kubernetes-csi.github.io/docs/) is an [industry standard](https://github.com/container-storage-interface/spec/blob/master/spec.md) that enables storage vendors to develop a plugin once and have it work across a number of container orchestration systems.

[SeaweedFS](https://github.com/seaweedfs/seaweedfs) is a simple and highly scalable distributed file system, to store and serve billions of files fast!

# Deployment (Kubernetes)
#### Prerequisites:
* Already have a working Kubernetes cluster (includes `kubectl`)
* Already have a working SeaweedFS cluster

## Utilize existing SeaweedFS storage for your Kubernetes cluster (bare metal)

1. Git clone this repository and adjust your SeaweedFS Filer address via variable SEAWEEDFS_FILER in `deploy/kubernetes/seaweedfs-csi.yaml` (2 places)

2. Apply the container storage interface for SeaweedFS for your cluster.  Use the '-pre-1.17' version for any cluster pre kubernetes version 1.17.  To generate an up to date manifest from the helm chart, do:

```
$ helm template seaweedfs ./deploy/helm/seaweedfs-csi-driver > deploy/kubernetes/seaweedfs-csi.yaml
```
Check the kubelet root directory ,Execute the following command
```
ps -ef | grep kubelet | grep root-dir
```
If the result returned from the previous check command is not empty, the root directory (eg:--root-dir=/data/k8s/kubelet/data) representing the kubelet is not the default value (/var/lib/kubelet), so you need to update the kubelet root-dir in the CSI driven deployment file and deploy:
```
sed 's+/var/lib/kubelet+/data/k8s/kubelet/data+g'  deploy/kubernetes/seaweedfs-csi.yaml | kubectl apply -f - 
```
If the result returned by the previous check command is null, you can directly deploy it without modifying the configuration:
```
$ kubectl apply -f deploy/kubernetes/seaweedfs-csi.yaml
```
3. Ensure all the containers are ready and running
```
$ kubectl get po -n kube-system
```
4. Testing: Create a persistant volume claim for 5GiB with name `seaweedfs-csi-pvc` with storage class `seaweedfs-storage`. The value, 5Gib does not have any significance as for SeaweedFS the whole filesystem is mounted into the container.
```
$ kubectl apply -f deploy/kubernetes/sample-seaweedfs-pvc.yaml
```
5. Verify if the persistant volume claim exists and wait until its the STATUS is `Bound`
```
$ kubectl get pvc
```
6. After its in `Bound` state, create a sample workload mounting that volume
```
$ kubectl apply -f deploy/kubernetes/sample-busybox-pod.yaml
```
7. Verify the storage mount of the busybox pod
```
$ kubectl exec my-csi-app -- df -h
```
8. Clean up
```
$ kubectl delete -f deploy/kubernetes/sample-busybox-pod.yaml
$ kubectl delete -f deploy/kubernetes/sample-seaweedfs-pvc.yaml
$ kubectl delete -f deploy/kubernetes/seaweedfs-csi.yaml
```

# Deployment by helm chart

1. Clone project
```bash
git clone https://github.com/seaweedfs/seaweedfs-csi-driver.git
```
2. Install
```bash
helm install --set seaweedfsFiler=<filerHost:port> seaweedfs-csi-driver ./seaweedfs-csi-driver/deploy/helm/seaweedfs-csi-driver
```

3. Clean up
```bash
helm uninstall seaweedfs-csi-driver
```

# Safe rollout update
Updating seaweed-csi-driver DaemonSet (DS) will break processeses who implement fuse mount:
newly created pods will not remount net device.

For safe update set `node.updateStrategy.type: OnDelete` for manual update. Steps:

  1. delete DS pods on the node where there is no seaweedfs PV
  2. cordon or taint node
  3. evict or delete pods with seaweedfs PV
  4. delete DS pod on node
  5. uncordon or remove taint on node
  6. repeat all steps on [all nodes]

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

# License
[Apache v2 license](https://www.apache.org/licenses/LICENSE-2.0)

# Code of conduct
Participation in this project is governed by [Kubernetes/CNCF code of conduct](https://github.com/kubernetes/community/blob/master/code-of-conduct.md)
