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

package proxmox

// Network plumbing (ADR-0014, Slice 0a). The manager sends a Create's networks
// as json.Marshal([]contracts.NetworkAttachment) in CreateRequest.networks_json
// (internal/transport/grpc/client.go). That struct has no JSON tags, so its wire
// keys are the Go field names ("Bridge", "VLAN", "Model", "StaticIP", ...); Go's
// case-insensitive key matching reads them back into the same struct. This file
// turns those attachments into PVE's netN, ipconfigN and nameserver keys, and
// reads a netN back.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// pveDefaultNICModel is the NIC model of an attachment that names none.
	pveDefaultNICModel = "virtio"
	// pveDefaultBridge is the bridge of a NIC of a new VM that names none.
	pveDefaultBridge = "vmbr0"
	// pveMaxNICs is the number of NICs a PVE VM can have (net0 to net31).
	pveMaxNICs = 32
	// pveMaxVLANTag is the highest 802.1Q VLAN ID PVE accepts for tag=.
	pveMaxVLANTag = 4094

	// pveNetKeyPrefix is the prefix of a NIC's config key: net0, net1, ...
	pveNetKeyPrefix = "net"
	// pveNetKeyBridge, pveNetKeyTag, pveNetKeyModel and pveNetKeyMACAddr are
	// keys of a netN value.
	pveNetKeyBridge  = "bridge"
	pveNetKeyTag     = "tag"
	pveNetKeyModel   = "model"
	pveNetKeyMACAddr = "macaddr"
	// pveConfigKeyNameserver is the VM-wide DNS server list of a PVE VM.
	pveConfigKeyNameserver = "nameserver"
)

// Messages of the InvalidSpec errors a network attachment can draw. Each
// takes the attachment's index; none echoes the (tenant-supplied) value.
const (
	networksJSONInvalidMessage    = "networks_json is not a valid list of network attachments"
	networksTooManyMessage        = "the VM names more network attachments than a Proxmox VM has NICs"
	networkBridgeInvalidMessage   = "network %d: the bridge must be a Linux bridge name (letters, digits, '.', '_' or '-', at most 15 characters; the VMNetworkAttachment CRD currently allows vmbr0, vmbr1, ... only)"
	networkModelInvalidMessage    = "network %d: the NIC model is not one Proxmox VE supports"
	networkVLANInvalidMessage     = "network %d: the VLAN tag must be between 1 and 4094"
	networkMACInvalidMessage      = "network %d: the MAC address must be a unicast, non-zero 48-bit address"
	networkStaticIPInvalidMessage = "network %d: the static IP address is not a valid unicast address"
	networkPrefixInvalidMessage   = "network %d: a static IP address needs a prefix length of 1 to 32 (IPv4) or 1 to 128 (IPv6)"
	networkGatewayInvalidMessage  = "network %d: the gateway is not a valid unicast address of the same family as the static IP address"
	networkDNSInvalidMessage      = "network %d: the DNS servers must be a comma-separated list of IP addresses"
)

var (
	// pveBridgePattern matches a Linux bridge or SDN VNet name. It admits no
	// comma or whitespace, which would let a value add keys to the netN string.
	pveBridgePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

	// pveNICModels are the NIC models PVE's netN accepts.
	pveNICModels = map[string]bool{
		"e1000": true, "e1000-82540em": true, "e1000-82544gc": true, "e1000-82545em": true,
		"e1000e": true, "i82551": true, "i82557b": true, "i82559er": true,
		"ne2k_isa": true, "ne2k_pci": true, "pcnet": true, "rtl8139": true,
		"virtio": true, "vmxnet3": true,
	}

	// pveVirtioOnlyOpts are the netN options PVE accepts on a virtio NIC only.
	pveVirtioOnlyOpts = []string{"mtu", "queues", "host-tunnel"}
)

