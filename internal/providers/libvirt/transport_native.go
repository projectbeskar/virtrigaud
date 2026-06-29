//go:build libvirt_native

// Native libvirt control-plane transport — initial slice.
//
// First cut of the native connection the migration sketch targets
// (docs/libvirt-go-sdk-migration-sketch.md). It deliberately reuses the
// EXISTING host-key trust model rather than inventing a new one: the
// qemu+ssh:// URI built by setupConnection, the ~/.ssh/config written by
// createSSHConfig (StrictHostKeyChecking + UserKnownHostsFile=known_hosts),
// and the verifyKnownHostsPresent hard-fail gate. The official binding's
// qemu+ssh transport reads that same ssh config + known_hosts, so the ADR-0004
// trust semantics carry over with zero new mechanism.
//
// Build-tagged: importing libvirt.org/go/libvirt turns the whole package into a
// CGO build that needs libvirt-dev headers. Until the transport is wired into
// Provider, keep it opt-in with `-tags libvirt_native` so the default
// `go build`/`go test` on machines without libvirt headers is unaffected.
package libvirt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	libvirtgo "libvirt.org/go/libvirt"
)

// init registers the native dialer so buildNativeTransport (in the non-tagged
// shim) can construct it. *nativeConn satisfies controlTransport.
func init() {
	nativeDialer = func(uri string, hostKey hostKeyPolicy) (controlTransport, error) {
		return dialNative(uri, hostKey)
	}
}

// guestAgentTimeoutSecs bounds every guest-agent round-trip. An absent agent
// errors fast; a hung one is capped here so the rich path can never block a
// caller for long (the CGO call does not honor ctx, so this is the real cap).
const guestAgentTimeoutSecs = 3

// nativeConn is a persistent native libvirt control-plane connection. One
// long-lived connection replaces the per-call `virsh ... over ssh` fork that
// the Validate-storm and Describe-timeout investigations identified as the
// transport tax.
type nativeConn struct {
	c   *libvirtgo.Connect
	uri string // remembered for deriving the console host
}

// dialNative is THE single secured entry point for the native transport — the
// chokepoint where the trust contract is enforced. It re-runs the host-key
// hard-fail gate, then opens a persistent connection over the already-built
// qemu+ssh:// URI.
//
// Precondition: setupConnection has run on the owning VirshProvider, so
// ~/.ssh/config and the private key are on disk and `uri` already carries
// no_tty/keyfile/no_verify. We still call verifyKnownHostsPresent here so the
// native path independently fails closed if ever dialed without trust material
// (ADR-0004: no TOFU, no silent downgrade).
func dialNative(uri string, hostKey hostKeyPolicy) (*nativeConn, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse libvirt URI %q: %w", uri, err)
	}
	if err := hostKey.verifyKnownHostsPresent(u.Host); err != nil {
		return nil, fmt.Errorf("native libvirt host-key pre-flight failed: %w", err)
	}

	c, err := libvirtgo.NewConnect(uri)
	if err != nil {
		return nil, fmt.Errorf("native libvirt connect to %q failed: %w", uri, err)
	}
	return &nativeConn{c: c, uri: uri}, nil
}

// Validate is the cheap liveness probe that replaces `virsh list --all --name`.
// IsAlive is a check on the existing connection — no domain enumeration, no
// subprocess, no ssh fork. This is the Phase-1 win: many concurrent reconciles
// stop translating into a fork storm.
//
// ponytail: ctx is accepted for a uniform transport signature but not threaded
// — the CGO call is blocking. Wire a watchdog/cancel only if a hung libvirtd
// round-trip actually shows up in practice.
func (n *nativeConn) Validate(ctx context.Context) error {
	alive, err := n.c.IsAlive()
	if err != nil {
		return fmt.Errorf("native libvirt liveness check failed: %w", err)
	}
	if !alive {
		return fmt.Errorf("native libvirt connection is not alive")
	}
	return nil
}

