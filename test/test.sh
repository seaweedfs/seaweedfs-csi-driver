endpoint='unix:///tmp/csi.sock'

# Install csi-sanity
cd ..
git clone https://github.com/kubernetes-csi/csi-test.git -b v5.0.0
cd csi-test
make install

## Start Local weed server with filer
weed server -dir=/tmp/seaweedfs/data -s3 -volume.max=100 -volume.port=8090

## Run CSI Driver
cd ../seaweedfs-csi-driver
make build
./_output/seaweedfs-csi-driver -endpoint="$endpoint" -alsologtostderr -v=5 -filer=localhost:8888

# Run CSI Sanity Tests
../csi-test/cmd/csi-sanity/csi-sanity\
    --ginkgo.v\
    --csi.testvolumeparameters="$(pwd)/test/sanity/params.yaml"\
    --csi.endpoint="$endpoint"\
    --ginkgo.skip="should not fail when requesting to create a volume with already existing name and same capacity|should fail when requesting to create a volume with already existing name and different capacity|should work|should fail when the requested volume does not exist|should return appropriate capabilities"