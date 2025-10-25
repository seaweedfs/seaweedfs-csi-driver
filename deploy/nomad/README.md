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

```shell
export NOMAD_ADDR=http://nomad.service.consul:4646

# Start CSI plugin
nomad run seaweedfs-csi.hcl

# Create volume
nomad volume create example-seaweedfs-volume.hcl

# Start sample app
nomad run example-seaweedfs-app.hcl
```
