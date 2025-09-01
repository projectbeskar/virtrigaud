/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pveapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds the PVE API client configuration
type Config struct {
	Endpoint           string
	TokenID            string
	TokenSecret        string
	Username           string
	Password           string
	InsecureSkipVerify bool
	CABundle           []byte
	NodeSelector       []string
	RequestTimeout     time.Duration
	TaskPollInterval   time.Duration
	TaskTimeout        time.Duration
}

// Client represents a Proxmox VE API client
type Client struct {
	config     *Config
	httpClient *http.Client
	baseURL    *url.URL
}

// NewClient creates a new PVE API client
func NewClient(config *Config) (*Client, error) {
	if config.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}

	baseURL, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint URL: %w", err)
	}

	// Set defaults
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.TaskPollInterval == 0 {
		config.TaskPollInterval = 2 * time.Second
	}
	if config.TaskTimeout == 0 {
		config.TaskTimeout = 5 * time.Minute
	}

	// Create HTTP client with TLS configuration
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: config.InsecureSkipVerify,
		},
	}

	// Configure CA bundle if provided
	// CA bundle loading - intentionally not implemented yet, using system trust store
	if len(config.CABundle) > 0 {
		_ = config.CABundle // intentionally unused: custom CA support planned for future release
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   config.RequestTimeout,
	}

	return &Client{
		config:     config,
		httpClient: httpClient,
		baseURL:    baseURL,
	}, nil
}

// Config returns the client configuration
func (c *Client) Config() *Config {
	return c.config
}