// parseNetworks turns a Create's networks_json into the NICs and the IP
// configs of the VM, one of each per attachment, indexed in order. An
// attachment's bridge, model, VLAN tag and MAC (a MAC only when the manager
// forwards one) set its NIC, and its StaticIP/Prefix/Gateway/DNS its IP
// config (DHCP when it has no StaticIP).
//
// forTemplate reports a VM cloned from a template. Its NICs keep the fields
// an attachment leaves empty (the clone's own value stays, see
// applyCloneNetworks); a new VM gets virtio on vmbr0 for them. A request with
// no attachment gives a new VM one default NIC, and a clone none (it keeps the
// template's); either way ipconfig0 defaults to DHCP.
//
// An invalid attachment is an InvalidSpec error: nothing is created.
func parseNetworks(networksJSON string, forTemplate bool) ([]pveapi.NetworkConfig, []pveapi.IPConfig, error) {
	var attachments []contracts.NetworkAttachment
	if strings.TrimSpace(networksJSON) != "" {
		if err := json.Unmarshal([]byte(networksJSON), &attachments); err != nil {
			return nil, nil, errors.NewInvalidSpec("%s", networksJSONInvalidMessage)
		}
	}
	if len(attachments) > pveMaxNICs {
		return nil, nil, errors.NewInvalidSpec("%s", networksTooManyMessage)
	}

	nets := make([]pveapi.NetworkConfig, 0, len(attachments))
	ips := make([]pveapi.IPConfig, 0, len(attachments))
	for i := range attachments {
		nic, err := nicFromAttachment(i, &attachments[i], forTemplate)
		if err != nil {
			return nil, nil, err
		}
		ipc, err := ipConfigFromAttachment(i, &attachments[i])
		if err != nil {
			return nil, nil, err
		}
		nets = append(nets, nic)
		ips = append(ips, ipc)
	}

	if len(attachments) == 0 {
		if !forTemplate {
			nets = append(nets, pveapi.NetworkConfig{Index: 0, Model: pveDefaultNICModel, Bridge: pveDefaultBridge})
		}
		ips = append(ips, pveapi.IPConfig{Index: 0, DHCP: true})
	}
	return nets, ips, nil
}

// nicFromAttachment validates attachment i and returns its NIC. Fields the
// attachment leaves empty stay empty for a template clone and default for a
// new VM.
func nicFromAttachment(i int, a *contracts.NetworkAttachment, forTemplate bool) (pveapi.NetworkConfig, error) {
	nic := pveapi.NetworkConfig{Index: i}

	if bridge := strings.TrimSpace(a.Bridge); bridge != "" {
		if !pveBridgePattern.MatchString(bridge) {
			return nic, errors.NewInvalidSpec(networkBridgeInvalidMessage, i)
		}
		nic.Bridge = bridge
	}
	if model := strings.ToLower(strings.TrimSpace(a.Model)); model != "" {
		if !pveNICModels[model] {
			return nic, errors.NewInvalidSpec(networkModelInvalidMessage, i)
		}
		nic.Model = model
	}
	if a.VLAN != 0 {
		if a.VLAN < 1 || a.VLAN > pveMaxVLANTag {
			return nic, errors.NewInvalidSpec(networkVLANInvalidMessage, i)
		}
		nic.VLAN = int(a.VLAN)
	}
	if mac := strings.TrimSpace(a.MacAddress); mac != "" {
		normalised, ok := normaliseMAC(mac)
		if !ok {
			return nic, errors.NewInvalidSpec(networkMACInvalidMessage, i)
		}
		nic.MAC = normalised
	}

	if !forTemplate {
		if nic.Model == "" {
			nic.Model = pveDefaultNICModel
		}
		if nic.Bridge == "" {
			nic.Bridge = pveDefaultBridge
		}
	}
	return nic, nil
}

// normaliseMAC returns mac as PVE writes it (upper-case, colon-separated) and
// whether it is a 48-bit unicast address other than all zeros.
func normaliseMAC(mac string) (string, bool) {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 || hw[0]&1 != 0 {
		return "", false
	}
	if hw.String() == "00:00:00:00:00:00" {
		return "", false
	}
	return strings.ToUpper(hw.String()), true
}

