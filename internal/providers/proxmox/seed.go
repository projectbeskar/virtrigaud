/*
Copyright 2026.

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

import (
	"archive/tar"
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/projectbeskar/virtrigaud/internal/providers/cloneidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// The NoCloud seed volume (R35).
//
// A request's cloud-init user-data reaches a Proxmox guest whole, as on
// libvirt (a NoCloud seed ISO) and vSphere (guestinfo.userdata): the provider
// builds a NoCloud ("cidata") ISO9660 image of the user-data, a meta-data it
// renders and a network-config it renders, on the PVE node, and attaches it
// to the VM as a CD-ROM. PVE's own cloud-init drive cannot carry a whole
// user-data document without a `cicustom` snippet, which needs a storage with
// the `snippets` content type and outlives the VM.
//
// How it is built (buildSeedScript, one SSH session to the node the API
// endpoint names):
//
//   - The three files travel as a tar archive on the session's stdin, never on
//     a command line, never in a log. They are extracted into a fresh mktemp
//     directory (0700) inside seedStageDir (on /run, a tmpfs), a directory
//     only root can enter, under a per-VM lock,
//     and the ISO is made there with genisoimage (a qemu-server dependency).
//   - `qm importdisk <vmid> <iso> <storage> --format raw` copies it into the
//     VM's own storage as a VM-owned volume (vm-<vmid>-disk-N; never named
//     vm-<vmid>-cloudinit, which PVE regenerates). raw keeps a file storage
//     from turning the ISO into qcow2, which the guest could not read.
//   - `qm set <vmid> --<slot> <volume>,media=cdrom` attaches it. When the VM
//     has a PVE cloud-init drive, the seed takes that drive's slot, which
//     makes PVE free the drive's volume: two cidata sources would be undefined
//     behaviour. The seed is not in the boot order: qemu-server appends the
//     new CD-ROM to it, and a config update of its own takes it out again
//     (keepSeedOutOfBootOrder).
//   - The temporary directory is removed on every exit, and a volume that was
//     imported and identified as the seed but could not be attached is
//     deleted again (`qm set --delete
//     unusedN` frees it).
//
// A seed is never combined with PVE's cloud-init settings: the provider sets
// no ciuser, sshkeys, ipconfigN or nameserver for such a VM.
//
// Lifecycle. The seed volume is VM-owned (vm-<vmid>-disk-N). `qm destroy`
// frees the volumes a VM's config references as disks, unused disks or its
// cloud-init drive, but skips CD-ROMs, so the seed is freed by the
// destroy-unreferenced-disks=1 that Delete and every Create rollback pass
// (pveapi DeleteVM): it frees every volume of the VMID on the enabled
// storages. A clone's copy of its source's CD-ROM line references a volume
// the clone does not own, which neither destroy touches (cloneSeed replaces
// it).

const (
	// seedStageDir is the root-only directory on the PVE node the seed ISO is
	// built in (each build in its own mktemp directory, removed on exit). It is
	// on /run, a tmpfs, so a build a crash interrupted does not outlive a reboot.
	seedStageDir = "/run/virtrigaud/seed-stage"

	// seedWorkTemplate is the mktemp template of one build's directory.
	seedWorkTemplate = ".seed-XXXXXXXXXX"

	// seedStaleMinutes is the age after which a build directory a killed
	// script left behind is removed by the next build.
	seedStaleMinutes = 60

	// maxSeedFileBytes bounds each seed file (user-data, meta-data). A
	// cloud-init document is typically a few KiB.
	maxSeedFileBytes = 1 << 20

	// seedFileUserData, seedFileMetaData and seedFileNetworkConfig are the
	// NoCloud files of a seed.
	seedFileUserData      = "user-data"
	seedFileMetaData      = "meta-data"
	seedFileNetworkConfig = "network-config"

	// seedExitNoGenisoimage, seedExitImportBusy, seedExitImportUnclear and
	// seedExitNotOnNode
	// are the exit codes of the seed scripts: genisoimage is missing on the
	// node; a `qm importdisk` of the VM is still running (an interrupted
	// earlier attempt); the import's volume could not be told apart; the
	// VM is not on the node the SSH data plane reaches.
	seedExitNoGenisoimage = 20
	seedExitImportBusy    = 21
	seedExitImportUnclear = 22
	seedExitNotOnNode     = 23
	// seedExitQMConfig, seedExitQMImport and seedExitQMSet are the exit codes
	// of the seed script when `qm config`, `qm importdisk` or `qm set` fails.
	// qm (Perl) can die with any status, 255 included, which ssh also uses
	// for a broken connection: each qm call has its own code instead, so a
	// failed qm call is a definite failure, never "the session broke".
	seedExitQMConfig = 24
	seedExitQMImport = 25
	seedExitQMSet    = 26

	// seedEmptyUserData is a clone's seed user-data when its VMClone gives
	// none (NoCloud requires a user-data file): a cloud-config whose body is
	// an empty mapping, which configures nothing. A body with no YAML document
	// at all (just the "#cloud-config" line) is "empty cloud config" to
	// cloud-init: it logs a warning for it, and `cloud-init status` then
	// reports "degraded done" (exit 2) instead of "done" (rc1 finding O1).
	seedEmptyUserData = "#cloud-config\n# No user-data was given for this clone.\n{}\n"

	// seedFallbackIDFormat names a VM in its instance-id when the request
	// carries no owner UID (cloneidentity.InstanceID's fallback).
	seedFallbackIDFormat = "pve-%d"
)

// Wire messages of the seed's definite refusals. None of them repeats a
// request value, a host name or a path.
const (
	seedNoSSHMessage = "the Proxmox provider applies cloud-init user-data through a NoCloud seed it builds on the PVE " +
		"node over SSH, and the SSH data plane is not configured: set ssh_user and ssh_password or ssh_privatekey " +
		"(and known_hosts) in the Provider's credentials Secret; nothing was created"
	seedNoSSHCredentialsMessage = "the Proxmox provider applies cloud-init user-data through a NoCloud seed it builds on " +
		"the PVE node over SSH, and the Provider's credentials Secret has no SSH credentials: set ssh_password or " +
		"ssh_privatekey; nothing was created"
	seedNoKnownHostsMessage = "the Proxmox provider applies cloud-init user-data through a NoCloud seed it builds on " +
		"the PVE node over SSH, and SSH host-key verification is on but the credentials Secret has no known_hosts " +
		"(see the provider log); nothing was created"
	seedNoGenisoimageMessage = "the Proxmox provider applies cloud-init user-data through a NoCloud seed it builds on " +
		"the PVE node, and genisoimage is not installed there (install the genisoimage package); nothing was created"
	seedNotOnNodeMessage = "the Proxmox provider builds a VM's cloud-init seed on the PVE node its SSH data plane " +
		"reaches (the API endpoint's host), and the VM is on another node: point PVE_NODE_SELECTOR (or the " +
		"clone's placement host) at that node"
	seedUserDataTooLargeMessage = "the cloud-init user-data is larger than 1 MiB"
	seedMetaDataTooLargeMessage = "the cloud-init meta-data is larger than 1 MiB"
	seedMetaDataInvalidMessage  = "the cloud-init meta-data is not a YAML mapping"
	seedNoSlotMessage           = "the VM has no free IDE or SATA slot for its cloud-init seed CD-ROM"
	seedCustomizeInvalidMessage = "the clone's customization is not valid JSON"
)

// seedVolumePattern matches the volume (after "<storage>:") of a seed: a
// VM-owned disk volume vm-<vmid>-disk-N, in the <vmid>/ directory and with
// the raw extension a file storage gives it. Submatch 1 is its VMID.
var seedVolumePattern = regexp.MustCompile(`^(?:\d+/)?vm-(\d+)-disk-\d+(?:\.(?:raw|qcow2|vmdk))?$`)

// seedSlotCandidates are the slots a seed CD-ROM goes to when the VM has no
// PVE cloud-init drive, in order of preference (ide2 is PVE's own CD-ROM and
// cloud-init slot).
var seedSlotCandidates = []string{"ide2", "ide0", "ide1", "ide3", "sata0", "sata1", "sata2", "sata3", "sata4", "sata5"}

// seedContent is the NoCloud seed of one VM: its files, byte for byte.
type seedContent struct {
	userData      []byte
	metaData      []byte
	networkConfig []byte // nil: no network-config file
}

// seedInFlightError is a seed build whose outcome is unknown: its SSH session
// broke or was cut off while the node may still run it, or a `qm importdisk`
// of the VM is still running. The VM must not be removed under it.
type seedInFlightError struct {
	cause error
}

func (e *seedInFlightError) Error() string {
	return fmt.Sprintf("the cloud-init seed may still be being built on the node: %v", e.cause)
}

func (e *seedInFlightError) Unwrap() error { return e.cause }

// isSeedInFlight reports whether err is (or wraps) a seedInFlightError.
func isSeedInFlight(err error) bool {
	var inFlight *seedInFlightError
	return stderrors.As(err, &inFlight)
}

// seedDrive reports whether a drive's config value is a seed CD-ROM — a
// media=cdrom drive whose volume is a VM-owned disk volume — and the VMID
// that owns the volume.
func seedDrive(value string) (owner int, ok bool) {
	head, opts, _ := strings.Cut(strings.TrimSpace(value), ",")
	if !hasDriveOption(opts, pveMediaCDROM) {
		return 0, false
	}
	storage, volume, found := strings.Cut(head, ":")
	if !found || !pveapi.ValidStorageID(storage) {
		return 0, false
	}
	m := seedVolumePattern.FindStringSubmatch(volume)
	if m == nil {
		return 0, false
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return id, true
}

// hasDriveOption reports whether the comma-separated drive options opts
// contain option (e.g. "media=cdrom").
func hasDriveOption(opts, option string) bool {
	for _, o := range strings.Split(opts, ",") {
		if strings.EqualFold(strings.TrimSpace(o), option) {
			return true
		}
	}
	return false
}

// seedSlots returns, sorted, the drive slots of cfg that hold a seed, split
// into those whose volume VM vmid owns and those that reference another VM's
// (a clone's copy of its source's CD-ROM line).
func seedSlots(cfg map[string]interface{}, vmid int) (own, foreign []string) {
	for key, v := range cfg {
		value, ok := v.(string)
		if !ok || !pveBusDiskKey.MatchString(key) {
			continue
		}
		owner, isSeed := seedDrive(value)
		switch {
		case !isSeed:
		case owner == vmid:
			own = append(own, key)
		default:
			foreign = append(foreign, key)
		}
	}
	sort.Strings(own)
	sort.Strings(foreign)
	return own, foreign
}

// seedSlotFor returns the slot a new seed for VM vmid goes to: the slot of
// another VM's seed the config references (it is replaced), else the slot of
// the VM's PVE cloud-init drive (replacing it makes PVE free its volume),
// else the first free seedSlotCandidates slot.
func seedSlotFor(cfg map[string]interface{}, vmid int) (string, bool) {
	if _, foreign := seedSlots(cfg, vmid); len(foreign) > 0 {
		return foreign[0], true
	}
	if slot, _, ok := pveapi.CloudInitDrive(cfg); ok {
		return slot, true
	}
	for _, slot := range seedSlotCandidates {
		v, present := cfg[slot]
		if s, isString := v.(string); !present || (isString && strings.TrimSpace(s) == "") {
			return slot, true
		}
	}
	return "", false
}

// createSeedInstanceID is the instance-id of a VM made by req: the owner
// VirtualMachine's UID (cloneidentity.InstanceID, as a clone's), so it is
// unique per VirtualMachine and the same for a re-sent Create; without an
// owner UID, its VMID.
func createSeedInstanceID(req *providerv1.CreateRequest, vmid int) string {
	owner := contracts.ObjectIdentity{UID: req.GetOwner().GetUid(), Namespace: req.GetOwner().GetNamespace(), Name: req.GetOwner().GetName()}
	return cloneidentity.InstanceID(owner, fmt.Sprintf(seedFallbackIDFormat, vmid))
}

// createSeedMetaData renders the meta-data of the seed of a VM made by req:
// the request's meta-data (when it has one) with its instance-id set to
// createSeedInstanceID and its local-hostname to the VM name, through the
// YAML encoder (cloneidentity.RewriteMetaData); every other key of the
// request's meta-data is kept. A meta-data that is not a mapping is an
// InvalidSpec error that never repeats it.
func createSeedMetaData(req *providerv1.CreateRequest, vmid int) ([]byte, error) {
	if len(req.MetaData) > maxSeedFileBytes {
		return nil, errors.NewInvalidSpec("%s", seedMetaDataTooLargeMessage)
	}
	meta, err := cloneidentity.RewriteMetaData(string(req.MetaData), createSeedInstanceID(req, vmid), req.Name)
	if err != nil {
		return nil, errors.NewInvalidSpec("%s", seedMetaDataInvalidMessage)
	}
	return []byte(meta), nil
}

// checkCreateSeedRequest checks, before anything is created, what the seed
// of a Create with user-data needs from the request: bounded user-data and a
// meta-data that renders.
func checkCreateSeedRequest(req *providerv1.CreateRequest, vmid int) error {
	if len(req.UserData) > maxSeedFileBytes {
		return errors.NewInvalidSpec("%s", seedUserDataTooLargeMessage)
	}
	_, err := createSeedMetaData(req, vmid)
	return err
}

// checkSeedPrerequisites returns an InvalidSpec error naming the missing
// configuration when the provider cannot build a seed: no SSH data plane, no
// SSH credentials, or host-key verification on without a known_hosts file.
// The details (host, path) go to the log only.
func (p *Provider) checkSeedPrerequisites() error {
	if p.ssh == nil {
		return errors.NewInvalidSpec("%s", seedNoSSHMessage)
	}
	if !p.ssh.usePassword() && !p.ssh.useKey() {
		return errors.NewInvalidSpec("%s", seedNoSSHCredentialsMessage)
	}
	if err := p.ssh.hostKey.verifyKnownHostsPresent(p.ssh.host); err != nil {
		p.logger.Error("Cannot build a cloud-init seed", "error", err)
		return errors.NewInvalidSpec("%s", seedNoKnownHostsMessage)
	}
	return nil
}

// seedPreflightCommand exits seedExitNoGenisoimage when genisoimage is not
// installed on the node, and prints the node's name otherwise.
var seedPreflightCommand = fmt.Sprintf("command -v genisoimage >/dev/null 2>&1 || exit %d; uname -n", seedExitNoGenisoimage)

// seedPreflight checks over SSH, before anything is created, that the node
// the SSH data plane reaches can build the seed of a VM that will be on
// node: genisoimage is installed, and it is that node (PVE names a node by
// its host name without the domain). A missing genisoimage or another node
// is a definite InvalidSpec error; an SSH failure is CREATE_NOT_PERFORMED.
func (p *Provider) seedPreflight(ctx context.Context, node string) error {
	if p.ssh == nil {
		return errors.NewInvalidSpec("%s", seedNoSSHMessage)
	}
	stdout, _, err := p.ssh.runSSH(ctx, seedPreflightCommand)
	if err != nil {
		if sshExitCode(err) == seedExitNoGenisoimage {
			return errors.NewInvalidSpec("%s", seedNoGenisoimageMessage)
		}
		return p.createNotPerformed("cannot reach the PVE node over SSH to build the cloud-init seed; nothing was created (will retry)", err)
	}
	name, _, _ := strings.Cut(strings.TrimSpace(stdout), ".")
	if node != "" && name != node {
		p.logger.Error("The SSH data plane reaches another PVE node than the VM's", "ssh_node", name, "vm_node", node)
		return errors.NewInvalidSpec("%s", seedNotOnNodeMessage)
	}
	return nil
}

// sshExitCode is the exit status of a remote command err reports, or -1.
func sshExitCode(err error) int {
	var exitErr *exec.ExitError
	if stderrors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// nocloudNetwork is a NoCloud network-config, version 2.
type nocloudNetwork struct {
	Version   int                        `json:"version"`
	Ethernets map[string]nocloudEthernet `json:"ethernets"`
}

// nocloudEthernet is one NIC of a nocloudNetwork: matched by its MAC and
// named eth<N> (N the PVE netN index), as PVE's own nocloud network data
// names it.
type nocloudEthernet struct {
	Match       nocloudMatch        `json:"match"`
	SetName     string              `json:"set-name"`
	DHCP4       bool                `json:"dhcp4,omitempty"`
	Addresses   []string            `json:"addresses,omitempty"`
	Routes      []nocloudRoute      `json:"routes,omitempty"`
	Nameservers *nocloudNameservers `json:"nameservers,omitempty"`
}

// nocloudMatch matches a NIC by MAC address.
type nocloudMatch struct {
	MACAddress string `json:"macaddress"`
}

// nocloudRoute is a route of a NIC.
type nocloudRoute struct {
	To  string `json:"to"`
	Via string `json:"via"`
}

// nocloudNameservers are the DNS servers of a NIC.
type nocloudNameservers struct {
	Addresses []string `json:"addresses"`
}

// renderNetworkConfig renders the NoCloud network-config (version 2) for the
// IP settings ipConfigs (one per network attachment, as Create would turn
// them into ipconfigN) of a VM whose config cfg shows its NICs. Each entry
// matches the MAC of NIC netN and names it eth<N>; a DHCP entry gets dhcp4, a
// static one its address and a default route through its gateway. The DNS
// servers (PVE's VM-wide nameserver) are given to every NIC. An entry whose
// NIC the config does not have (or that has no MAC) is skipped and returned
// in skipped. It returns nil when no entry remains: the seed then has no
// network-config, and cloud-init falls back to DHCP on the first NIC.
func renderNetworkConfig(ipConfigs []pveapi.IPConfig, cfg map[string]interface{}) (out []byte, skipped []int, err error) {
	var servers []string
	if ns := pveapi.NameserverValue(ipConfigs); ns != "" {
		servers = strings.Fields(ns)
	}
	doc := nocloudNetwork{Version: 2, Ethernets: map[string]nocloudEthernet{}}
	for _, ipc := range ipConfigs {
		value, _ := cfg[fmt.Sprintf("%s%d", pveNetKeyPrefix, ipc.Index)].(string)
		mac := strings.ToLower(parseNetValue(value).mac)
		if _, ok := normaliseMAC(mac); value == "" || !ok {
			skipped = append(skipped, ipc.Index)
			continue
		}
		name := fmt.Sprintf("eth%d", ipc.Index)
		eth := nocloudEthernet{Match: nocloudMatch{MACAddress: mac}, SetName: name}
		switch {
		case ipc.DHCP:
			eth.DHCP4 = true
		case ipc.IP != "":
			eth.Addresses = []string{ipc.IP}
			if ipc.Gateway != "" {
				to := "0.0.0.0/0"
				if strings.Contains(ipc.IP, ":") {
					to = "::/0"
				}
				eth.Routes = []nocloudRoute{{To: to, Via: ipc.Gateway}}
			}
		default:
			skipped = append(skipped, ipc.Index)
			continue
		}
		if len(servers) > 0 {
			eth.Nameservers = &nocloudNameservers{Addresses: servers}
		}
		doc.Ethernets[name] = eth
	}
	if len(doc.Ethernets) == 0 {
		return nil, skipped, nil
	}
	out, err = yaml.Marshal(doc)
	if err != nil {
		return nil, skipped, fmt.Errorf("render the cloud-init network-config: %w", err)
	}
	return out, skipped, nil
}

// dhcpIPConfigs is one DHCP IP setting per NIC of cfg (net0, net1, ...), in
// index order: a clone's seed network-config.
func dhcpIPConfigs(cfg map[string]interface{}) []pveapi.IPConfig {
	var out []pveapi.IPConfig
	for key := range cfg {
		if index, ok := netKeyIndex(key); ok {
			out = append(out, pveapi.IPConfig{Index: index, DHCP: true})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}

// seedArchive packs a seed's files into the tar archive the seed script
// extracts: regular files, mode 0600, fixed owner and time, and no
// network-config entry when the seed has none.
func seedArchive(content seedContent) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := []struct {
		name string
		data []byte
	}{
		{seedFileUserData, content.userData},
		{seedFileMetaData, content.metaData},
		{seedFileNetworkConfig, content.networkConfig},
	}
	for _, f := range files {
		if f.data == nil && f.name == seedFileNetworkConfig {
			continue
		}
		hdr := &tar.Header{
			Name:     f.name,
			Mode:     0o600,
			Size:     int64(len(f.data)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("pack the cloud-init seed: %w", err)
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, fmt.Errorf("pack the cloud-init seed: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("pack the cloud-init seed: %w", err)
	}
	return buf.Bytes(), nil
}

// seedUnusedKeys is the shell fragment that prints the unusedN keys of the
// config in "$C", one per line.
const seedUnusedKeys = `printf '%s\n' "$C" | sed -n 's/^\(unused[0-9][0-9]*\):.*/\1/p'`

