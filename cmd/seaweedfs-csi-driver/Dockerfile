FROM golang:1.24-alpine as builder

RUN apk add git g++

RUN mkdir -p /go/src/github.com/seaweedfs/
RUN git clone https://github.com/seaweedfs/seaweedfs-csi-driver /go/src/github.com/seaweedfs/seaweedfs-csi-driver
RUN cd /go/src/github.com/seaweedfs/seaweedfs-csi-driver && go build -ldflags="-s -w" -o /seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go && go clean -cache -modcache

FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=builder /seaweedfs-csi-driver /

RUN chmod +x /seaweedfs-csi-driver
ENTRYPOINT ["/seaweedfs-csi-driver"]