// DescribeBasic is the cheapest existence+power probe: two typed calls, no IP
// discovery, no XML. Kept for callers/tests that only need liveness of a single
// domain. Describe builds on the same lookup for the full controller response.
func (n *nativeConn) DescribeBasic(ctx context.Context, name string) (exists bool, state string, err error) {
	dom, err := n.c.LookupDomainByName(name)
	if err != nil {
		if isNoDomain(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("lookup domain %q: %w", name, err)
	}
	defer func() { _ = dom.Free() }()

	st, _, err := dom.GetState()
	if err != nil {
		return true, "", fmt.Errorf("get state for %q: %w", name, err)
	}
	return true, domainStateString(st), nil
}

// Describe is the reconcile-safe path: it fills the full
// contracts.DescribeResponse (existence, power state, best-effort IPs, console
// URL, minimal provider-raw) using only cheap typed calls and deliberately
// OMITS the heavy monitoring/guest-agent sweep. This is what the VM controller
// calls on every reconcile.
func (n *nativeConn) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	dom, err := n.c.LookupDomainByName(id)
	if err != nil {
		if isNoDomain(err) {
			return contracts.DescribeResponse{Exists: false}, nil
		}
		return contracts.DescribeResponse{}, fmt.Errorf("lookup domain %q: %w", id, err)
	}
	defer func() { _ = dom.Free() }()
	return n.describeCore(dom, id)
}

// DescribeRich returns the same response as Describe plus observability data in
// ProviderRaw: host-side stats (memory/CPU/block) which are always cheap and
// agent-free, and best-effort guest-agent enrichment (OS/hostname) which is
// bounded by guestAgentTimeoutSecs and skipped entirely if the agent is absent.
// It is NOT for the reconcile hot path — call it from a lower-frequency
// monitoring path so a slow/missing guest agent can never stall reconcile.
func (n *nativeConn) DescribeRich(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	dom, err := n.c.LookupDomainByName(id)
	if err != nil {
		if isNoDomain(err) {
			return contracts.DescribeResponse{Exists: false}, nil
		}
		return contracts.DescribeResponse{}, fmt.Errorf("lookup domain %q: %w", id, err)
	}
	defer func() { _ = dom.Free() }()

	resp, err := n.describeCore(dom, id)
	if err != nil || !resp.Exists {
		return resp, err
	}

	// Host-side: no guest agent, no fork — safe to collect inline.
	enrichMemoryStats(dom, resp.ProviderRaw)
	enrichCPUStats(dom, resp.ProviderRaw)
	enrichBlockStats(dom, resp.ProviderRaw)

	// Guest-side: only when running, best-effort and time-bounded.
	if resp.PowerState == "On" {
		enrichGuestAgent(dom, resp.ProviderRaw)
	}
	return resp, nil
}

// describeCore fills the five contract fields from cheap typed calls. Shared by
// Describe and DescribeRich so the core is identical regardless of monitoring.
func (n *nativeConn) describeCore(dom *libvirtgo.Domain, id string) (contracts.DescribeResponse, error) {
	state, _, err := dom.GetState()
	if err != nil {
		return contracts.DescribeResponse{}, fmt.Errorf("get state for %q: %w", id, err)
	}
	power := powerStateOnOff(state)

	raw := map[string]string{
		"name":               id,
		"state":              domainStateString(state),
		"power_state_mapped": power,
	}
	if uuid, err := dom.GetUUIDString(); err == nil {
		raw["uuid"] = uuid
	}
	if info, err := dom.GetInfo(); err == nil {
		raw["vcpus"] = strconv.Itoa(int(info.NrVirtCpu))
		raw["memory_kib"] = strconv.FormatUint(info.Memory, 10)
		raw["max_memory_kib"] = strconv.FormatUint(info.MaxMem, 10)
	}

	ips := n.describeIPs(dom, state)
	if len(ips) > 0 {
		raw["primary_ip"] = ips[0]
	}

	consoleURL := ""
	if state == libvirtgo.DOMAIN_RUNNING {
		consoleURL = consoleURLFromXML(dom, n.consoleHost())
	}

	return contracts.DescribeResponse{
		Exists:      true,
		PowerState:  power,
		IPs:         ips,
		ConsoleURL:  consoleURL,
		ProviderRaw: raw,
	}, nil
}