// parseDNSServers validates attachment i's comma-separated DNS servers and
// returns them as a normalised comma-separated list.
func parseDNSServers(i int, dns string) (string, error) {
	var servers []string
	for _, server := range strings.Split(dns, ",") {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		serverAddr, err := netip.ParseAddr(server)
		if err != nil || !usableUnicast(serverAddr) {
			return "", errors.NewInvalidSpec(networkDNSInvalidMessage, i)
		}
		servers = append(servers, serverAddr.String())
	}
	return strings.Join(servers, ","), nil
}

// ipConfigFromAttachment validates attachment i's IP settings and returns its
// IP config: DHCP when the attachment has no StaticIP, otherwise the address
// with its prefix and the gateway. The prefix comes from Prefix, or from the
// address itself when it is written in CIDR form (then Prefix, if also set,
// must agree). The DNS servers are kept for both: PVE's nameserver is
// VM-wide, so a DHCP entry's servers are applied too. The prefix and gateway
// of a DHCP entry are ignored.
func ipConfigFromAttachment(i int, a *contracts.NetworkAttachment) (pveapi.IPConfig, error) {
	ipc := pveapi.IPConfig{Index: i, DHCP: true}
	dns, dnsErr := parseDNSServers(i, a.DNS)
	if dnsErr != nil {
		return ipc, dnsErr
	}
	ipc.DNS = dns
	raw := strings.TrimSpace(a.StaticIP)
	if raw == "" {
		return ipc, nil
	}

	var addr netip.Addr
	bits := int(a.Prefix)
	if strings.Contains(raw, "/") {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return ipc, errors.NewInvalidSpec(networkStaticIPInvalidMessage, i)
		}
		if bits != 0 && bits != prefix.Bits() {
			return ipc, errors.NewInvalidSpec(networkPrefixInvalidMessage, i)
		}
		addr, bits = prefix.Addr(), prefix.Bits()
	} else {
		parsed, err := netip.ParseAddr(raw)
		if err != nil {
			return ipc, errors.NewInvalidSpec(networkStaticIPInvalidMessage, i)
		}
		addr = parsed
	}
	if !usableUnicast(addr) {
		return ipc, errors.NewInvalidSpec(networkStaticIPInvalidMessage, i)
	}
	maxBits := 128
	if addr.Is4() {
		maxBits = 32
	}
	if bits < 1 || bits > maxBits {
		return ipc, errors.NewInvalidSpec(networkPrefixInvalidMessage, i)
	}
	ipc.DHCP = false
	ipc.IP = fmt.Sprintf("%s/%d", addr, bits)

	if gw := strings.TrimSpace(a.Gateway); gw != "" {
		gwAddr, err := netip.ParseAddr(gw)
		if err != nil || !usableUnicast(gwAddr) || gwAddr.Is4() != addr.Is4() {
			return ipc, errors.NewInvalidSpec(networkGatewayInvalidMessage, i)
		}
		ipc.Gateway = gwAddr.String()
	}

	return ipc, nil
}

// usableUnicast reports whether addr is a plain unicast address: valid, not
// zoned, not an IPv4-mapped IPv6 one, and neither unspecified, loopback nor
// multicast.
func usableUnicast(addr netip.Addr) bool {
	return addr.IsValid() && addr.Zone() == "" && !addr.Is4In6() &&
		!addr.IsUnspecified() && !addr.IsLoopback() && !addr.IsMulticast()
}

// netSpecified reports whether the request names anything about the NIC: the
// bridge, the model, the VLAN tag or the MAC. A NIC it names nothing about is
// left as the template made it.
func netSpecified(nic *pveapi.NetworkConfig) bool {
	return nic.Bridge != "" || nic.Model != "" || nic.VLAN != 0 || nic.MAC != ""
}

