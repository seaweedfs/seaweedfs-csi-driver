#!/bin/sh

if [ -z "$FILER" ]; then
	echo "FILER is not set!"
	exit 1
fi

NODE_ID=$(cat /node_hostname)
C_WRITER=${C_WRITER:-32}

CMD="/seaweedfs-csi-driver --filer=$FILER --nodeid=${NODE_ID} --endpoint=unix://run/docker/plugins/seaweed.sock --concurrentWriters=${C_WRITER} --dataCenter=${DATACENTER} --dataLocality=none --logtostderr --map.uid=${UID_MAP} --map.gid=${GID_MAP} --cacheCapacityMB=${CACHE_SIZE} --cacheDir=/tmp/seaweedFS/docker-csi" 

exec $CMD
