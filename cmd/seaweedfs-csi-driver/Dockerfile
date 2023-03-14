FROM golang:1.20-alpine as builder
# Can be removed at golang 1.20
ENV CGO_CFLAGS=-fno-stack-protector

RUN apk add git g++

RUN mkdir -p /go/src/github.com/seaweedfs/
RUN git clone https://github.com/seaweedfs/seaweedfs /go/src/github.com/seaweedfs/seaweedfs
RUN cd /go/src/github.com/seaweedfs/seaweedfs/weed && go install

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