// pveNIC is a parsed netN value: the model, the MAC if the value carries one,
// and the other options (bridge, tag, firewall, rate, ...) in their order.
type pveNIC struct {
	model string
	mac   string
	opts  []pveNICOpt
}

// pveNICOpt is one key=value option of a netN value.
type pveNICOpt struct {
	key   string
	value string
}

// parseNetValue parses a PVE netN value. PVE writes it as
// "<model>=<MAC>,bridge=<bridge>[,tag=<n>][,firewall=1]..."; a value may also
// give the model alone ("virtio"), as "model=<model>", or the MAC as
// "macaddr=<MAC>". A value it cannot read is returned with an empty model and
// its parts as options.
func parseNetValue(value string) pveNIC {
	var nic pveNIC
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		switch {
		case pveNICModels[key] && nic.model == "":
			nic.model = key
			if hasVal {
				nic.mac = val
			}
		case key == pveNetKeyModel && hasVal && nic.model == "":
			nic.model = val
		case key == pveNetKeyMACAddr && hasVal:
			nic.mac = val
		default:
			nic.opts = append(nic.opts, pveNICOpt{key: key, value: val})
		}
	}
	return nic
}

// opt returns the value of option key.
func (n *pveNIC) opt(key string) string {
	for _, o := range n.opts {
		if o.key == key {
			return o.value
		}
	}
	return ""
}

// setOpt sets option key to value, adding it if absent. An empty value
// removes it.
func (n *pveNIC) setOpt(key, value string) {
	for i, o := range n.opts {
		if o.key != key {
			continue
		}
		if value == "" {
			n.opts = append(n.opts[:i], n.opts[i+1:]...)
		} else {
			n.opts[i].value = value
		}
		return
	}
	if value != "" {
		n.opts = append(n.opts, pveNICOpt{key: key, value: value})
	}
}

