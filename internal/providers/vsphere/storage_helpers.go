package vsphere

import (
	"fmt"
	"strings"
)

// extractPVCNameFromURL extracts the PVC name from a PVC URL
// URL format: pvc://<pvc-name>/<file-path>
// Returns: <pvc-name>
func extractPVCNameFromURL(url string) (string, error) {
	// Remove pvc:// prefix
	url = strings.TrimPrefix(url, "pvc://")

	// Split on first slash to get PVC name
	parts := strings.SplitN(url, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		return "", fmt.Errorf("invalid PVC URL format: expected pvc://<pvc-name>/<file-path>, got: %s", url)
	}

	return parts[0], nil
}

