package mountmanager

import (
	"fmt"
	"strings"
)

// ParseEndpoint splits an endpoint string like "unix:///path" into scheme and address.
func ParseEndpoint(endpoint string) (scheme, address string, err error) {
	parts := strings.SplitN(endpoint, "://", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("invalid endpoint: %s", endpoint)
	}
	return strings.ToLower(parts[0]), parts[1], nil
}
