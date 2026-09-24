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
	"unicode/utf8"
)

// GuestAgentInfo represents information gathered from QEMU Guest Agent
type GuestAgentInfo struct {
	// Guest OS Information
	OSName          string `json:"os_name"`
	OSVersion       string `json:"os_version"`
	OSKernelName    string `json:"os_kernel_name"`
	OSKernelRelease string `json:"os_kernel_release"`
	OSKernelVersion string `json:"os_kernel_version"`
	OSMachine       string `json:"os_machine"`
	OSPrettyName    string `json:"os_pretty_name"`

	// Guest Network Information
	NetworkInterfaces []GuestNetworkInterface `json:"network_interfaces"`

	// Guest Filesystem Information
	Filesystems []GuestFilesystem `json:"filesystems"`

	// Guest Agent Status
	AgentVersion string `json:"agent_version"`
	AgentStatus  string `json:"agent_status"`

	// Guest Time Information
	GuestTime time.Time `json:"guest_time"`
}

// GuestNetworkInterface represents a network interface inside the guest
type GuestNetworkInterface struct {
	Name         string       `json:"name"`
	HardwareAddr string       `json:"hardware_addr"`
	IPAddresses  []string     `json:"ip_addresses"`
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

// QEMU guest-agent commands this provider issues (the "execute" field).
const (
	qgaGuestPing          = "guest-ping"
	qgaGetOSInfo          = "guest-get-osinfo"
	qgaNetworkInterfaces  = "guest-network-get-interfaces"
	qgaGetFSInfo          = "guest-get-fsinfo"
	qgaGetTime            = "guest-get-time"
	qgaExec               = "guest-exec"
	qgaExecStatus         = "guest-exec-status"
	qgaSetTime            = "guest-set-time"
	guestExecShell        = "/bin/sh"
	guestExecShellCommand = "-c"
)

// Bounds on guest-agent traffic. The guest agent is controlled by whoever runs
// the guest, so a stalling or verbose agent must not be able to hold the
// provider's per-host exec slots or bloat VirtualMachine status.
const (
	// guestAgentCommandTimeoutSeconds is passed to every `virsh
	// qemu-agent-command --timeout`, bounding how long libvirtd waits for one
	// agent reply.
	guestAgentCommandTimeoutSeconds = "3"
	// guestInfoBudget bounds the whole GetGuestInfo enrichment on top of the
	// per-command timeout: once it is spent, remaining queries are skipped.
	guestInfoBudget = 8 * time.Second
	// maxGuestInterfaces and maxGuestFilesystems cap how many guest-reported
	// entries are kept; each one becomes several ProviderRaw/status keys.
	maxGuestInterfaces  = 16
	maxGuestFilesystems = 16
	// maxGuestIPsPerInterface caps the addresses kept per guest interface.
	maxGuestIPsPerInterface = 16
	// maxGuestNameLen truncates guest-reported names (interface names, mount
	// points) that are embedded in ProviderRaw keys.
	maxGuestNameLen = 64
)

// guestAgentRequest is the JSON body of one `virsh qemu-agent-command` call.
type guestAgentRequest struct {
	Execute   string               `json:"execute"`
	Arguments *guestAgentArguments `json:"arguments,omitempty"`
}

// guestAgentArguments carries the arguments of guest-exec / guest-exec-status.
// Unused fields are omitted, so each request serializes to exactly the QMP
// shape the former hand-formatted JSON produced.
type guestAgentArguments struct {
	Path          string   `json:"path,omitempty"`
	Arg           []string `json:"arg,omitempty"`
	CaptureOutput bool     `json:"capture-output,omitempty"`
	PID           int      `json:"pid,omitempty"`
}

// agentCommand runs one QEMU guest-agent command against domainName via
// `virsh qemu-agent-command <domain> <json>`.
//
// It replaces the former `bash -c "virsh qemu-agent-command <domain>
// \"$(cat <<'EOF' … EOF)\""` construction, which (a) interpolated the domain
// name and, for guest-exec, the guest command text into a bash script on the
// hypervisor host, and (b) over SSH was flattened unquoted so the remote shell
// ran `bash -c virsh` with no arguments — i.e. the guest-agent paths silently
// never worked there. The JSON is now built with encoding/json and handed to
// virsh as ONE argv element (the transport shell-quotes it), so neither the
// domain name nor any guest command can reach a shell on the host.
func (g *GuestAgentProvider) agentCommand(ctx context.Context, domainName string, req guestAgentRequest) (*VirshResult, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode guest-agent %s request: %w", req.Execute, err)
	}
	return g.virshProvider.runVirshCommand(ctx, "qemu-agent-command",
		"--timeout", guestAgentCommandTimeoutSeconds, domainName, string(payload))
}

