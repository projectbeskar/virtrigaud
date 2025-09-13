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

package libvirt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// GuestAgentInfo represents information gathered from QEMU Guest Agent
type GuestAgentInfo struct {
	// Guest OS Information
	OSName         string `json:"os_name"`
	OSVersion      string `json:"os_version"`
	OSKernelName   string `json:"os_kernel_name"`
	OSKernelRelease string `json:"os_kernel_release"`
	OSKernelVersion string `json:"os_kernel_version"`
	OSMachine      string `json:"os_machine"`
	OSPrettyName   string `json:"os_pretty_name"`
	
	// Guest Network Information
	NetworkInterfaces []GuestNetworkInterface `json:"network_interfaces"`
	
	// Guest Filesystem Information
	Filesystems []GuestFilesystem `json:"filesystems"`
	
	// Guest Agent Status
	AgentVersion string `json:"agent_version"`
	AgentStatus  string `json:"agent_status"`
	
	// Guest Time Information
	GuestTime time.Time `json:"guest_time"`
	
	// Guest Users
	Users []GuestUser `json:"users"`
}

// GuestNetworkInterface represents a network interface inside the guest
type GuestNetworkInterface struct {
	Name         string   `json:"name"`
	HardwareAddr string   `json:"hardware_addr"`
	IPAddresses  []string `json:"ip_addresses"`
	Statistics   NetworkStats `json:"statistics,omitempty"`
}

// NetworkStats represents network interface statistics
type NetworkStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxBytes   uint64 `json:"tx_bytes"`
	TxPackets uint64 `json:"tx_packets"`
}

// GuestFilesystem represents a filesystem inside the guest
type GuestFilesystem struct {
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// GuestUser represents a logged-in user inside the guest
type GuestUser struct {
	User     string    `json:"user"`
	Domain   string    `json:"domain,omitempty"`
	LoginTime time.Time `json:"login_time"`
}

// GuestAgentProvider manages QEMU Guest Agent communication
type GuestAgentProvider struct {
	virshProvider *VirshProvider
}

// NewGuestAgentProvider creates a new guest agent provider
func NewGuestAgentProvider(virshProvider *VirshProvider) *GuestAgentProvider {
	return &GuestAgentProvider{
		virshProvider: virshProvider,
	}
}

// GetGuestInfo retrieves comprehensive guest information via QEMU Guest Agent
func (g *GuestAgentProvider) GetGuestInfo(ctx context.Context, domainName string) (*GuestAgentInfo, error) {
	log.Printf("INFO Gathering guest information via QEMU Guest Agent for domain: %s", domainName)
	
	info := &GuestAgentInfo{
		AgentStatus: "unknown",
	}
	
	// Check if guest agent is available and responsive
	if !g.isGuestAgentAvailable(ctx, domainName) {
		info.AgentStatus = "not_available"
		log.Printf("WARN QEMU Guest Agent not available for domain: %s", domainName)
		return info, nil
	}
	
	info.AgentStatus = "available"
	
	// Gather OS information
	if err := g.getGuestOSInfo(ctx, domainName, info); err != nil {
		log.Printf("WARN Failed to get guest OS info: %v", err)
	}
	
	// Gather network information
	if err := g.getGuestNetworkInfo(ctx, domainName, info); err != nil {
		log.Printf("WARN Failed to get guest network info: %v", err)
	}
	
	// Gather filesystem information
	if err := g.getGuestFilesystemInfo(ctx, domainName, info); err != nil {
		log.Printf("WARN Failed to get guest filesystem info: %v", err)
	}
	
	// Get guest time
	if err := g.getGuestTime(ctx, domainName, info); err != nil {
		log.Printf("WARN Failed to get guest time: %v", err)
	}
	
	// Get logged-in users
	if err := g.getGuestUsers(ctx, domainName, info); err != nil {
		log.Printf("WARN Failed to get guest users: %v", err)
	}
	
	log.Printf("INFO Successfully gathered guest information for domain: %s", domainName)
	return info, nil
}

// isGuestAgentAvailable checks if QEMU Guest Agent is available and responsive
func (g *GuestAgentProvider) isGuestAgentAvailable(ctx context.Context, domainName string) bool {
	// Try to ping the guest agent - write JSON to temp file and use it
	cmd := fmt.Sprintf("echo '{\"execute\":\"guest-ping\"}' > /tmp/guest-ping.json && virsh qemu-agent-command %s \"$(cat /tmp/guest-ping.json)\" && rm -f /tmp/guest-ping.json", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", cmd)
	if err != nil {
		log.Printf("DEBUG Guest agent ping failed for %s: %v", domainName, err)
		return false
	}
	
	// Check if we got a valid response
	log.Printf("DEBUG Guest agent ping response for %s: stdout=%s, stderr=%s", domainName, result.Stdout, result.Stderr)
	if strings.Contains(result.Stdout, "return") {
		log.Printf("DEBUG Guest agent is responsive for domain: %s", domainName)
		return true
	}
	
	return false
}

// getGuestOSInfo retrieves operating system information from the guest
func (g *GuestAgentProvider) getGuestOSInfo(ctx context.Context, domainName string, info *GuestAgentInfo) error {
	// Get OS info using guest-get-osinfo command
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-get-osinfo\"}\nEOF\n)\"", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return fmt.Errorf("failed to get OS info: %w", err)
	}
	
	// Parse the JSON response
	var response struct {
		Return struct {
			Name           string `json:"name"`
			KernelRelease  string `json:"kernel-release"`
			Version        string `json:"version"`
			PrettyName     string `json:"pretty-name"`
			VersionID      string `json:"version-id"`
			KernelVersion  string `json:"kernel-version"`
			Machine        string `json:"machine"`
			ID             string `json:"id"`
		} `json:"return"`
	}
	
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return fmt.Errorf("failed to parse OS info response: %w", err)
	}
	
	// Map the response to our structure
	info.OSName = response.Return.Name
	info.OSVersion = response.Return.Version
	info.OSKernelRelease = response.Return.KernelRelease
	info.OSKernelVersion = response.Return.KernelVersion
	info.OSMachine = response.Return.Machine
	info.OSPrettyName = response.Return.PrettyName
	
	log.Printf("DEBUG Retrieved OS info: %s %s", info.OSName, info.OSVersion)
	return nil
}

