# SeaweedFS CSI-Driver Docker Plugin

This Docker plugin integrates the SeaweedFS CSI-Driver with Docker. It allows you to use SeaweedFS as a volume driver in Docker environments.

## Environment Variables

| Variable              | Description                                                                                           |
|-----------------------|-------------------------------------------------------------------------------------------------------|
| `FILER`               | Filer endpoint(s), format: `<IP1>:<PORT>,<IP2>:<PORT2>`                                                |
| `CACHE_SIZE`          | The size of the cache to use in MB. Default: 256MB                                                     |
| `CACHE_DIR`           | The cache directory.                                                                                   |
| `C_WRITER`            | Limit concurrent goroutine writers if not 0. Default 32                                                |
| `DATACENTER`          | Data center this node is running in (locality-definition). Default: `DefaultDataCenter`                |
| `UID_MAP`             | Map local UID to UID on filer, comma-separated `<local_uid>:<filer_uid>`                               |
| `GID_MAP`             | Map local GID to GID on filer, comma-separated `<local_gid>:<filer_gid>`                               |

## Mounts

| Mount Destination   | Source Path     | Description                  |
|---------------------|-----------------|------------------------------|
| `/node_hostname`    | `/etc/hostname` | Used to get the nodename     |
| `/tmp`              | `/tmp`          | Used for caching             |

## Usag

```bash
docker plugin install --disable --alias seaweedfs-csi:swarm --grant-all-permissions gradlon/swarm-csi-swaweedfs:v1.2.0
docker plugin set seaweedfs-csi:swarm FILER=<IP>:8888,<IP>:8888
docker plugin set seaweedfs-csi:swarm CACHE_SIZE=512
docker plugin enable seaweedfs-csi:swarm
docker volume create --driver seaweedfs-csi:swarm --availability active --scope single --sharing none  --type mount --opt path="/docker/volumes/teste1" test-volume

docker volume create \
  --driver seaweedfs-csi:swarm \
  --availability active \
  --scope multi \
  --sharing all \
  --type mount \
  testVolume

  docker service create   --name testService   --mount type=cluster,src=testVolume,dst=/usr/share/nginx/html   --publish 2080:80   nginx
```

## Build Guide

Follow these steps to build the SeaweedFS CSI-Driver Docker plugin:

```bash
#!/bin/bash
USAGE="Usage: ./build.sh [PREFIX] [VERSION] [TAG] [PLUGIN_NAME] [ARCH]"

if [[ "$1" == "-h" ]]; then
  echo "$USAGE"
  exit 0
fi

VERSION=${2:-latest}
ARCH=${5:-linux/amd64}
PLUGIN_NAME=${4:-swarm-csi-swaweedfs}
PLUGIN_TAG=${3:-v1.2.0}
PREFIX=${1:-gradlon}

docker build --platform ${ARCH} --build-arg BASE_IMAGE=chrislusf/seaweedfs-csi-driver:${VERSION} --build-arg ARCH=$ARCH -t seawadd-csi_tmp_img .
mkdir -p ./plugin/rootfs
cp config.json ./plugin/
docker container create --name seawadd-csi_tmp seawadd-csi_tmp_img 
docker container export seawadd-csi_tmp | tar -x -C ./plugin/rootfs
docker container rm -vf seawadd-csi_tmp 
docker image rm seawadd-csi_tmp_img 

docker plugin disable gradlon/swarm-csi-swaweedfs:v1.2.0
docker plugin rm ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG} 2> /dev/null || true
docker plugin create ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG} ./plugin
docker plugin push ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG}
rm -rf ./plugin/
```