// seedLockName is the name, in the stage directory, of the lock a seed build
// of VM vmid holds for its whole run.
func seedLockName(vmid int) string {
	return ".lock-" + strconv.Itoa(vmid)
}

// seedLockPath is the node path of VM vmid's seed build lock.
func seedLockPath(vmid int) string {
	return seedStageDir + "/" + seedLockName(vmid)
}

// seedLockCleanupTimeout bounds removeSeedLock's SSH session.
const seedLockCleanupTimeout = 30 * time.Second

// seedLockRemoveScript is the node-side script that removes VM vmid's seed
// build lock file from stageDir (a 0-byte file on a tmpfs that outlives the
// build) once the VM is gone. It removes the file only while it holds the
// lock on it and the file it locked is still the one at the path, so it never
// removes a lock a build holds, and a build that opened the file before it was
// removed finds, once it gets the lock, that its file is no longer the one at
// the path and stops as busy (buildSeedScript): two builds can never each
// hold a lock on a different file. A missing file, a symbolic link or a held
// lock is left alone and the script exits 0. stageDir is a parameter only so
// the tests can run it against a local directory; production passes
// seedStageDir.
func seedLockRemoveScript(stageDir string, vmid int) string {
	qd := proxmoxShellQuote(stageDir)
	ql := proxmoxShellQuote(stageDir + "/" + seedLockName(vmid))
	return "[ ! -L " + qd + " ] && [ -f " + ql + " ] && [ ! -L " + ql + " ] || exit 0; " +
		"exec 9<" + ql + "; flock -n 9 || exit 0; [ /dev/fd/9 -ef " + ql + " ] || exit 0; rm -f -- " + ql
}