// getGuestNetworkInfo retrieves network interface information from the guest
func (g *GuestAgentProvider) getGuestNetworkInfo(ctx context.Context, domainName string, info *GuestAgentInfo) error {
	// Get network interfaces using guest-network-get-interfaces command
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-network-get-interfaces\"}\nEOF\n)\"", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return fmt.Errorf("failed to get network info: %w", err)
	}
	
	// Parse the JSON response
	var response struct {
		Return []struct {
			Name         string `json:"name"`
			HardwareAddr string `json:"hardware-address"`
			IPAddresses  []struct {
				IPAddress     string `json:"ip-address"`
				IPAddressType string `json:"ip-address-type"`
				Prefix        int    `json:"prefix"`
			} `json:"ip-addresses"`
			Statistics struct {
				RxBytes   uint64 `json:"rx-bytes"`
				RxPackets uint64 `json:"rx-packets"`
				TxBytes   uint64 `json:"tx-bytes"`
				TxPackets uint64 `json:"tx-packets"`
			} `json:"statistics"`
		} `json:"return"`
	}
	
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return fmt.Errorf("failed to parse network info response: %w", err)
	}
	
	// Convert to our structure
	for _, iface := range response.Return {
		guestIface := GuestNetworkInterface{
			Name:         iface.Name,
			HardwareAddr: iface.HardwareAddr,
			Statistics: NetworkStats{
				RxBytes:   iface.Statistics.RxBytes,
				RxPackets: iface.Statistics.RxPackets,
				TxBytes:   iface.Statistics.TxBytes,
				TxPackets: iface.Statistics.TxPackets,
			},
		}
		
		// Extract IP addresses
		for _, ip := range iface.IPAddresses {
			guestIface.IPAddresses = append(guestIface.IPAddresses, ip.IPAddress)
		}
		
		info.NetworkInterfaces = append(info.NetworkInterfaces, guestIface)
	}
	
	log.Printf("DEBUG Retrieved %d network interfaces", len(info.NetworkInterfaces))
	return nil
}

// getGuestFilesystemInfo retrieves filesystem information from the guest
func (g *GuestAgentProvider) getGuestFilesystemInfo(ctx context.Context, domainName string, info *GuestAgentInfo) error {
	// Get filesystem info using guest-get-fsinfo command
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-get-fsinfo\"}\nEOF\n)\"", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return fmt.Errorf("failed to get filesystem info: %w", err)
	}
	
	// Parse the JSON response
	var response struct {
		Return []struct {
			Name       string `json:"name"`
			Mountpoint string `json:"mountpoint"`
			Type       string `json:"type"`
			TotalBytes uint64 `json:"total-bytes"`
			UsedBytes  uint64 `json:"used-bytes"`
		} `json:"return"`
	}
	
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return fmt.Errorf("failed to parse filesystem info response: %w", err)
	}
	
	// Convert to our structure
	for _, fs := range response.Return {
		guestFS := GuestFilesystem{
			Name:       fs.Name,
			Mountpoint: fs.Mountpoint,
			Type:       fs.Type,
			TotalBytes: fs.TotalBytes,
			UsedBytes:  fs.UsedBytes,
			FreeBytes:  fs.TotalBytes - fs.UsedBytes,
		}
		
		info.Filesystems = append(info.Filesystems, guestFS)
	}
	
	log.Printf("DEBUG Retrieved %d filesystems", len(info.Filesystems))
	return nil
}

