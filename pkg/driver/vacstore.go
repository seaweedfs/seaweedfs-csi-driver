package driver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// vacRootDir is the filer subtree holding persisted VolumeAttributesClass
// parameters. It deliberately lives outside /buckets so it is invisible to
// S3 bucket listing and to the volumes' own mounts, and it dies with the
// volume because DeleteVolume removes the per-volume entry.
const vacRootDir = "/.csi/vac"

// vacStore persists the mutable parameters accepted by ControllerModifyVolume
// so a later NodeStage/NodePublish can apply them: Kubernetes routes
// VolumeAttributesClass parameters only through the modify RPC, never through
// the node volume context, so the driver must keep the accepted values itself.
type vacStore interface {
	Read(ctx context.Context, volumeID string) (map[string]string, error)
	Write(ctx context.Context, volumeID string, params map[string]string) error
	Delete(ctx context.Context, volumeID string) error
}

// vacEntry is the JSON document stored per volume.
type vacEntry struct {
	Parameters map[string]string `json:"parameters"`
}

// vacPath returns the filer path for a volume's persisted parameters. The
// volume ID may contain '/' (controller-resolved absolute paths) and must
// become a single path element. base64 RawURLEncoding is injective and
// contains no '/', so distinct volume IDs never collide and path traversal
// is impossible.
func vacPath(volumeID string) string {
	escaped := base64.RawURLEncoding.EncodeToString([]byte(volumeID))
	return vacRootDir + "/" + escaped
}

// filerVacStore keeps the entries on the filer via its HTTP API: PUT/GET/DELETE
// on a small file next to nothing the volumes can see. Content and availability
// follow the filer store (e.g. an SQL-backed filer), not the k8s API, so the
// state survives PV recreation and cluster rebuilds as long as the data does.
type filerVacStore struct {
	filers []pb.ServerAddress
	scheme string
	client *http.Client
}

func newFilerVacStore(filers []pb.ServerAddress) *filerVacStore {
	tlsConfig := vacStoreTLSConfig()
	scheme := "http"
	client := &http.Client{Timeout: 10 * time.Second}
	if tlsConfig != nil {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}
	return &filerVacStore{
		filers: filers,
		scheme: scheme,
		client: client,
	}
}

// vacStoreTLSConfig builds the TLS config for the VAC store's HTTP calls to
// the filer. The scheme is EXPLICIT: it is enabled by [vac] use_tls = true in
// the security config (or env WEED_VAC_USE_TLS=true), never inferred from
// grpc.ca. grpc.ca configures trust for the filer *gRPC* port and says
// nothing about the HTTP port: many deployments keep it plain HTTP, or
// terminate TLS with a public CA the driver cannot validate against the
// SeaweedFS CA. Inferring https from grpc.ca breaks every volume mount on
// such clusters ("server gave HTTP response to HTTPS client").
//
// CA resolution when use_tls is set: vac.ca first, then grpc.ca (some
// deployments serve the filer HTTP port with the internal SeaweedFS TLS).
// With no CA configured, the system trust store is used (public CA behind a
// load balancer).
func vacStoreTLSConfig() *tls.Config {
	v := util.GetViper()
	if !v.GetBool("vac.use_tls") {
		return nil
	}
	caFile := v.GetString("vac.ca")
	if caFile == "" {
		caFile = v.GetString("grpc.ca")
	}
	if caFile == "" {
		glog.Warningf("vac.use_tls is set without vac.ca or grpc.ca; VAC store will trust the system CA store")
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		glog.Warningf("could not read VAC store CA %s: %v; falling back to the system CA store", caFile, err)
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		glog.Warningf("could not parse VAC store CA %s; falling back to the system CA store", caFile)
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func (s *filerVacStore) roundTrip(ctx context.Context, method, volumeID string, body []byte, okCodes ...int) ([]byte, error) {
	var lastErr error
	for _, filer := range s.filers {
		target := s.scheme + "://" + filer.ToHttpAddress() + vacPath(volumeID)
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reqBody)
		if err != nil {
			return nil, err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("filer %s: %w", filer, err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, code := range okCodes {
			if resp.StatusCode == code {
				return respBody, nil
			}
		}
		return nil, fmt.Errorf("filer %s: unexpected status %d", filer, resp.StatusCode)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no filer configured")
	}
	return nil, lastErr
}

func (s *filerVacStore) Read(ctx context.Context, volumeID string) (map[string]string, error) {
	if len(s.filers) == 0 {
		return nil, nil
	}
	body, err := s.roundTrip(ctx, http.MethodGet, volumeID, nil, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		// 404: no persisted parameters for this volume — the normal case.
		return nil, nil
	}
	var entry vacEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, fmt.Errorf("corrupt persisted parameters for %s: %w", volumeID, err)
	}
	return entry.Parameters, nil
}

func (s *filerVacStore) Write(ctx context.Context, volumeID string, params map[string]string) error {
	if len(params) == 0 {
		return nil
	}
	body, err := json.Marshal(vacEntry{Parameters: params})
	if err != nil {
		return err
	}
	_, err = s.roundTrip(ctx, http.MethodPut, volumeID, body, http.StatusOK, http.StatusCreated)
	return err
}

func (s *filerVacStore) Delete(ctx context.Context, volumeID string) error {
	_, err := s.roundTrip(ctx, http.MethodDelete, volumeID, nil, http.StatusOK, http.StatusNotFound, http.StatusNoContent)
	return err
}

// mergePersistedVolumeAttributes overlays the persisted parameters onto the
// volume context. Only mutable keys are ever taken from the store — the write
// side filters to them, and the read side filters again so a corrupted or
// stale entry cannot smuggle structural values (collection, path) into a mount.
func mergePersistedVolumeAttributes(volContext map[string]string, persisted map[string]string) {
	for key, value := range persisted {
		if _, mutable := mutableMountParameters[key]; !mutable {
			glog.Warningf("ignoring non-mutable persisted parameter %q", key)
			continue
		}
		volContext[key] = value
	}
}
