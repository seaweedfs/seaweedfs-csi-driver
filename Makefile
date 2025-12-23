# VERSION=latest make push
.PHONY: build container clean deps push

REGISTRY_NAME=chrislusf
IMAGE_NAME=seaweedfs-csi-driver
VERSION ?= dev
IMAGE_TAG=$(REGISTRY_NAME)/$(IMAGE_NAME):$(VERSION)
COMMIT ?= $(shell git rev-parse --short HEAD)
LDFLAGS ?= -s -w -X github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver.gitCommit=${COMMIT}

deps:
	pushd cmd/seaweedfs-csi-driver; go get -u; popd
	pushd cmd/seaweedfs-csi-driver; go mod tidy; popd
build:
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags '$(LDFLAGS)' -o _output/seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go
container: build
	docker build -t $(IMAGE_TAG) -f cmd/seaweedfs-csi-driver/Dockerfile.dev .
push: container
	docker push $(IMAGE_TAG)
clean:
	go clean -r -x
	-rm -rf _output
