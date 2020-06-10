# Container Storage Interface (CSI) for SeaweedFs
[Container storage interface](https://kubernetes-csi.github.io/docs/) is an [industry standard](https://github.com/container-storage-interface/spec/blob/master/spec.md) that will enable storage vendors to develop a plugin once and have it work across a number of container orchestration systems.

[SeaweedFS](https://github.com/chrislusf/seaweedfs) is a simple and highly scalable distributed file system, to store and serve billions of files fast!

# Deployment (Kubernetes)
#### Prerequisites:
* Already have a working Kubernetes cluster (includes `kubectl`)
* Already have a working SeaweedFS cluster

## Utilize exiting SeaweedFS storage for your Kubernetes cluster (bare metal)

1. Git clone this repository and add your SeaweedFS master IP to `deploy/kubernetes/seaweedfs-csi.yaml` (2 places)

2. Apply the container storage interface for SeaweedFS for your cluster
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

# Developing and contributing

1. Add and commit your changes after forking this repository
2. Run unit tests
```
make test
```
3. After its successfull, create a pull request.


# Miscelleneous
| Description                        | Command       |
| -------------                      |:------------- |
|Docker command for launching seaweedfs|`docker run --cap-add SYS_ADMIN --security-opt apparmor:unconfined -v /dev/fuse:/dev/fuse --privileged -it seaweedfs /bin/bash`

# License
[Apache v2 license](https://www.apache.org/licenses/LICENSE-2.0)

# Code of conduct
Participation in this project is governed by [Kubernetes/CNCF code of conduct](https://github.com/kubernetes/community/blob/master/code-of-conduct.md)