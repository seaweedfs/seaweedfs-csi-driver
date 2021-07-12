FROM frolvlad/alpine-glibc as builder
RUN apk add git go g++ curl jq

RUN curl -sL \
    $(curl -s https://api.github.com/repos/chrislusf/seaweedfs/releases/latest \
    | jq -r '.assets[]|select(.name=="linux_amd64.tar.gz")|.browser_download_url') \
    | tar xzvf -

COPY ../../ /src
RUN cd /src && go build -o /seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go

FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=builder /weed /usr/bin/
COPY --from=builder /seaweedfs-csi-driver /

RUN chmod +x /seaweedfs-csi-driver
ENTRYPOINT ["/seaweedfs-csi-driver"]
