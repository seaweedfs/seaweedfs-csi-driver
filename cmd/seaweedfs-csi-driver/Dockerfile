FROM golang:1.18-alpine as builder

RUN apk add git g++

RUN mkdir -p /go/src/github.com/chrislusf/
RUN git clone https://github.com/chrislusf/seaweedfs /go/src/github.com/chrislusf/seaweedfs
RUN cd /go/src/github.com/chrislusf/seaweedfs/weed && go install

RUN mkdir -p /go/src/github.com/seaweedfs/
RUN git clone https://github.com/seaweedfs/seaweedfs-csi-driver /go/src/github.com/seaweedfs/seaweedfs-csi-driver
RUN cd /go/src/github.com/seaweedfs/seaweedfs-csi-driver && go build -o /seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go

FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=builder /go/bin/weed /usr/bin/
COPY --from=builder /seaweedfs-csi-driver /

RUN chmod +x /seaweedfs-csi-driver
ENTRYPOINT ["/seaweedfs-csi-driver"]