// removeSeedLock removes, best effort, VM vmid's seed build lock file from
// the node the SSH data plane reaches (where every seed is built), once the
// VM is destroyed. A failure is logged, never returned: the file is empty, on
// a tmpfs, and harmless. Without SSH credentials nothing is done.
func (p *Provider) removeSeedLock(ctx context.Context, vmid int) {
	if p.ssh == nil || (!p.ssh.usePassword() && !p.ssh.useKey()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seedLockCleanupTimeout)
	defer cancel()
	if _, _, err := p.ssh.runSSH(ctx, seedLockRemoveScript(seedStageDir, vmid)); err != nil {
		p.logger.Warn("Could not remove the VM's cloud-init seed build lock file on the node (an empty file on /run, removed at reboot)",
			"vmid", vmid, "error", err)
	}
}

// pveQemuConfDir is where the PVE node keeps the configs of its own VMs
// (/etc/pve/qemu-server is the local node's directory).
const pveQemuConfDir = "/etc/pve/qemu-server"

// buildSeedScript is the node-side script that builds a seed from the tar
// archive on its stdin and attaches it to VM vmid in slot, on storage.
// stageDir and confDir are parameters only so the tests can run it against
// local directories; production passes seedStageDir and pveQemuConfDir.
// Every value is a fixed token, a number or shell-quoted; the seed's content
// is never in it. In order:
//
//  1. The VM's config must be on this node (else exit seedExitNotOnNode: the
//     SSH data plane reaches another node than the VM's), and genisoimage
//     must be installed (else exit seedExitNoGenisoimage).
//  2. stageDir, refused when it is a symbolic link, is (re)created 0700 and
//     checked to be owned by the running user, mode 700. The script then
//     holds the VM's lock (flock on stageDir/.lock-<vmid>) until it exits:
//     another seed build of the VM still running (an interrupted earlier
//     attempt), a lock file removed between its open and its flock
//     (seedLockRemoveScript; the lock it holds would then be on a file no
//     other build opens), or a `qm importdisk` of the VM, exits
//     seedExitImportBusy (the
//     pgrep pattern is written so that it does not match this script's own
//     command line). Build directories a killed script left behind for more
//     than seedStaleMinutes are removed; a fresh build directory "$W" is made.
//     seedStageDir is on /run, a tmpfs: nothing of a build outlives a reboot.
//  3. On every exit "$W" is removed, and a seed volume imported and
//     identified but not attached is deleted from the VM (which frees it).
//  4. The archive is extracted into "$W/cidata" (no owner, no modes from
//     it), and genisoimage builds "$W/seed.iso" (volume ID cidata, Joliet and
//     Rock Ridge names, as libvirt's seed).
//  5. `qm importdisk ... --format raw`. The seed is the one unusedN key the
//     import added (read from complete `qm config` outputs, a failed read
//     failing the script), and its volume must be VM vmid's disk volume on
//     storage. Anything else exits seedExitImportUnclear without deleting
//     anything: a volume left unused is freed with the VM.
//  6. `qm set --<slot> <volume>,media=cdrom` (and `--delete alsoDelete`, a
//     PVE cloud-init drive in another slot); the volume is printed.
//
// Each qm call that fails exits with its own code (seedExitQMConfig,
// seedExitQMImport, seedExitQMSet).
func buildSeedScript(stageDir, confDir string, vmid int, storage, slot, alsoDelete string) string {
	qd := proxmoxShellQuote(stageDir)
	qs := proxmoxShellQuote(storage)
	deleteDrive := ""
	if alsoDelete != "" {
		deleteDrive = " --delete " + alsoDelete
	}
	return "set -e; umask 077; VMID=" + strconv.Itoa(vmid) + "; " +
		"[ -f " + proxmoxShellQuote(confDir) + `/"$VMID.conf" ] || ` +
		fmt.Sprintf("{ echo 'the VM is not on this node' >&2; exit %d; }; ", seedExitNotOnNode) +
		fmt.Sprintf("command -v genisoimage >/dev/null 2>&1 || { echo 'genisoimage is not installed' >&2; exit %d; }; ", seedExitNoGenisoimage) +
		"[ ! -L " + qd + " ] || exit 1; install -d -m 0700 " + qd + "; " +
		"[ ! -L " + qd + " ] && [ \"$(stat -c '%u %a' -- " + qd + ")\" = \"$(id -u) 700\" ] || exit 1; " +
		"exec 9>" + proxmoxShellQuote(stageDir+"/"+seedLockName(vmid)) + "; " +
		fmt.Sprintf("flock -n 9 || { echo 'a seed build of the VM is still running' >&2; exit %d; }; ", seedExitImportBusy) +
		"[ /dev/fd/9 -ef " + proxmoxShellQuote(stageDir+"/"+seedLockName(vmid)) + " ] || " +
		fmt.Sprintf("{ echo 'the seed build lock was removed meanwhile' >&2; exit %d; }; ", seedExitImportBusy) +
		"if pgrep -f -- " + proxmoxShellQuote(importdiskRunningPattern(vmid)) + " >/dev/null 2>&1; then " +
		fmt.Sprintf("echo 'a qm importdisk of the VM is still running' >&2; exit %d; fi; ", seedExitImportBusy) +
		"find " + qd + " -mindepth 1 -maxdepth 1 -name '.seed-*' -mmin +" + strconv.Itoa(seedStaleMinutes) +
		" -exec rm -rf -- {} + 2>/dev/null || true; " +
		"W=$(mktemp -d -p " + qd + " " + seedWorkTemplate + "); U=''; OK=''; " +
		`trap 'if [ -n "$U" ] && [ -z "$OK" ]; then qm set "$VMID" --delete "$U" >/dev/null 2>&1 || true; fi; rm -rf -- "$W"' EXIT; ` +
		"trap 'exit 1' HUP INT TERM; " +
		`mkdir -- "$W/cidata"; ` +
		`tar -x -f - -C "$W/cidata" --no-same-owner --no-same-permissions; ` +
		`genisoimage -quiet -output "$W/seed.iso" -volid cidata -joliet -rock -input-charset utf-8 "$W/cidata"; ` +
		fmt.Sprintf(`C=$(qm config "$VMID") || exit %d; B=$(`, seedExitQMConfig) + seedUnusedKeys + "); " +
		`qm importdisk "$VMID" "$W/seed.iso" ` + qs + fmt.Sprintf(" --format raw >/dev/null || exit %d; ", seedExitQMImport) +
		fmt.Sprintf(`C=$(qm config "$VMID") || exit %d; N=''; `, seedExitQMConfig) +
		"for K in $(" + seedUnusedKeys + "); do " +
		`case " $(echo $B) " in *" $K "*) ;; *) if [ -n "$N" ]; then ` +
		fmt.Sprintf("echo 'qm importdisk added more than one volume' >&2; exit %d; fi; N=$K;; esac; done; ", seedExitImportUnclear) +
		fmt.Sprintf(`[ -n "$N" ] || { echo 'qm importdisk added no volume' >&2; exit %d; }; `, seedExitImportUnclear) +
		`V=$(printf '%s\n' "$C" | sed -n "s/^$N: //p"); ` +
		`case "$V" in ` + qs + `:vm-"$VMID"-disk-[0-9]*|` + qs + `:"$VMID"/vm-"$VMID"-disk-[0-9]*) ;; ` +
		fmt.Sprintf("*) echo 'qm importdisk added an unexpected volume' >&2; exit %d;; esac; ", seedExitImportUnclear) +
		`U=$N; qm set "$VMID" --` + slot + ` "$V,media=cdrom"` + deleteDrive +
		fmt.Sprintf(" >/dev/null || exit %d; ", seedExitQMSet) + `OK=1; echo "$V"`
}