// GetGuestInfo retrieves guest information via the QEMU Guest Agent. The whole
// enrichment is bounded by guestInfoBudget (each call additionally by
// guestAgentCommandTimeoutSeconds), and the result is capped by
// boundGuestInfo, because the agent's answers are guest-controlled.
func (g *GuestAgentProvider) GetGuestInfo(ctx context.Context, domainName string) (*GuestAgentInfo, error) {
	log.Printf("INFO Gathering guest information via QEMU Guest Agent for domain: %s", domainName)

	ctx, cancel := context.WithTimeout(ctx, guestInfoBudget)
	defer cancel()

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

	// Logged-in guest users (guest-get-users) are deliberately not collected:
	// they would surface personal data in VirtualMachine status.
	queries := []struct {
		what string
		run  func(context.Context, string, *GuestAgentInfo) error
	}{
		{"OS info", g.getGuestOSInfo},
		{"network info", g.getGuestNetworkInfo},
		{"filesystem info", g.getGuestFilesystemInfo},
		{"time", g.getGuestTime},
	}
	for _, q := range queries {
		if ctx.Err() != nil {
			log.Printf("WARN Guest agent budget exhausted for domain %s; skipping remaining queries", domainName)
			break
		}
		if err := q.run(ctx, domainName, info); err != nil {
			log.Printf("WARN Failed to get guest %s: %v", q.what, err)
		}
	}

	boundGuestInfo(info)
	log.Printf("INFO Gathered guest information for domain: %s", domainName)
	return info, nil
}

// boundGuestInfo caps the guest-reported collections in info (interfaces,
// filesystems, addresses per interface) and truncates guest-chosen names that
// end up in ProviderRaw keys, so a hostile or misbehaving agent cannot bloat
// VirtualMachine status.
func boundGuestInfo(info *GuestAgentInfo) {
	if len(info.NetworkInterfaces) > maxGuestInterfaces {
		info.NetworkInterfaces = info.NetworkInterfaces[:maxGuestInterfaces]
	}
	for i := range info.NetworkInterfaces {
		iface := &info.NetworkInterfaces[i]
		iface.Name = truncateGuestName(iface.Name)
		if len(iface.IPAddresses) > maxGuestIPsPerInterface {
			iface.IPAddresses = iface.IPAddresses[:maxGuestIPsPerInterface]
		}
	}
	if len(info.Filesystems) > maxGuestFilesystems {
		info.Filesystems = info.Filesystems[:maxGuestFilesystems]
	}
	for i := range info.Filesystems {
		info.Filesystems[i].Mountpoint = truncateGuestName(info.Filesystems[i].Mountpoint)
	}
}

// truncateGuestName shortens s to at most maxGuestNameLen bytes without
// splitting a UTF-8 sequence.
func truncateGuestName(s string) string {
	if len(s) <= maxGuestNameLen {
		return s
	}
	cut := maxGuestNameLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// isGuestAgentAvailable checks if QEMU Guest Agent is available and responsive
func (g *GuestAgentProvider) isGuestAgentAvailable(ctx context.Context, domainName string) bool {
	// Ping the guest agent.
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{Execute: qgaGuestPing})
	if err != nil {
		log.Printf("DEBUG Guest agent ping failed for %s: %v", domainName, err)
		return false
	}

	// Check if we got a valid response
	log.Printf("DEBUG Guest agent ping response for %s: stdout=%s", domainName, result.Stdout)
	if strings.Contains(result.Stdout, "return") {
		log.Printf("DEBUG Guest agent is responsive for domain: %s", domainName)
		return true
	}

	return false
}

// getGuestOSInfo retrieves operating system information from the guest
func (g *GuestAgentProvider) getGuestOSInfo(ctx context.Context, domainName string, info *GuestAgentInfo) error {
	// Get OS info using guest-get-osinfo command
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{Execute: qgaGetOSInfo})
	if err != nil {
		return fmt.Errorf("failed to get OS info: %w", err)
	}

	// Parse the JSON response
	var response struct {
		Return struct {
			Name          string `json:"name"`
			KernelRelease string `json:"kernel-release"`
			Version       string `json:"version"`
			PrettyName    string `json:"pretty-name"`
			VersionID     string `json:"version-id"`
			KernelVersion string `json:"kernel-version"`
			Machine       string `json:"machine"`
			ID            string `json:"id"`
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
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{Execute: qgaNetworkInterfaces})
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
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{Execute: qgaGetFSInfo})
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
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{Execute: qgaGetTime})
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

// ExecuteGuestCommand executes a command inside the guest via guest agent
func (g *GuestAgentProvider) ExecuteGuestCommand(ctx context.Context, domainName, command string) (string, error) {
	log.Printf("INFO Executing guest command in domain %s: %s", domainName, command)

	// Check if guest agent is available
	if !g.isGuestAgentAvailable(ctx, domainName) {
		return "", fmt.Errorf("guest agent not available for domain: %s", domainName)
	}

	// Execute the command in the guest via guest-exec (/bin/sh -c <command>).
	// encoding/json escapes the command correctly; it is never shell-evaluated
	// on the hypervisor host, only by /bin/sh inside the guest.
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{
		Execute: qgaExec,
		Arguments: &guestAgentArguments{
			Path:          guestExecShell,
			Arg:           []string{guestExecShellCommand, command},
			CaptureOutput: true,
		},
	})
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
	statusReq := guestAgentRequest{
		Execute:   qgaExecStatus,
		Arguments: &guestAgentArguments{PID: execResponse.Return.PID},
	}

	// Wait for command completion (with timeout)
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return "", fmt.Errorf("command execution timeout")
		case <-ticker.C:
			statusResult, err := g.agentCommand(ctx, domainName, statusReq)
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
	result, err := g.agentCommand(ctx, domainName, guestAgentRequest{Execute: qgaSetTime})
	if err != nil {
		return fmt.Errorf("failed to set guest time: %w", err)
	}

	log.Printf("DEBUG Guest time sync result: %s", result.Stdout)
	log.Printf("INFO Successfully synchronized guest time for domain: %s", domainName)
	return nil
}