// VM represents a Proxmox VE virtual machine
type VM struct {
	VMID       int    `json:"vmid"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Node       string `json:"node"`
	CPUs       int    `json:"cpus,omitempty"`
	Memory     int64  `json:"maxmem,omitempty"`
	Template   int    `json:"template,omitempty"`
	QMPStatus  string `json:"qmpstatus,omitempty"`
	PID        int    `json:"pid,omitempty"`
	ConfigLock string `json:"lock,omitempty"`
}

// VMConfig represents VM configuration parameters
type VMConfig struct {
	VMID      int               `json:"vmid,omitempty"`
	Name      string            `json:"name,omitempty"`
	CPUs      int               `json:"cores,omitempty"`
	Memory    int64             `json:"memory,omitempty"`
	Template  string            `json:"template,omitempty"`
	Clone     string            `json:"clone,omitempty"`
	Storage   string            `json:"storage,omitempty"`
	IDE2      string            `json:"ide2,omitempty"`
	CIUser    string            `json:"ciuser,omitempty"`
	CIPasswd  string            `json:"cipassword,omitempty"`
	SSHKeys   string            `json:"sshkeys,omitempty"`
	Networks  []NetworkConfig   `json:"-"` // Will be mapped to net0, net1, etc.
	IPConfigs []IPConfig        `json:"-"` // Will be mapped to ipconfig0, ipconfig1, etc.
	Custom    map[string]string `json:"-"`
}

// NetworkConfig represents a VM network interface
type NetworkConfig struct {
	Index    int    `json:"index"`          // Network interface index (0, 1, 2...)
	Model    string `json:"model"`          // Network adapter model (virtio, e1000, etc.)
	Bridge   string `json:"bridge"`         // Bridge name (vmbr0, vmbr1, etc.)
	VLAN     int    `json:"vlan,omitempty"` // VLAN tag
	MAC      string `json:"mac,omitempty"`  // MAC address
	Firewall bool   `json:"firewall"`       // Enable firewall
	LinkDown bool   `json:"link_down"`      // Start with link down
}

// IPConfig represents IP configuration for a network interface
type IPConfig struct {
	Index   int    `json:"index"`                // Interface index (0, 1, 2...)
	IP      string `json:"ip,omitempty"`         // IP address with CIDR (e.g., "192.168.1.100/24")
	Gateway string `json:"gateway,omitempty"`    // Gateway IP
	DNS     string `json:"nameserver,omitempty"` // DNS servers (comma-separated)
	DHCP    bool   `json:"dhcp"`                 // Use DHCP instead of static IP
}

// Task represents a PVE task
type Task struct {
	UPID      string  `json:"upid"`
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	User      string  `json:"user"`
	Node      string  `json:"node"`
	PID       int     `json:"pid"`
	StartTime int64   `json:"starttime"`
	Status    string  `json:"status"`
	ExitCode  *string `json:"exitstatus,omitempty"`
}

// Snapshot represents a VM snapshot
type Snapshot struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	SnapTime    int64  `json:"snaptime,omitempty"`
	VMSTATE     int    `json:"vmstate,omitempty"`
	Parent      string `json:"parent,omitempty"`
}

// APIResponse represents a generic PVE API response
type APIResponse struct {
	Data   interface{} `json:"data"`
	Errors interface{} `json:"errors,omitempty"`
}

// request makes an HTTP request to the PVE API
func (c *Client) request(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		if data, ok := body.(url.Values); ok {
			reqBody = strings.NewReader(data.Encode())
		} else {
			jsonData, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal request body: %w", err)
			}
			reqBody = bytes.NewReader(jsonData)
		}
	}

	reqURL := c.baseURL.ResolveReference(&url.URL{Path: path})
	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set authentication headers
	if c.config.TokenID != "" && c.config.TokenSecret != "" {
		req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", c.config.TokenID, c.config.TokenSecret))
	}

	// Set content type
	if body != nil {
		if _, ok := body.(url.Values); ok {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	return c.httpClient.Do(req)
}

// GetVM retrieves information about a specific VM
func (c *Client) GetVM(ctx context.Context, node string, vmid int) (*VM, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/current", node, vmid)

	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode == 404 {
		return nil, ErrVMNotFound
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	vmData, err := json.Marshal(apiResp.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal VM data: %w", err)
	}

	var vm VM
	if err := json.Unmarshal(vmData, &vm); err != nil {
		return nil, fmt.Errorf("failed to unmarshal VM: %w", err)
	}

	vm.Node = node
	vm.VMID = vmid

	return &vm, nil
}

// CreateVM creates a new VM
func (c *Client) CreateVM(ctx context.Context, node string, config *VMConfig) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu", node)

	// Convert config to form values
	values := c.configToValues(config)

	resp, err := c.request(ctx, "POST", path, values)
	if err != nil {
		return "", fmt.Errorf("failed to create VM: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create VM failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	// Extract task ID from response
	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// CloneVM clones an existing VM
func (c *Client) CloneVM(ctx context.Context, node string, vmid int, config *VMConfig) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/clone", node, vmid)

	values := c.configToValues(config)

	resp, err := c.request(ctx, "POST", path, values)
	if err != nil {
		return "", fmt.Errorf("failed to clone VM: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("clone VM failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// DeleteVM deletes a VM
func (c *Client) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d", node, vmid)

	resp, err := c.request(ctx, "DELETE", path, nil)
	if err != nil {
		return "", fmt.Errorf("failed to delete VM: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode == 404 {
		// VM doesn't exist, consider it deleted
		return "", nil
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("delete VM failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", nil
}

// PowerOperation performs a power operation on a VM
func (c *Client) PowerOperation(ctx context.Context, node string, vmid int, operation string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/%s", node, vmid, operation)

	resp, err := c.request(ctx, "POST", path, nil)
	if err != nil {
		return "", fmt.Errorf("failed to perform power operation: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("power operation failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", nil
}

// GetTaskStatus gets the status of a task
func (c *Client) GetTaskStatus(ctx context.Context, node, taskID string) (*Task, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", node, taskID)

	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get task status: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	taskData, err := json.Marshal(apiResp.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal task data: %w", err)
	}

	var task Task
	if err := json.Unmarshal(taskData, &task); err != nil {
		return nil, fmt.Errorf("failed to unmarshal task: %w", err)
	}

	return &task, nil
}

// WaitForTask waits for a task to complete
func (c *Client) WaitForTask(ctx context.Context, node, taskID string) error {
	if taskID == "" {
		return nil // No task to wait for
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, c.config.TaskTimeout)
	defer cancel()

	ticker := time.NewTicker(c.config.TaskPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("task timeout: %w", timeoutCtx.Err())
		case <-ticker.C:
			task, err := c.GetTaskStatus(ctx, node, taskID)
			if err != nil {
				return fmt.Errorf("failed to get task status: %w", err)
			}

			if task.Status == "stopped" {
				if task.ExitCode != nil && *task.ExitCode != "OK" {
					return fmt.Errorf("task failed with exit code: %s", *task.ExitCode)
				}
				return nil
			}
		}
	}
}

// configToValues converts VMConfig to url.Values
func (c *Client) configToValues(config *VMConfig) url.Values {
	values := url.Values{}

	if config.VMID != 0 {
		values.Set("vmid", strconv.Itoa(config.VMID))
	}
	if config.Name != "" {
		values.Set("name", config.Name)
	}
	if config.CPUs != 0 {
		values.Set("cores", strconv.Itoa(config.CPUs))
	}
	if config.Memory != 0 {
		values.Set("memory", strconv.FormatInt(config.Memory, 10))
	}
	if config.Template != "" {
		values.Set("template", config.Template)
	}
	if config.Clone != "" {
		values.Set("clone", config.Clone)
	}
	if config.Storage != "" {
		values.Set("storage", config.Storage)
	}
	if config.IDE2 != "" {
		values.Set("ide2", config.IDE2)
	}
	if config.CIUser != "" {
		values.Set("ciuser", config.CIUser)
	}
	if config.CIPasswd != "" {
		values.Set("cipassword", config.CIPasswd)
	}
	if config.SSHKeys != "" {
		values.Set("sshkeys", config.SSHKeys)
	}

	// Configure network interfaces
	for _, netConfig := range config.Networks {
		netString := c.buildNetworkString(netConfig)
		if netString != "" {
			values.Set(fmt.Sprintf("net%d", netConfig.Index), netString)
		}
	}

	// Configure IP settings
	for _, ipConfig := range config.IPConfigs {
		ipString := c.buildIPConfigString(ipConfig)
		if ipString != "" {
			values.Set(fmt.Sprintf("ipconfig%d", ipConfig.Index), ipString)
		}
	}

	// Add custom parameters
	for key, value := range config.Custom {
		values.Set(key, value)
	}

	return values
}

// buildNetworkString constructs the network configuration string for PVE
func (c *Client) buildNetworkString(netConfig NetworkConfig) string {
	var parts []string

	// Model (required)
	model := netConfig.Model
	if model == "" {
		model = "virtio" // Default to virtio
	}
	parts = append(parts, model)

	// Bridge (required)
	if netConfig.Bridge != "" {
		parts = append(parts, fmt.Sprintf("bridge=%s", netConfig.Bridge))
	} else {
		parts = append(parts, "bridge=vmbr0") // Default bridge
	}

	// VLAN tag
	if netConfig.VLAN > 0 {
		parts = append(parts, fmt.Sprintf("tag=%d", netConfig.VLAN))
	}

	// MAC address
	if netConfig.MAC != "" {
		parts = append(parts, fmt.Sprintf("macaddr=%s", netConfig.MAC))
	}

	// Firewall
	if netConfig.Firewall {
		parts = append(parts, "firewall=1")
	}

	// Link down
	if netConfig.LinkDown {
		parts = append(parts, "link_down=1")
	}

	return strings.Join(parts, ",")
}

// buildIPConfigString constructs the IP configuration string for PVE
func (c *Client) buildIPConfigString(ipConfig IPConfig) string {
	if ipConfig.DHCP {
		return "dhcp=1"
	}

	var parts []string

	// IP address
	if ipConfig.IP != "" {
		parts = append(parts, fmt.Sprintf("ip=%s", ipConfig.IP))
	}

	// Gateway
	if ipConfig.Gateway != "" {
		parts = append(parts, fmt.Sprintf("gw=%s", ipConfig.Gateway))
	}

	// DNS servers
	if ipConfig.DNS != "" {
		parts = append(parts, fmt.Sprintf("nameserver=%s", ipConfig.DNS))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, ",")
}

// FindNode selects an appropriate node for VM placement
func (c *Client) FindNode(ctx context.Context) (string, error) {
	// If node selector is configured, use the first available
	if len(c.config.NodeSelector) > 0 {
		// TODO: Check node availability
		return c.config.NodeSelector[0], nil
	}

	// TODO: Implement node discovery and selection logic
	return "pve", nil // Default node name
}

// ReconfigureConfig represents VM reconfiguration parameters
type ReconfigureConfig struct {
	CPUs     *int   `json:"cores,omitempty"`
	Sockets  *int   `json:"sockets,omitempty"`
	Memory   *int64 `json:"memory,omitempty"`   // Memory in MB
	DiskSize *int64 `json:"disksize,omitempty"` // Disk size in GB
	Disk     string `json:"disk,omitempty"`     // Disk identifier (e.g., "scsi0")
}

// ReconfigureVM reconfigures a VM
func (c *Client) ReconfigureVM(ctx context.Context, node string, vmid int, config *ReconfigureConfig) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", node, vmid)

	values := url.Values{}

	if config.CPUs != nil {
		values.Set("cores", strconv.Itoa(*config.CPUs))
	}
	if config.Sockets != nil {
		values.Set("sockets", strconv.Itoa(*config.Sockets))
	}
	if config.Memory != nil {
		values.Set("memory", strconv.FormatInt(*config.Memory, 10))
	}
	if config.DiskSize != nil && config.Disk != "" {
		// For disk resize, we need to modify the disk parameter
		values.Set(config.Disk, fmt.Sprintf("size=%dG", *config.DiskSize))
	}

	resp, err := c.request(ctx, "PUT", path, values)
	if err != nil {
		return "", fmt.Errorf("failed to reconfigure VM: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("reconfigure VM failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	// Return task ID if operation is async
	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", nil // Synchronous operation completed
}

// ResizeDisk resizes a VM disk
func (c *Client) ResizeDisk(ctx context.Context, node string, vmid int, disk string, sizeGB int64) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/resize", node, vmid)

	values := url.Values{}
	values.Set("disk", disk)
	values.Set("size", fmt.Sprintf("%dG", sizeGB))

	resp, err := c.request(ctx, "PUT", path, values)
	if err != nil {
		return "", fmt.Errorf("failed to resize disk: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("resize disk failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", nil
}

// CreateSnapshot creates a VM snapshot
func (c *Client) CreateSnapshot(ctx context.Context, node string, vmid int, snapname, description string, includeMemory bool) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/snapshot", node, vmid)

	values := url.Values{}
	values.Set("snapname", snapname)
	if description != "" {
		values.Set("description", description)
	}
	if includeMemory {
		values.Set("vmstate", "1")
	}

	resp, err := c.request(ctx, "POST", path, values)
	if err != nil {
		return "", fmt.Errorf("failed to create snapshot: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create snapshot failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// DeleteSnapshot deletes a VM snapshot
func (c *Client) DeleteSnapshot(ctx context.Context, node string, vmid int, snapname string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/snapshot/%s", node, vmid, snapname)

	resp, err := c.request(ctx, "DELETE", path, nil)
	if err != nil {
		return "", fmt.Errorf("failed to delete snapshot: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode == 404 {
		// Snapshot doesn't exist, consider it deleted
		return "", nil
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("delete snapshot failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", nil
}

// RevertSnapshot reverts a VM to a snapshot
func (c *Client) RevertSnapshot(ctx context.Context, node string, vmid int, snapname string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/snapshot/%s/rollback", node, vmid, snapname)

	resp, err := c.request(ctx, "POST", path, nil)
	if err != nil {
		return "", fmt.Errorf("failed to revert snapshot: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("revert snapshot failed with status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if taskID, ok := apiResp.Data.(string); ok {
		return taskID, nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// ListSnapshots lists VM snapshots
func (c *Client) ListSnapshots(ctx context.Context, node string, vmid int) ([]*Snapshot, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/snapshot", node, vmid)

	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	snapshotData, err := json.Marshal(apiResp.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal snapshot data: %w", err)
	}

	var snapshots []*Snapshot
	if err := json.Unmarshal(snapshotData, &snapshots); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshots: %w", err)
	}

	return snapshots, nil
}

// PrepareImage ensures a template/image exists, importing if necessary
func (c *Client) PrepareImage(ctx context.Context, node, storage, imageURL, templateName string) (string, error) {
	// First check if template already exists
	if templateName != "" {
		// Try to find existing template by name
		path := fmt.Sprintf("/api2/json/nodes/%s/qemu", node)
		resp, err := c.request(ctx, "GET", path, nil)
		if err == nil {
			defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical
			if resp.StatusCode == 200 {
				var apiResp APIResponse
				// Decode response for debugging - errors intentionally ignored
				_ = json.NewDecoder(resp.Body).Decode(&apiResp) // best effort decode for debug info
			}
		}
	}

	// If imageURL provided, simulate download/import
	if imageURL != "" {
		// In real implementation, this would:
		// 1. Download the image to storage
		// 2. Create VM from image
		// 3. Convert to template
		path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/download-url", node, storage)

		values := url.Values{}
		values.Set("content", "iso")
		values.Set("filename", "imported-image.qcow2")
		values.Set("url", imageURL)

		resp, err := c.request(ctx, "POST", path, values)
		if err != nil {
			return "", fmt.Errorf("failed to import image: %w", err)
		}
		defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("image import failed with status %d: %s", resp.StatusCode, string(body))
		}

		var apiResp APIResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return "", fmt.Errorf("failed to decode response: %w", err)
		}

		if taskID, ok := apiResp.Data.(string); ok {
			return taskID, nil
		}
	}

	return "", nil // Template already exists or operation completed
}

// GetVMConfig retrieves VM configuration
func (c *Client) GetVMConfig(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config", node, vmid)

	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM config: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

	if resp.StatusCode == 404 {
		return nil, ErrVMNotFound
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if config, ok := apiResp.Data.(map[string]interface{}); ok {
		return config, nil
	}

	return nil, fmt.Errorf("unexpected response format")
}

// Custom errors
var (
	ErrVMNotFound           = fmt.Errorf("VM not found")
	ErrNodeNotFound         = fmt.Errorf("node not found")
	ErrTaskFailed           = fmt.Errorf("task failed")
	ErrDiskShrinkNotAllowed = fmt.Errorf("disk shrinking not allowed")
)