// attachSeed builds the seed content on the PVE node and attaches it to VM
// vmid on node, on storage, unless the VM already has a seed of its own (a
// re-sent Create or Clone that finds its VM interrupted: nothing is built, so
// a VM never gets a second seed). Either way the seed is then taken out of
// the VM's boot order (keepSeedOutOfBootOrder), so a re-sent request also
// finishes that step. cfg is the VM's current config. The returned error is a
// seedInFlightError when the build's outcome is unknown.
func (p *Provider) attachSeed(ctx context.Context, node string, vmid int, storage string, cfg map[string]interface{}, content seedContent) error {
	if own, _ := seedSlots(cfg, vmid); len(own) > 0 {
		p.logger.Info("The VM already has its cloud-init seed", "vmid", vmid, "slot", own[0])
		return p.keepSeedOutOfBootOrder(ctx, node, vmid)
	}
	if p.ssh == nil {
		return errors.NewInvalidSpec("%s", seedNoSSHMessage)
	}
	if err := checkRequestStorage(storage); err != nil {
		return err
	}
	slot, ok := seedSlotFor(cfg, vmid)
	if !ok {
		return errors.NewInvalidSpec("%s", seedNoSlotMessage)
	}
	// A PVE cloud-init drive in another slot (the seed took a foreign seed's)
	// goes too: two cidata sources would be undefined behaviour.
	alsoDelete := ""
	if ciSlot, _, hasDrive := pveapi.CloudInitDrive(cfg); hasDrive && ciSlot != slot {
		alsoDelete = ciSlot
	}
	archive, err := seedArchive(content)
	if err != nil {
		return err
	}
	// The audit line of every seed build: what is changed on which VM, never
	// the content.
	p.logger.Info("Building the cloud-init seed on the PVE node over SSH", "vmid", vmid, "node", node, "storage", storage, "slot", slot,
		"replaces_drive", alsoDelete, "user_data_bytes", len(content.userData), "network_config", content.networkConfig != nil)
	stdout, _, err := p.ssh.runSSHInput(ctx, archive, buildSeedScript(seedStageDir, pveQemuConfDir, vmid, storage, slot, alsoDelete))
	if err != nil {
		if sshMayHaveRun(ctx, err) || sshExitCode(err) == seedExitImportBusy {
			return &seedInFlightError{cause: err}
		}
		p.logCause("building the cloud-init seed failed", err)
		switch sshExitCode(err) {
		case seedExitNoGenisoimage:
			return errors.NewInvalidSpec("%s", seedNoGenisoimageMessage)
		case seedExitNotOnNode:
			return errors.NewInvalidSpec("%s", seedNotOnNodeMessage)
		}
		return fmt.Errorf("build and attach the cloud-init seed of VM %d: %w", vmid, err)
	}
	p.logger.Info("Attached the cloud-init seed", "vmid", vmid, "slot", slot, "volume", strings.TrimSpace(stdout))
	return p.keepSeedOutOfBootOrder(ctx, node, vmid)
}