// getGuestTime retrieves the current time from inside the guest
func (g *GuestAgentProvider) getGuestTime(ctx context.Context, domainName string, info *GuestAgentInfo) error {
	// Get guest time using guest-get-time command
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-get-time\"}\nEOF\n)\"", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return fmt.Errorf("failed to get guest time: %w", err)
	}
	
	// Parse the JSON response
	var response struct {
		Return int64 `json:"return"`
	}
	
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return fmt.Errorf("failed to parse guest time response: %w", err)
	}
	
	// Convert nanoseconds to time
	info.GuestTime = time.Unix(0, response.Return)
	
	log.Printf("DEBUG Retrieved guest time: %v", info.GuestTime)
	return nil
}

// getGuestUsers retrieves information about logged-in users from the guest
func (g *GuestAgentProvider) getGuestUsers(ctx context.Context, domainName string, info *GuestAgentInfo) error {
	// Get user info using guest-get-users command
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-get-users\"}\nEOF\n)\"", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return fmt.Errorf("failed to get guest users: %w", err)
	}
	
	// Parse the JSON response
	var response struct {
		Return []struct {
			User      string  `json:"user"`
			Domain    string  `json:"domain"`
			LoginTime float64 `json:"login-time"`
		} `json:"return"`
	}
	
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return fmt.Errorf("failed to parse guest users response: %w", err)
	}
	
	// Convert to our structure
	for _, user := range response.Return {
		guestUser := GuestUser{
			User:      user.User,
			Domain:    user.Domain,
			LoginTime: time.Unix(int64(user.LoginTime), 0),
		}
		
		info.Users = append(info.Users, guestUser)
	}
	
	log.Printf("DEBUG Retrieved %d logged-in users", len(info.Users))
	return nil
}

// ExecuteGuestCommand executes a command inside the guest via guest agent
func (g *GuestAgentProvider) ExecuteGuestCommand(ctx context.Context, domainName, command string) (string, error) {
	log.Printf("INFO Executing guest command in domain %s: %s", domainName, command)
	
	// Check if guest agent is available
	if !g.isGuestAgentAvailable(ctx, domainName) {
		return "", fmt.Errorf("guest agent not available for domain: %s", domainName)
	}
	
	// Execute command using guest-exec with heredoc to avoid quote issues
	escapedCommand := strings.ReplaceAll(command, `"`, `\"`)
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-exec\",\"arguments\":{\"path\":\"/bin/sh\",\"arg\":[\"-c\",\"%s\"],\"capture-output\":true}}\nEOF\n)\"", domainName, escapedCommand)
	
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return "", fmt.Errorf("failed to execute guest command: %w", err)
	}
	
	// Parse the response to get the PID
	var execResponse struct {
		Return struct {
			PID int `json:"pid"`
		} `json:"return"`
	}
	
	if err := json.Unmarshal([]byte(result.Stdout), &execResponse); err != nil {
		return "", fmt.Errorf("failed to parse exec response: %w", err)
	}
	
	// Get the command status and output
	statusCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":%d}}\nEOF\n)\"", domainName, execResponse.Return.PID)
	
	// Wait for command completion (with timeout)
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-timeout:
			return "", fmt.Errorf("command execution timeout")
		case <-ticker.C:
			statusResult, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", statusCmd)
			if err != nil {
				continue
			}
			
			var statusResponse struct {
				Return struct {
					Exited   bool   `json:"exited"`
					ExitCode int    `json:"exitcode"`
					OutData  string `json:"out-data"`
					ErrData  string `json:"err-data"`
				} `json:"return"`
			}
			
			if err := json.Unmarshal([]byte(statusResult.Stdout), &statusResponse); err != nil {
				continue
			}
			
			if statusResponse.Return.Exited {
				if statusResponse.Return.ExitCode != 0 {
					return "", fmt.Errorf("command failed with exit code %d: %s", 
						statusResponse.Return.ExitCode, statusResponse.Return.ErrData)
				}
				
				log.Printf("INFO Guest command executed successfully in domain: %s", domainName)
				return statusResponse.Return.OutData, nil
			}
		}
	}
}

// SetGuestTime synchronizes the guest time with the host
func (g *GuestAgentProvider) SetGuestTime(ctx context.Context, domainName string) error {
	log.Printf("INFO Synchronizing guest time for domain: %s", domainName)
	
	// Check if guest agent is available
	if !g.isGuestAgentAvailable(ctx, domainName) {
		return fmt.Errorf("guest agent not available for domain: %s", domainName)
	}
	
	// Set guest time using guest-set-time command (sync with host)
	heredocCmd := fmt.Sprintf("virsh qemu-agent-command %s \"$(cat <<'EOF'\n{\"execute\":\"guest-set-time\"}\nEOF\n)\"", domainName)
	result, err := g.virshProvider.runVirshCommand(ctx, "!", "bash", "-c", heredocCmd)
	if err != nil {
		return fmt.Errorf("failed to set guest time: %w", err)
	}
	
	log.Printf("DEBUG Guest time sync result: %s", result.Stdout)
	log.Printf("INFO Successfully synchronized guest time for domain: %s", domainName)
	return nil
}
