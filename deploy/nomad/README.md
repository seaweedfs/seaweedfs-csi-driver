# Example of using seaweedfs with HashiCorp Nomad


## Running seaweedfs cluster

You can skip this part if you have already running seaweedfs.

Assumptions:
 - Running Nomad cluster
 - At least 3 nodes with static IP addresses
 - Enabled memory oversubscription (https://learn.hashicorp.com/tutorials/nomad/memory-oversubscription?in=nomad%2Fadvanced-scheduling)
 - Running PostgreSQL instance for filer

```shell
export NOMAD_ADDR=http://nomad.service.consul:4646

nomad run seaweedfs.hcl
```

Seaweedfs master will be available on http://seaweedfs-master.service.consul:9333/

Seaweedfs filer will be available on http://seaweedfs-filer.service.consul:8888/


## Running CSI

The CSI driver is split into two components that register under the same
`csi_plugin` id (`seaweedfs`):

 - **controller** (`seaweedfs-csi-controller.hcl`) — a single `service` job that
   implements the volume lifecycle RPCs. Nomad calls it for `nomad volume create`
   / `nomad volume delete`. It only talks to the filer.
 - **node** (`seaweedfs-csi.hcl`) — a `system` job that runs on every worker and
   stages/publishes volumes into allocations, using the `seaweedfs-mount`
   sidecar over a shared unix socket.

You need **both** running. With only the node plugin, `nomad volume create`
fails with `plugin has no controller`.

```shell
export NOMAD_ADDR=http://nomad.service.consul:4646

# Start CSI controller (one instance) and node (one per worker)
nomad run seaweedfs-csi-controller.hcl
nomad run seaweedfs-csi.hcl

# Wait until the plugin reports a healthy controller and the expected node count
nomad plugin status seaweedfs

# Create volume
nomad volume create example-seaweedfs-volume.hcl

# Start sample app
nomad run example-seaweedfs-app.hcl
```

> If you only run the node plugin (no controller), you cannot `nomad volume
> create`. Instead, pre-create the bucket/directory in SeaweedFS yourself and
> `nomad volume register example-seaweedfs-volume.hcl` to register the existing
> volume.