// describeIPs collects best-effort IPv4/IPv6 addresses for a running domain.
// It tries the dnsmasq lease DB first (no guest agent needed), then the guest
// agent, deduping and dropping loopback/link-local. Every source is
// best-effort: a missing lease DB or absent guest agent is not an error, it
// just yields fewer IPs — the reconcile path must not fail because IP discovery
// is incomplete.
func (n *nativeConn) describeIPs(dom *libvirtgo.Domain, state libvirtgo.DomainState) []string {
	if state != libvirtgo.DOMAIN_RUNNING {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, src := range []libvirtgo.DomainInterfaceAddressesSource{
		libvirtgo.DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE,
		libvirtgo.DOMAIN_INTERFACE_ADDRESSES_SRC_AGENT,
	} {
		ifaces, err := dom.ListAllInterfaceAddresses(src)
		if err != nil {
			continue
		}
		for _, iface := range ifaces {
			for _, a := range iface.Addrs {
				ip := strings.TrimSpace(a.Addr)
				if ip == "" || ip == "::1" || strings.HasPrefix(ip, "127.") || strings.HasPrefix(ip, "fe80:") || seen[ip] {
					continue // drop empty, loopback, and link-local noise
				}
				seen[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

// consoleHost derives the hypervisor host for a VNC URL from the connection URI.
func (n *nativeConn) consoleHost() string {
	if u, err := url.Parse(n.uri); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "localhost"
}

// Close releases the native connection. Safe to call on a nil-wrapped conn.
func (n *nativeConn) Close() {
	if n != nil && n.c != nil {
		_, _ = n.c.Close()
	}
}

// --- enrichment (DescribeRich only) ---------------------------------------

// enrichMemoryStats adds balloon/guest memory stats. Host-side, no guest agent.
func enrichMemoryStats(dom *libvirtgo.Domain, raw map[string]string) {
	stats, err := dom.MemoryStats(uint32(libvirtgo.DOMAIN_MEMORY_STAT_NR), 0)
	if err != nil {
		return
	}
	for _, s := range stats {
		if name := memStatName(s.Tag); name != "" {
			raw["mem_"+name+"_kib"] = strconv.FormatUint(s.Val, 10)
		}
	}
}

// enrichCPUStats adds cumulative CPU time. Host-side, no guest agent.
func enrichCPUStats(dom *libvirtgo.Domain, raw map[string]string) {
	if info, err := dom.GetInfo(); err == nil {
		raw["cpu_time_ns"] = strconv.FormatUint(info.CpuTime, 10)
	}
}

// enrichBlockStats adds per-disk I/O counters. Host-side (qemu view), no agent.
func enrichBlockStats(dom *libvirtgo.Domain, raw map[string]string) {
	xml, err := dom.GetXMLDesc(0)
	if err != nil {
		return
	}
	for _, dev := range diskTargets(xml) {
		bs, err := dom.BlockStats(dev)
		if err != nil {
			continue
		}
		if bs.RdBytesSet {
			raw["blk_"+dev+"_rd_bytes"] = strconv.FormatInt(bs.RdBytes, 10)
		}
		if bs.WrBytesSet {
			raw["blk_"+dev+"_wr_bytes"] = strconv.FormatInt(bs.WrBytes, 10)
		}
		if bs.RdReqSet {
			raw["blk_"+dev+"_rd_req"] = strconv.FormatInt(bs.RdReq, 10)
		}
		if bs.WrReqSet {
			raw["blk_"+dev+"_wr_req"] = strconv.FormatInt(bs.WrReq, 10)
		}
	}
}

// enrichGuestAgent adds in-guest data (OS, hostname) via the QEMU guest agent.
// Best-effort and time-bounded: it pings first and, if the agent is absent or
// unresponsive within guestAgentTimeoutSecs, records that and returns without
// touching the rest — never an error, never a long block. Uses raw agent
// commands (not virDomainGetGuestInfo) so it also works against old libvirtd
// that lacks the higher-level guestinfo procedure.
func enrichGuestAgent(dom *libvirtgo.Domain, raw map[string]string) {
	const timeout = libvirtgo.DomainQemuAgentCommandTimeout(guestAgentTimeoutSecs)

	if _, err := dom.QemuAgentCommand(`{"execute":"guest-ping"}`, timeout, 0); err != nil {
		raw["guest_agent"] = "unavailable"
		return
	}
	raw["guest_agent"] = "available"

	if out, err := dom.QemuAgentCommand(`{"execute":"guest-get-osinfo"}`, timeout, 0); err == nil {
		var r struct {
			Return struct {
				Name          string `json:"name"`
				Version       string `json:"version"`
				PrettyName    string `json:"pretty-name"`
				KernelRelease string `json:"kernel-release"`
				ID            string `json:"id"`
			} `json:"return"`
		}
		if json.Unmarshal([]byte(out), &r) == nil {
			setIf(raw, "guest_os", r.Return.Name)
			setIf(raw, "guest_os_version", r.Return.Version)
			setIf(raw, "guest_os_pretty", r.Return.PrettyName)
			setIf(raw, "guest_kernel", r.Return.KernelRelease)
		}
	}

	if out, err := dom.QemuAgentCommand(`{"execute":"guest-get-host-name"}`, timeout, 0); err == nil {
		var r struct {
			Return struct {
				HostName string `json:"host-name"`
			} `json:"return"`
		}
		if json.Unmarshal([]byte(out), &r) == nil {
			setIf(raw, "guest_hostname", r.Return.HostName)
		}
	}
}

func setIf(raw map[string]string, k, v string) {
	if v != "" {
		raw[k] = v
	}
}

// isNoDomain reports whether err is libvirt's "domain not found".
func isNoDomain(err error) bool {
	lverr, ok := err.(libvirtgo.Error)
	return ok && lverr.Code == libvirtgo.ERR_NO_DOMAIN
}

// powerStateOnOff maps libvirt domain state to the contract's On/Off, matching
// the existing virsh mapLibvirtPowerState (running -> On, everything else -> Off).
func powerStateOnOff(s libvirtgo.DomainState) string {
	if s == libvirtgo.DOMAIN_RUNNING {
		return "On"
	}
	return "Off"
}

// memStatName maps the libvirt memory-stat tag to a short field name; "" for
// tags we don't surface.
func memStatName(tag int32) string {
	switch libvirtgo.DomainMemoryStatTags(tag) {
	case libvirtgo.DOMAIN_MEMORY_STAT_RSS:
		return "rss"
	case libvirtgo.DOMAIN_MEMORY_STAT_AVAILABLE:
		return "available"
	case libvirtgo.DOMAIN_MEMORY_STAT_UNUSED:
		return "unused"
	case libvirtgo.DOMAIN_MEMORY_STAT_USABLE:
		return "usable"
	case libvirtgo.DOMAIN_MEMORY_STAT_ACTUAL_BALLOON:
		return "actual_balloon"
	default:
		return ""
	}
}

// diskTargets extracts disk target dev names (vd*/sd*/hd*/xvd*) from domain XML,
// skipping NIC and other <target> elements so BlockStats is only called on disks.
func diskTargets(xml string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(xml, "\n") {
		if !strings.Contains(line, "<target ") {
			continue
		}
		dev := xmlAttr(line, "dev")
		if dev == "" || seen[dev] {
			continue
		}
		if strings.HasPrefix(dev, "vd") || strings.HasPrefix(dev, "sd") ||
			strings.HasPrefix(dev, "hd") || strings.HasPrefix(dev, "xvd") {
			seen[dev] = true
			out = append(out, dev)
		}
	}
	return out
}

// consoleURLFromXML scans the domain XML for a graphics device (VNC or SPICE)
// and returns a console URL like vnc://host:5901 or spice://host:5915, or ""
// when there is no usable graphics port (autoport unresolved -> port='-1', or a
// headless/serial-only guest). String-scan to avoid a full XML schema dep.
func consoleURLFromXML(dom *libvirtgo.Domain, host string) string {
	xml, err := dom.GetXMLDesc(0)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(xml, "\n") {
		if !strings.Contains(line, "<graphics") {
			continue
		}
		typ := xmlAttr(line, "type")
		port := xmlAttr(line, "port")
		if port == "" || port == "-1" {
			continue
		}
		switch typ {
		case "vnc", "spice":
			return fmt.Sprintf("%s://%s:%s", typ, host, port)
		}
	}
	return ""
}

// xmlAttr pulls the value of a single-quoted XML attribute (key='value') out of
// one line; "" if absent.
func xmlAttr(line, key string) string {
	i := strings.Index(line, key+"='")
	if i == -1 {
		return ""
	}
	rest := line[i+len(key)+2:]
	if j := strings.Index(rest, "'"); j != -1 {
		return rest[:j]
	}
	return ""
}

// domainStateString maps the typed libvirt domain state to the lowercase
// strings the rest of the provider already speaks (matching virsh `domstate`).
func domainStateString(s libvirtgo.DomainState) string {
	switch s {
	case libvirtgo.DOMAIN_RUNNING:
		return "running"
	case libvirtgo.DOMAIN_BLOCKED:
		return "blocked"
	case libvirtgo.DOMAIN_PAUSED:
		return "paused"
	case libvirtgo.DOMAIN_SHUTDOWN:
		return "shutdown"
	case libvirtgo.DOMAIN_SHUTOFF:
		return "shutoff"
	case libvirtgo.DOMAIN_CRASHED:
		return "crashed"
	case libvirtgo.DOMAIN_PMSUSPENDED:
		return "pmsuspended"
	default:
		return "nostate"
	}
}
