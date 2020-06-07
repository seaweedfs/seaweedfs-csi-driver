FROM frolvlad/alpine-glibc as builder
RUN apk add git go g++

RUN mkdir -p /go/src/github.com/chrislusf/
RUN git clone https://github.com/chrislusf/seaweedfs /go/src/github.com/chrislusf/seaweedfs
RUN cd /go/src/github.com/chrislusf/seaweedfs/weed && go install

RUN mkdir -p /go/src/github.com/seaweedfs/
RUN git clone https://github.com/seaweedfs/seaweedfs-csi-driver /go/src/github.com/seaweedfs/seaweedfs-csi-driver
RUN cd /go/src/github.com/seaweedfs/seaweedfs-csi-driver && make build

COPY ./_output/seaweedfs-csi-driver /seaweedfs-csi-driver

FROM alpine AS final
LABEL author="Chris Lu"
COPY --from=builder /root/go/bin/weed /usr/bin/
COPY --from=builder /seaweedfs-csi-driver /

RUN chmod +x /seaweedfs-csi-driver
ENTRYPOINT ["/seaweedfs-csi-driver"]
