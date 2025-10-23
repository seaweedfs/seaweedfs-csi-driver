package mountmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/utils"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to the mount service over a Unix domain socket.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient builds a new Client for the given endpoint.
func NewClient(endpoint string) (*Client, error) {
	scheme, address, err := utils.ParseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if scheme != "unix" {
		return nil, fmt.Errorf("unsupported endpoint scheme: %s", scheme)
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", address)
		},
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		baseURL: "http://unix",
	}, nil
}

// Mount mounts a volume using the mount service.
func (c *Client) Mount(req *MountRequest) (*MountResponse, error) {
	var resp MountResponse
	if err := c.doPost("/mount", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Unmount unmounts a volume using the mount service.
func (c *Client) Unmount(req *UnmountRequest) (*UnmountResponse, error) {
	var resp UnmountResponse
	if err := c.doPost("/unmount", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) doPost(path string, payload any, out any) error {
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call mount service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && errResp.Error != "" {
			return errors.New(errResp.Error)
		}
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mount service error: %s (%s)", resp.Status, string(data))
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
