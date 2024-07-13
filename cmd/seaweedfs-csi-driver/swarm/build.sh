#!/bin/bash
VERSION=${1:-latest}
ARCH=${2:-linux/amd64}
PLUGIN_NAME=${3:-swarm-csi-swaweedfs}
PLUGIN_TAG=${4:-v1.2.0}
PREFIX=${5:-gradlon}

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