// keepSeedOutOfBootOrder takes VM vmid's seed CD-ROM out of its boot order.
// The `qm set` that attaches the seed appends it to a non-empty order list
// (qemu-server marks every new CD-ROM bootable), so the order is set again,
// in a config update of its own, after the seed is attached
// (seedFreeBootOrder): an update that set the order and attached the CD-ROM
// together would leave either order, because qemu-server applies an update's
// parameters in no defined sequence. The seed is not bootable (no El Torito
// image); cloud-init finds it by its label.
func (p *Provider) keepSeedOutOfBootOrder(ctx context.Context, node string, vmid int) error {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read the config of VM %d to take its cloud-init seed out of the boot order: %w", vmid, err)
	}
	own, _ := seedSlots(cfg, vmid)
	boot, changed := seedFreeBootOrder(cfg, own)
	if !changed {
		return nil
	}
	p.logger.Info("Taking the cloud-init seed out of the boot order", "vmid", vmid, "slots", own, "boot", boot)
	task, err := p.client.ReconfigureVMRaw(ctx, node, vmid, url.Values{pveConfigKeyBoot: {boot}})
	if err != nil {
		return fmt.Errorf("take the cloud-init seed of VM %d out of the boot order: %w", vmid, err)
	}
	if err := p.client.WaitForTask(ctx, node, task); err != nil {
		return fmt.Errorf("take the cloud-init seed of VM %d out of the boot order: %w", vmid, err)
	}
	return nil
}