// String writes the NIC as a PVE netN value: "<model>[=<MAC>]", then bridge
// and tag, then the other options.
func (n *pveNIC) String() string {
	head := n.model
	if head == "" {
		head = pveDefaultNICModel
	}
	if n.mac != "" {
		head += "=" + n.mac
	}
	parts := []string{head}
	for _, key := range []string{pveNetKeyBridge, pveNetKeyTag} {
		if v := n.opt(key); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	for _, o := range n.opts {
		if o.key == pveNetKeyBridge || o.key == pveNetKeyTag {
			continue
		}
		if o.value == "" {
			parts = append(parts, o.key)
		} else {
			parts = append(parts, o.key+"="+o.value)
		}
	}
	return strings.Join(parts, ",")
}

// mergeNetValue returns the netN value for a NIC that exists as current (the
// value PVE gave a clone, possibly empty when the template has no such NIC)
// and that the request names as nic. The request's model replaces the
// current one; the current MAC is kept (PVE gave the clone its own, and the
// provider does not draw another), unless the request carries one; other
// options of the current NIC (firewall, rate, ...) are kept. A request that
// names a bridge names the NIC's whole L2 placement: its VLAN tag replaces
// the current one, and none removes it. A request that names only a model or
// a MAC leaves the bridge and tag as they are. A NIC with no bridge at all
// gets pveDefaultBridge.
func mergeNetValue(current string, nic *pveapi.NetworkConfig) string {
	merged := parseNetValue(current)
	if nic.Model != "" {
		merged.model = nic.Model
	}
	if merged.model != "" && merged.model != pveDefaultNICModel {
		// PVE refuses these options on a model other than virtio.
		for _, key := range pveVirtioOnlyOpts {
			merged.setOpt(key, "")
		}
	}
	if nic.MAC != "" {
		merged.mac = nic.MAC
	}
	if nic.Bridge != "" {
		merged.setOpt(pveNetKeyBridge, nic.Bridge)
		merged.setOpt(pveNetKeyTag, vlanOpt(nic.VLAN))
	} else if nic.VLAN != 0 {
		merged.setOpt(pveNetKeyTag, vlanOpt(nic.VLAN))
	}
	if merged.opt(pveNetKeyBridge) == "" {
		merged.setOpt(pveNetKeyBridge, pveDefaultBridge)
	}
	return merged.String()
}

// vlanOpt is the value of a tag= option for VLAN, "" for none.
func vlanOpt(vlan int) string {
	if vlan <= 0 {
		return ""
	}
	return strconv.Itoa(vlan)
}

// applyCloneNetworks applies, to a VM cloned from a template, the network
// settings the request names: for each NIC it names (a bridge, model, VLAN
// or MAC), the clone's netN is read and merged with the request (see
// mergeNetValue), so the template's bridge, VLAN and options survive where
// the request is silent and the clone keeps the MAC PVE gave it. A NIC the
// request does not name is not touched. Each attachment's static IP becomes
// an ipconfigN, and the DNS servers the VM-wide nameserver; a DHCP ipconfigN
// is set only when cloudInit says the VM is given a cloud-init configuration
// by this request. It sets what it sets in one idempotent config update, so a
// re-sent Create that finds its VM interrupted runs it again.
func (p *Provider) applyCloneNetworks(ctx context.Context, node string, vmConfig *pveapi.VMConfig, cloudInit bool) error {
	values := url.Values{}

	var current map[string]interface{}
	for i := range vmConfig.Networks {
		nic := &vmConfig.Networks[i]
		if !netSpecified(nic) {
			continue
		}
		if current == nil {
			config, err := p.client.GetVMConfig(ctx, node, vmConfig.VMID)
			if err != nil {
				return fmt.Errorf("read the network config of cloned VM %d: %w", vmConfig.VMID, err)
			}
			current = config
		}
		key := fmt.Sprintf("%s%d", pveNetKeyPrefix, nic.Index)
		existing, _ := current[key].(string)
		values.Set(key, mergeNetValue(existing, nic))
	}

	for _, ipConfig := range vmConfig.IPConfigs {
		if ipConfig.DHCP && !cloudInit {
			continue
		}
		if value := pveapi.IPConfigString(ipConfig); value != "" {
			values.Set(fmt.Sprintf("ipconfig%d", ipConfig.Index), value)
		}
	}
	if nameserver := pveapi.NameserverValue(vmConfig.IPConfigs); nameserver != "" {
		values.Set(pveConfigKeyNameserver, nameserver)
	}

	if len(values) == 0 {
		return nil
	}
	p.logger.Info("Applying the network configuration to the cloned VM", "vmid", vmConfig.VMID)
	taskID, err := p.client.ReconfigureVMRaw(ctx, node, vmConfig.VMID, values)
	if err != nil {
		return fmt.Errorf("apply the network config to cloned VM %d: %w", vmConfig.VMID, err)
	}
	if err := p.client.WaitForTask(ctx, node, taskID); err != nil {
		return fmt.Errorf("apply the network config to cloned VM %d: %w", vmConfig.VMID, err)
	}
	return nil
}

// netKeyIndex returns N of a config key "netN" and whether key is one.
func netKeyIndex(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, pveNetKeyPrefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// networkInfosFromConfig lists the NICs of a VM config as the contract
// reports them, ordered by NIC index: the bridge as the network name, and the
// MAC.
func networkInfosFromConfig(config map[string]interface{}) []*providerv1.NetworkInfo {
	type indexed struct {
		index int
		info  *providerv1.NetworkInfo
	}
	var found []indexed
	for key, value := range config {
		index, ok := netKeyIndex(key)
		if !ok {
			continue
		}
		str, ok := value.(string)
		if !ok || str == "" {
			continue
		}
		nic := parseNetValue(str)
		found = append(found, indexed{index: index, info: &providerv1.NetworkInfo{Name: nic.opt(pveNetKeyBridge), Mac: nic.mac}})
	}
	sort.Slice(found, func(a, b int) bool { return found[a].index < found[b].index })
	infos := make([]*providerv1.NetworkInfo, 0, len(found))
	for _, f := range found {
		infos = append(infos, f.info)
	}
	return infos
}
