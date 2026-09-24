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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/diskutil"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage"
)

// Clean provider implementation using only virsh

// Create creates a new VM using virsh with full cloud-init support.
//
// Topology dispatch (ADR-0007 D9): a single-host provider creates on its one
// host exactly as before — target_host_id is ignored and this path is
// byte-for-byte unchanged. A CLUSTERED provider (topology: cluster) instead
// routes the create onto the host the operator's scheduler bound this VM to,
// named by req.TargetHostID (see createClustered).
//
// On both paths a name virsh would resolve as a domain ID or UUID is rejected
// up front (InvalidSpec), before any host is touched (ambiguousDomainNameError).
func (p *Provider) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	log.Printf("INFO Creating VM with cloud-init support: %s", req.Name)

	if err := ambiguousDomainNameError(req.Name); err != nil {
		return contracts.CreateResponse{}, err
	}

	if p.clustered() {
		return p.createClustered(ctx, req)
	}

	if p.virshProvider == nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	return p.createVM(ctx, p.virshProvider, req)
}

// createVM runs the create pipeline against a single host's VirshProvider vp: an
// ownership-checked pre-check for a domain of the same name already on that host
// (bindExistingDomain: an idempotent success ONLY if that domain is stamped with
// req.Owner's UID, a non-retryable Conflict otherwise) followed by the full
// cloud-init + storage create, which stamps req.Owner onto the new domain. It is
// the shared core of both the single-host path (vp == p.virshProvider) and the
// clustered create-on-host path (vp == the leased target host's provider), so a
// clustered create is byte-for-byte the single-host create — only the host the
// commands run against differs (ADR-0007 D9) — and the ownership rule applies
// to both.
func (p *Provider) createVM(ctx context.Context, vp *VirshProvider, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	if vp == nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if domain already exists on this host
	domains, err := vp.listDomains(ctx)
	if err != nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to list existing domains", err)
	}

	for _, domain := range domains {
		if domain.Name == req.Name {
			return bindExistingDomain(ctx, vp, req, domain.State)
		}
	}

	// Create VM with cloud-init support
	vmID, err := p.createVMWithCloudInit(ctx, vp, req)
	if err != nil {
		if isInvalidArgument(err) {
			// A rejected request (e.g. a confined image path) is not retryable:
			// return it as-is so it reaches the manager as InvalidArgument.
			return contracts.CreateResponse{}, fmt.Errorf("failed to create VM: %w", err)
		}
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to create VM", err)
	}

	log.Printf("INFO Successfully created VM: %s with ID: %s", req.Name, vmID)
	return contracts.CreateResponse{
		ID: vmID,
	}, nil
}

// createClustered routes a create onto the specific host the operator's
// scheduler bound this VM to (ADR-0007 P1, D4); req.TargetHostID names it.
//
//   - An EMPTY target_host_id is a clean typed error, never a silent
//     default-host create: a clustered provider has no single default host, and
//     the operator must schedule the VM (binding PR 2) before Create
//     (honesty-first, D9).
//   - Otherwise it borrows that host's connection LEASE from the N-host registry
//     (ConnFor) and ALWAYS releases it (defer Close). The lease is non-severing:
//     a concurrent host-remove drains gracefully behind the held lease and its
//     underlying connection is closed only once idle — the same lease discipline
//     GetHostInfo's collectOneHost uses (#318). The create then runs on that
//     host's libvirtd through the leased connection's VirshProvider.
func (p *Provider) createClustered(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	hostID := strings.TrimSpace(req.TargetHostID)
	if hostID == "" {
		return contracts.CreateResponse{}, contracts.NewInvalidSpecError(
			"clustered libvirt provider requires target_host_id: the operator must schedule the VM to a host before Create (ADR-0007 P1)", nil)
	}

	lease, err := p.clusterReg.ConnFor(ctx, hostconn.HostID(hostID))
	if err != nil {
		// Unknown host, a host being drained, or a failed lazy dial. A retryable,
		// HOST-scoped unavailability (ADR-0007 Addendum A, A1): the mounted
		// inventory may still be reconciling, or the host may recover. The
		// operator keeps the VM on this pending host rather than re-scheduling
		// it (A2), and the manager's circuit breaker does not count it.
		return contracts.CreateResponse{}, contracts.NewHostUnavailableError(
			fmt.Sprintf("connect to target host %q", hostID), err)
	}
	// Release the lease on every exit path (success, create error, or panic).
	// Close releases the per-borrow lease; it does NOT close the shared
	// underlying connection, so a drain that removed this host mid-create
	// completes cleanly the moment we return (ADR-0007 D3 non-severing).
	defer func() { _ = lease.Close() }()

	fn := p.createOnHostFn
	if fn == nil {
		fn = p.createOnLeasedHost
	}
	return fn(ctx, lease, req)
}

// createOnLeasedHost runs the create pipeline over an already-leased host
// connection — the production createOnHostFn. It narrows the lease to the
// host's *virshConn to reach that host's VirshProvider, then runs the same
// createVM core the single-host path uses, so a clustered create is byte-for-
// byte the single-host create aimed at the chosen libvirtd (ADR-0007 D9).
//
// It is reached through the p.createOnHostFn seam so clustered-routing tests can
// inject a recorder (asserting host selection and lease release) without a live
// libvirtd, mirroring the describeNativeFn/listNativeFn test seams.
func (p *Provider) createOnLeasedHost(ctx context.Context, lease hostconn.Conn, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	vc, err := virshConnFrom(lease)
	if err != nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("resolve target host connection", err)
	}
	return p.createVM(ctx, vc.virsh, req)
}

// virshConnFrom narrows a hostconn.Conn — possibly a registry lease wrapping the
// real connection — to the underlying *virshConn, so the clustered create path
// can reach the target host's VirshProvider. A ClusterRegistry hands out a lease
// that Unwraps to the *virshConn; a bare *virshConn is returned as-is. It errors
// (rather than nil-derefs) if the connection is not virsh-backed.
func virshConnFrom(c hostconn.Conn) (*virshConn, error) {
	// Bounded unwrap: a lease wraps the conn exactly once today, but tolerate a
	// small chain rather than assume a single layer.
	for i := 0; i < 8; i++ {
		if vc, ok := c.(*virshConn); ok {
			return vc, nil
		}
		u, ok := c.(interface{ Unwrap() hostconn.Conn })
		if !ok {
			break
		}
		next := u.Unwrap()
		if next == nil || next == c {
			break
		}
		c = next
	}
	return nil, fmt.Errorf("target host connection is not virsh-backed (%T)", c)
}

// createVMWithCloudInit creates a VM with comprehensive cloud-init support and
// storage management on the host backed by vp. vp is p.virshProvider in
// single-host mode and the leased target host's provider in clustered mode
// (ADR-0007 P1), so the whole pipeline — storage, cloud-init, domain define —
// runs against the intended libvirtd.
func (p *Provider) createVMWithCloudInit(ctx context.Context, vp *VirshProvider, req contracts.CreateRequest) (string, error) {
	log.Printf("INFO Creating VM with enhanced cloud-init configuration and storage: %s", req.Name)

	// Initialize providers
	cloudInitProvider := NewCloudInitProvider(vp)
	storageProvider := NewStorageProvider(vp)

	// Ensure default storage pool exists and is active
	if err := storageProvider.EnsureDefaultStoragePool(ctx); err != nil {
		return "", fmt.Errorf("failed to ensure storage pool: %w", err)
	}

	// Create disk image from template or create empty disk
	diskVolumeName := vmDiskVolumeName(req.Name)
	var diskPath string

	// Get disk size from VMClass (default to 20GB if not specified)
	diskSizeGB := p.extractDiskSize(req)
	log.Printf("INFO Using disk size: %dGB", diskSizeGB)

	// Check if VMImage is specified in the request
	if imageSpec := p.extractImageSpec(req); imageSpec != "" {
		log.Printf("INFO Creating disk from image: %q", imageSpec)

		var volume *StorageVolume
		var err error

		// Determine how to handle the image based on its type
		if req.Image.Path == "" && (strings.HasPrefix(imageSpec, "http://") || strings.HasPrefix(imageSpec, "https://")) {
			// Handle URL - download the image
			log.Printf("INFO Downloading cloud image from URL: %s", imageSpec)
			volume, err = storageProvider.DownloadCloudImage(ctx, imageSpec, diskVolumeName, defaultStoragePool, diskSizeGB)
		} else if req.Image.Path != "" || strings.HasPrefix(imageSpec, "/") {
			// A host path — from VMImage.spec.source.libvirt.path, a prepared
			// image, spec.importedDisk.path, or anything else shaped like a
			// path. It is user-controlled, so it is confined on THIS host (vp:
			// the leased target host in clustered mode) before any use.
			volume, err = p.createDiskFromHostImage(ctx, vp, storageProvider, req, imageSpec, diskVolumeName, diskSizeGB)
		} else {
			// Handle template name - look up in predefined templates
			log.Printf("INFO Creating disk from predefined template: %s", imageSpec)
			volume, err = storageProvider.CreateVolumeFromTemplate(ctx, imageSpec, diskVolumeName, "default", diskSizeGB)
		}

		if err != nil {
			return "", fmt.Errorf("failed to create disk from image: %w", err)
		}
		diskPath = volume.Path
	} else {
		// Create empty disk volume
		log.Printf("INFO Creating empty disk volume: %s", diskVolumeName)
		volume, err := storageProvider.CreateVolume(ctx, "default", diskVolumeName, "qcow2", diskSizeGB)
		if err != nil {
			return "", fmt.Errorf("failed to create disk volume: %w", err)
		}
		diskPath = volume.Path
	}

	// Prepare cloud-init if provided
	var cloudInitISOPath string
	if req.UserData != nil && req.UserData.CloudInitData != "" {
		log.Printf("INFO Preparing cloud-init configuration for VM: %s", req.Name)

		// Extract hostname from cloud-init data
		hostname := cloudInitProvider.ExtractHostnameFromCloudInit(req.UserData.CloudInitData)
		if hostname == "" {
			hostname = req.Name // fallback to VM name
		}

		// Validate cloud-init data
		if err := cloudInitProvider.ValidateCloudInitData(req.UserData.CloudInitData); err != nil {
			return "", fmt.Errorf("invalid cloud-init data: %w", err)
		}

		// Prepare cloud-init configuration
		cloudInitConfig := CloudInitConfig{
			UserData:   req.UserData.CloudInitData,
			InstanceID: req.Name,
			Hostname:   hostname,
		}

		var err error
		cloudInitISOPath, err = cloudInitProvider.PrepareCloudInit(ctx, cloudInitConfig)
		if err != nil {
			return "", fmt.Errorf("failed to prepare cloud-init: %w", err)
		}

		// Cleanup cloud-init files when done (defer)
		defer func() {
			if cleanupErr := cloudInitProvider.CleanupCloudInit(req.Name); cleanupErr != nil {
				log.Printf("WARN Failed to cleanup cloud-init files: %v", cleanupErr)
			}
		}()
	} else {
		// No user-data supplied: apply the minimal, credential-free default
		// (hostname + qemu-guest-agent only; see generateDefaultCloudInit).
		log.Printf("INFO Generating default cloud-init configuration for VM: %s", req.Name)

		defaultCloudInit := p.generateDefaultCloudInit(req.Name)
		cloudInitConfig := CloudInitConfig{
			UserData:   defaultCloudInit,
			InstanceID: req.Name,
			Hostname:   req.Name,
		}

		var err error
		cloudInitISOPath, err = cloudInitProvider.PrepareCloudInit(ctx, cloudInitConfig)
		if err != nil {
			log.Printf("WARN Failed to prepare default cloud-init: %v", err)
			// Continue without cloud-init
		} else {
			// Cleanup cloud-init files when done (defer)
			defer func() {
				if cleanupErr := cloudInitProvider.CleanupCloudInit(req.Name); cleanupErr != nil {
					log.Printf("WARN Failed to cleanup cloud-init files: %v", cleanupErr)
				}
			}()
		}
	}

	// Generate domain XML with proper disk and cloud-init ISO
	domainXML, err := p.generateDomainXMLWithStorage(ctx, vp, req, diskPath, cloudInitISOPath)
	if err != nil {
		return "", fmt.Errorf("failed to generate domain XML: %w", err)
	}

	// Create domain definition file
	if err := p.createDomainDefinition(ctx, vp, req.Name, domainXML); err != nil {
		return "", fmt.Errorf("failed to create domain definition: %w", err)
	}

	// Define the domain in libvirt
	if err := p.defineDomain(ctx, vp, req.Name); err != nil {
		return "", fmt.Errorf("failed to define domain: %w", err)
	}

	log.Printf("INFO Successfully created VM with storage and cloud-init: %s", req.Name)
	return req.Name, nil
}

// createDiskFromHostImage builds the VM's primary disk from a host path image
// (imagePath: VMImage.spec.source.libvirt.path, a prepared image, or
// VirtualMachine.spec.importedDisk.path). The path is user-controlled, so it is
// first confined ON THE HOST behind vp — the host the VM is being created on
// (see imagepath.go) — and only the resulting canonical path is used:
//
//   - a base image is COPIED (qemu-img convert, with the probed source format)
//     into the VM's own <vm>-disk.qcow2; it is never attached in place, so two
//     VMs never share one disk and deleting a VM never deletes the image;
//   - only this VM's own imported migration disk (<vm>-migrated.qcow2 in the
//     pool directory, unused by any domain) is attached in place.
//
// A rejected path returns an InvalidArgument error (non-retryable).
func (p *Provider) createDiskFromHostImage(ctx context.Context, vp *VirshProvider, sp *StorageProvider,
	req contracts.CreateRequest, imagePath, volumeName string, sizeGB int) (*StorageVolume, error) {
	policy, err := p.imagePolicy()
	if err != nil {
		return nil, err
	}

	confReq := imagePathRequest{Path: imagePath, ImportedDisk: req.Image.ImportedDisk, VMName: req.Name}
	if req.Image.ImportedDisk {
		poolInfo, err := sp.GetPoolInfo(ctx, defaultStoragePool)
		if err != nil {
			return nil, fmt.Errorf("get storage pool %q info: %w", defaultStoragePool, err)
		}
		confReq.PoolDir = poolInfo.Path
	}

	img, err := policy.confine(ctx, vp, confReq)
	if err != nil {
		return nil, err
	}
	if img.AdoptInPlace {
		log.Printf("INFO Attaching imported disk %q in place for VM %s", img.Path, req.Name)
		return sp.adoptVolumeInPlace(ctx, img.Path, defaultStoragePool)
	}
	log.Printf("INFO Copying base image %q (%s) into the disk of VM %s", img.Path, img.Format, req.Name)
	return sp.CopyImageToVolume(ctx, img.Path, img.Format, volumeName, defaultStoragePool, sizeGB)
}

// Delete removes a VM using virsh and cleans up all associated resources.
//
// Topology dispatch (ADR-0007 Addendum A, A1/A2):
//
//   - single-host: the delete runs on p.virshProvider exactly as before —
//     vm.HostID and vm.Owner are ignored, so a legacy unstamped domain stays
//     deletable;
//   - clustered: the delete is routed to vm.HostID (withHostConn) and is
//     OWNER-CHECKED against vm.Owner: a domain whose owner stamp is missing or
//     records another owner is reported not-found and never destroyed
//     (deleteClustered).
func (p *Provider) Delete(ctx context.Context, vm contracts.VMRef) (taskRef string, err error) {
	id := vm.ID
	log.Printf("INFO Deleting VM and all associated resources: %s", id)

	if p.clustered() {
		return "", p.withHostConn(ctx, vm.HostID, func(c libvirtConn) error {
			return p.deleteClustered(ctx, c, id, vm.Owner)
		})
	}

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	return p.deleteOn(ctx, p.singleHostConn(), id)
}

// deleteOn is the single-host delete core, run on connection c: a domain that
// does not exist is treated as deleted after a best-effort cleanup of files
// named after it; an existing one is torn down by deleteExistingDomain. The
// virsh/host command sequence is the historical one, unchanged.
func (p *Provider) deleteOn(ctx context.Context, c libvirtConn, id string) (string, error) {
	vp, err := virshOf(c)
	if err != nil {
		return "", err
	}

	// Check if domain exists
	domains, err := vp.listDomains(ctx)
	if err != nil {
		return "", contracts.NewRetryableError("failed to list domains", err)
	}

	domainExists := false
	for _, domain := range domains {
		if domain.Name == id {
			domainExists = true
			break
		}
	}

	if !domainExists {
		log.Printf("INFO Domain %s does not exist, cleaning up any remaining resources", id)
		// Even if domain doesn't exist, try to clean up orphaned resources
		p.cleanupOrphanedResources(ctx, vp, id)
		return "", nil
	}

	return p.deleteExistingDomain(ctx, vp, id)
}

// deleteClustered is the OWNER-CHECKED delete core of a clustered provider
// (ADR-0007 Addendum A, A2), run on the leased connection of the VM's host.
// It destroys the domain only when its VirtRigaud owner stamp (#333) records
// owner's UID. A domain that is absent, unstamped, unreadable or stamped with
// another owner is reported as a NotFound error — the manager treats it as
// already gone — and is NEVER touched, so a cleanup (e.g. the finalizer after
// an ALREADY_EXISTS create on a pending host) can never delete another
// tenant's domain. For the same reason no name-pattern orphan cleanup runs:
// files named after an absent domain cannot be proven to be this VM's.
//
// Like Power and Reconfigure (slice 2), the teardown addresses the checked
// domain by its UUID (ownedDomainTarget), so a domain replaced between the
// ownership check and the destroy/undefine is never torn down; an owned domain
// without a canonical UUID is a retryable error and is left alone.
func (p *Provider) deleteClustered(ctx context.Context, c libvirtConn, id string, owner contracts.ObjectIdentity) error {
	vp, err := virshOf(c)
	if err != nil {
		return err
	}
	host := c.HostID()

	d, err := ownedDomainTarget(ctx, vp, host, id, owner, "delete")
	if err != nil {
		if contracts.IsNotFound(err) {
			log.Printf("INFO Domain %s is not deletable by this VirtualMachine on host %s (absent or not owned); nothing was deleted "+
				"(no name-based orphan cleanup on a clustered host)", id, host)
		}
		return err
	}

	_, err = p.deleteExistingDomain(ctx, vp, d.handle)
	return err
}

// domainOwnership is what checkDomainOwner found about a domain on one host.
type domainOwnership struct {
	// present reports whether a domain of the requested name exists on the host.
	present bool
	// owned reports whether its VirtRigaud owner stamp (#333) records the
	// requester's UID (requesterOwnsDomain). Always false when !present.
	owned bool
	// uuid is the owned domain's libvirt UUID, read from the same document as
	// the stamp, so a later read can prove it still addresses THAT domain.
	uuid string
}

// checkDomainOwner is the ownership gate of every routed per-VM call on a
// clustered host (ADR-0007 Addendum A): it lists the host's domains and, when
// id exists, reads its owner stamp and decides with requesterOwnsDomain — which
// fails closed on a missing, unreadable, ambiguous or foreign stamp, and on a
// requester without a UID. Only read-only virsh queries run. A read failure is
// a retryable error, never a decision. op names the call for the operator log;
// the refusal itself is the caller's, with a message that never names the
// other owner.
func checkDomainOwner(ctx context.Context, vp *VirshProvider, host hostconn.HostID, id string, owner contracts.ObjectIdentity, op string) (domainOwnership, error) {
	domains, err := vp.listDomains(ctx)
	if err != nil {
		return domainOwnership{}, contracts.NewRetryableError("failed to list domains", err)
	}
	found := false
	for _, d := range domains {
		if d.Name == id {
			found = true
			break
		}
	}
	if !found {
		return domainOwnership{}, nil
	}

	res, err := vp.runVirshCommand(ctx, "dumpxml", id)
	if err != nil {
		return domainOwnership{}, contracts.NewRetryableError(fmt.Sprintf("read owner metadata of domain %q", id), err)
	}
	recorded, perr := domainOwners(res.Stdout)
	if perr == nil && requesterOwnsDomain(owner, recorded) {
		own := domainOwnership{present: true, owned: true}
		if d, derr := parseDomainLibvirtxml(res.Stdout); derr == nil {
			own.uuid = strings.TrimSpace(d.UUID)
		}
		return own, nil
	}

	switch {
	case perr != nil:
		log.Printf("WARN Refusing %s of domain %s on host %s: its owner metadata could not be read: %v", op, id, host, perr)
	case owner.IsZero():
		log.Printf("WARN Refusing %s of domain %s on host %s: the request carries no owner UID", op, id, host)
	case len(recorded) == 0:
		log.Printf("WARN Refusing %s of domain %s on host %s for %s/%s (uid %s): it has no VirtRigaud owner metadata",
			op, id, host, owner.Namespace, owner.Name, owner.UID)
	default:
		log.Printf("WARN Refusing %s of domain %s on host %s for %s/%s (uid %s): it is owned by %v",
			op, id, host, owner.Namespace, owner.Name, owner.UID, recorded)
	}
	return domainOwnership{present: true}, nil
}

// deleteExistingDomain tears down an existing domain on vp's host: it records
// the domain's disks and cloud-init ISO, force-stops and undefines it, then
// removes those files. Shared by the single-host and the owner-checked
// clustered delete.
func (p *Provider) deleteExistingDomain(ctx context.Context, vp *VirshProvider, id string) (string, error) {
	// Get disk paths before deleting the domain
	diskPaths, err := domainDiskPaths(ctx, vp, id)
	if err != nil {
		log.Printf("WARN Failed to get disk paths for %s: %v", id, err)
		// Continue with deletion even if we can't get disk paths
	}

	// Get cloud-init ISO path before deleting the domain
	cloudInitISOPath, err := getCloudInitISOPath(ctx, vp, id)
	if err != nil {
		log.Printf("WARN Failed to get cloud-init ISO path for %s: %v", id, err)
		// Continue with deletion
	}

	// Stop the domain if running
	if err := vp.destroyDomain(ctx, id); err != nil {
		log.Printf("WARN Failed to destroy domain %s: %v", id, err)
		// Continue with undefine even if destroy fails
	}

	// Remove the domain definition (this should also remove storage if --remove-all-storage is used)
	// However, we'll explicitly delete disks to ensure cleanup
	if err := vp.undefineDomain(ctx, id); err != nil {
		return "", contracts.NewRetryableError("failed to undefine domain", err)
	}

	// Delete disk images
	if len(diskPaths) > 0 {
		log.Printf("INFO Deleting %d disk(s) for VM %s", len(diskPaths), id)
		for _, diskPath := range diskPaths {
			if err := deleteDiskFile(ctx, vp, diskPath); err != nil {
				log.Printf("WARN Failed to delete disk %s: %v", diskPath, err)
				// Continue with other deletions
			} else {
				log.Printf("INFO Successfully deleted disk: %s", diskPath)
			}
		}
	}

	// Delete cloud-init ISO
	if cloudInitISOPath != "" {
		if err := deleteCloudInitResources(ctx, vp, id, cloudInitISOPath); err != nil {
			log.Printf("WARN Failed to delete cloud-init resources: %v", err)
			// Continue - not a critical error
		} else {
			log.Printf("INFO Successfully deleted cloud-init resources for: %s", id)
		}
	}

	log.Printf("INFO Successfully deleted domain and all resources: %s", id)
	return "", nil
}

// getDomainDiskPaths retrieves all disk paths for a domain on the provider's
// single-host connection.
func (p *Provider) getDomainDiskPaths(ctx context.Context, domainName string) ([]string, error) {
	return domainDiskPaths(ctx, p.virshProvider, domainName)
}

// domainDiskPaths retrieves all disk paths for a domain on vp's host.
func domainDiskPaths(ctx context.Context, vp *VirshProvider, domainName string) ([]string, error) {
	// Get domain XML to extract disk paths
	result, err := vp.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		return nil, fmt.Errorf("failed to dump domain XML: %w", err)
	}

	var diskPaths []string

	// Parse XML to find disk source files
	// Look for lines like: <source file='/var/lib/libvirt/images/vm-disk.qcow2'/>
	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "<source file=") && !strings.Contains(line, "device='disk'") {
			// Extract the file path from <source file='...'/>
			start := strings.Index(line, "file='")
			if start == -1 {
				start = strings.Index(line, "file=\"")
			}
			if start != -1 {
				start += 6 // len("file='") or len("file=\"")
				end := strings.IndexAny(line[start:], "\"'")
				if end != -1 {
					diskPath := line[start : start+end]
					// Skip cloud-init ISOs (we'll handle those separately)
					if !strings.HasSuffix(diskPath, "-cidata.iso") && !strings.HasSuffix(diskPath, "cloud-init.iso") {
						diskPaths = append(diskPaths, diskPath)
					}
				}
			}
		}
	}

	return diskPaths, nil
}

// getCloudInitISOPath retrieves the cloud-init ISO path for a domain on vp's host.
func getCloudInitISOPath(ctx context.Context, vp *VirshProvider, domainName string) (string, error) {
	// Get domain XML
	result, err := vp.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		return "", fmt.Errorf("failed to dump domain XML: %w", err)
	}

	// Look for cloud-init ISO in XML
	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "<source file=") {
			// Extract the file path
			start := strings.Index(line, "file='")
			if start == -1 {
				start = strings.Index(line, "file=\"")
			}
			if start != -1 {
				start += 6
				end := strings.IndexAny(line[start:], "\"'")
				if end != -1 {
					filePath := line[start : start+end]
					// Check if this is a cloud-init ISO
					if strings.HasSuffix(filePath, "cloud-init.iso") || strings.Contains(filePath, "virtrigaud-cloudinit") {
						return filePath, nil
					}
				}
			}
		}
	}

	return "", nil
}

// deleteDiskFile deletes a disk file from vp's libvirt host
func deleteDiskFile(ctx context.Context, vp *VirshProvider, diskPath string) error {
	log.Printf("INFO Deleting disk file: %s", diskPath)

	// Use rm to delete the disk file
	_, err := vp.runVirshCommand(ctx, "!", "sudo", "rm", "-f", diskPath)
	if err != nil {
		return fmt.Errorf("failed to delete disk file %s: %w", diskPath, err)
	}

	return nil
}

// deleteCloudInitResources deletes cloud-init ISO and associated files on vp's host
func deleteCloudInitResources(ctx context.Context, vp *VirshProvider, domainName, isoPath string) error {
	log.Printf("INFO Deleting cloud-init resources for: %s", domainName)

	// Delete the cloud-init directory which contains ISO, user-data, and meta-data
	// Extract directory from ISO path by removing the filename
	lastSlash := strings.LastIndex(isoPath, "/")
	cloudInitDir := isoPath
	if lastSlash != -1 {
		cloudInitDir = isoPath[:lastSlash]
	}

	_, err := vp.runVirshCommand(ctx, "!", "rm", "-rf", cloudInitDir)
	if err != nil {
		return fmt.Errorf("failed to delete cloud-init directory %s: %w", cloudInitDir, err)
	}

	return nil
}

// cleanupOrphanedResources attempts to clean up any resources that might be
// left behind on vp's host (single-host delete only).
func (p *Provider) cleanupOrphanedResources(ctx context.Context, vp *VirshProvider, domainName string) {
	log.Printf("INFO Cleaning up orphaned resources for: %s", domainName)

	// Try to delete disk files with common naming patterns
	diskPatterns := []string{
		fmt.Sprintf("/var/lib/libvirt/images/%s-disk.qcow2", domainName),
		fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", domainName),
		fmt.Sprintf("/var/lib/libvirt/images/%s-disk", domainName),
	}

	for _, diskPath := range diskPatterns {
		_, err := vp.runVirshCommand(ctx, "!", "sudo", "rm", "-f", diskPath)
		if err != nil {
			log.Printf("DEBUG Could not delete potential orphaned disk %s: %v", diskPath, err)
		} else {
			log.Printf("INFO Cleaned up orphaned disk: %s", diskPath)
		}
	}

	// Try to delete cloud-init directory
	cloudInitDir := fmt.Sprintf("/tmp/virtrigaud-cloudinit/%s", domainName)
	_, err := vp.runVirshCommand(ctx, "!", "rm", "-rf", cloudInitDir)
	if err != nil {
		log.Printf("DEBUG Could not delete cloud-init directory %s: %v", cloudInitDir, err)
	} else {
		log.Printf("INFO Cleaned up orphaned cloud-init directory: %s", cloudInitDir)
	}
}

// domainTarget addresses the one domain a Power or Reconfigure core acts on
// (ADR-0007 Addendum A, slice 2).
//
// On a single-host provider both fields are the domain name the operator sent,
// so every command is byte-for-byte the historical one (byName). On a clustered
// provider handle is the UUID of the domain whose owner stamp was just checked
// (ownedDomainTarget): every virsh command then acts on exactly that domain,
// and if it were replaced by a same-named domain between the check and the
// action, the UUID would no longer resolve and the command would fail instead
// of acting on another tenant's VM.
type domainTarget struct {
	// handle is what every virsh command addresses the domain by: its name
	// (single-host) or its owner-checked UUID (clustered).
	handle string
	// name is the domain name. It keys log lines and, on a single-host
	// provider only, the name-derived "<name>-disk" volume Reconfigure resizes.
	name string
	// diskByPath makes Reconfigure resize the domain's own primary disk, read
	// from the domain (domblklist --details) and addressed by its path, instead
	// of the "<name>-disk" volume found by name. Set for the owner-checked
	// clustered target; false on single-host, whose resize is unchanged.
	diskByPath bool
}

// byName is the single-host target: the domain addressed by its name, exactly
// as before routing.
func byName(id string) domainTarget { return domainTarget{handle: id, name: id} }

// canonicalUUIDRE matches the canonical dashed UUID form `virsh dumpxml`
// prints. ownedDomainTarget refuses to address a domain by anything else.
var canonicalUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ownedDomainTarget is the ownership gate of a routed, MUTATING per-VM call
// (Power, Reconfigure) on a clustered host (ADR-0007 Addendum A, slice 2). It
// checks the owner stamp of domain id against owner (checkDomainOwner, which
// fails closed) BEFORE anything is changed, and returns the domain addressed by
// the UUID read together with that stamp.
//
//   - absent, or a stamp that is missing, unreadable, ambiguous or records
//     another owner (or a request without an owner): a NotFound error, and the
//     domain is never touched. The message never names the other owner.
//   - owned, but with no canonical UUID in its definition: a retryable error —
//     the domain cannot be pinned, so nothing is done.
//
// op names the call for the refusal message and the operator log.
func ownedDomainTarget(ctx context.Context, vp *VirshProvider, host hostconn.HostID, id string, owner contracts.ObjectIdentity, op string) (domainTarget, error) {
	own, err := checkDomainOwner(ctx, vp, host, id, owner, op)
	if err != nil {
		return domainTarget{}, err
	}
	switch {
	case !own.present:
		return domainTarget{}, contracts.NewNotFoundError(fmt.Sprintf("libvirt domain %q not found on host %s", id, host), nil)
	case !own.owned:
		// Uniform message: it reaches the requesting VM's status, so it must not
		// disclose which other VirtualMachine (if any) owns the domain.
		return domainTarget{}, contracts.NewNotFoundError(fmt.Sprintf(
			"libvirt domain %q on host %s is not owned by this VirtualMachine; %s was not performed", id, host, op), nil)
	case !canonicalUUIDRE.MatchString(own.uuid):
		return domainTarget{}, contracts.NewRetryableError(
			fmt.Sprintf("cannot verify the identity of domain %q on host %s (no UUID in its definition)", id, host), nil)
	}
	return domainTarget{handle: own.uuid, name: id, diskByPath: true}, nil
}

// unsupportedPowerOpError is the InvalidSpec answer to a power operation the
// provider does not implement.
func unsupportedPowerOpError(op contracts.PowerOp) error {
	return contracts.NewInvalidSpecError(fmt.Sprintf("unsupported power operation: %s", op), nil)
}

// knownPowerOp reports whether runPowerOp implements op.
func knownPowerOp(op contracts.PowerOp) bool {
	switch op {
	case contracts.PowerOpOn, contracts.PowerOpOff, contracts.PowerOpReboot, contracts.PowerOpShutdownGraceful:
		return true
	}
	return false
}

// Power controls VM power state using virsh.
//
// Topology dispatch (ADR-0007 Addendum A, slice 2):
//
//   - single-host: the operation runs on p.virshProvider exactly as before —
//     vm.HostID and vm.Owner are ignored;
//   - clustered: an unknown op is refused before any host is touched; otherwise
//     the call is routed to vm.HostID (withHostConn) and is OWNER-CHECKED
//     against vm.Owner before the domain is started, stopped or rebooted
//     (powerClustered).
func (p *Provider) Power(ctx context.Context, vm contracts.VMRef, op contracts.PowerOp) (taskRef string, err error) {
	id := vm.ID
	log.Printf("INFO Power operation %s on VM: %s", op, id)

	if p.clustered() {
		if !knownPowerOp(op) {
			return "", unsupportedPowerOpError(op)
		}
		return "", p.withHostConn(ctx, vm.HostID, func(c libvirtConn) error {
			return p.powerClustered(ctx, c, id, vm.Owner, op)
		})
	}

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	return "", p.runPowerOp(ctx, p.singleHostConn(), byName(id), op)
}

// powerClustered is the routed, OWNER-CHECKED Power of a clustered VM on its
// bound host's leased connection c: the operation runs only on a domain whose
// owner stamp records owner's UID, addressed by its UUID (ownedDomainTarget).
func (p *Provider) powerClustered(ctx context.Context, c libvirtConn, id string, owner contracts.ObjectIdentity, op contracts.PowerOp) error {
	vp, err := virshOf(c)
	if err != nil {
		return err
	}
	d, err := ownedDomainTarget(ctx, vp, c.HostID(), id, owner, fmt.Sprintf("power operation %s", op))
	if err != nil {
		return err
	}
	return p.runPowerOp(ctx, c, d, op)
}

// runPowerOp is the Power core, run on connection c — p.virshProvider's
// connection in single-host mode, the leased host connection in clustered
// mode — against domain d. The virsh/host command sequence is the historical
// one, unchanged.
func (p *Provider) runPowerOp(ctx context.Context, c libvirtConn, d domainTarget, op contracts.PowerOp) error {
	vp, err := virshOf(c)
	if err != nil {
		return err
	}
	id := d.handle

	switch op {
	case contracts.PowerOpOn:
		err = vp.startDomain(ctx, id)
		// After starting, sync the persistent XML to match the running state
		// This prevents "pending changes" in Cockpit by ensuring the persistent
		// definition matches what libvirt expanded (e.g., CPU features)
		if err == nil {
			if syncErr := syncPersistentXML(ctx, vp, id); syncErr != nil {
				log.Printf("WARN Failed to sync persistent XML for %s: %v", id, syncErr)
				// Don't fail the power on operation for this
			}
		}
	case contracts.PowerOpOff:
		err = vp.stopDomain(ctx, id)
	case contracts.PowerOpReboot:
		// Restart by stopping then starting
		if stopErr := vp.stopDomain(ctx, id); stopErr != nil {
			log.Printf("WARN Failed to stop domain for reboot: %v", stopErr)
		}
		err = vp.startDomain(ctx, id)
		// Sync persistent XML after reboot as well
		if err == nil {
			if syncErr := syncPersistentXML(ctx, vp, id); syncErr != nil {
				log.Printf("WARN Failed to sync persistent XML for %s: %v", id, syncErr)
			}
		}
	case contracts.PowerOpShutdownGraceful:
		// Graceful shutdown for libvirt - attempt guest shutdown, fallback to force stop
		err = vp.shutdownDomain(ctx, id)
		if err != nil {
			log.Printf("WARN Graceful shutdown failed for %s, falling back to force stop: %v", id, err)
			err = vp.stopDomain(ctx, id)
		}
	default:
		return unsupportedPowerOpError(op)
	}

	if err != nil {
		return contracts.NewRetryableError(fmt.Sprintf("failed to perform power operation %s", op), err)
	}

	log.Printf("INFO Successfully performed power operation %s on %s", op, d.name)
	return nil
}

// syncPersistentXML updates the persistent definition of domainName on vp's
// host to match its running state. This prevents "pending changes" in
// management tools like Cockpit by ensuring the persistent XML matches what
// libvirt expanded (e.g., host-model CPU to specific features).
func syncPersistentXML(ctx context.Context, vp *VirshProvider, domainName string) error {
	log.Printf("INFO Syncing persistent XML definition for domain: %s", domainName)

	// Get the running domain XML (this includes expanded CPU features, etc.)
	result, err := vp.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		return fmt.Errorf("failed to dump running XML: %w", err)
	}

	// Write the running XML to a temporary file (content over stdin, path as a
	// positional parameter — no heredoc, no shell interpolation).
	remotePath := fmt.Sprintf("/tmp/%s-sync.xml", domainName)
	if err := vp.writeRemoteFile(ctx, remotePath, []byte(result.Stdout)); err != nil {
		return fmt.Errorf("failed to write sync XML file: %w", err)
	}

	// Define the domain again with the running XML (this updates the persistent definition)
	_, err = vp.runRemoteVirshCommand(ctx, "define", remotePath)
	if err != nil {
		return fmt.Errorf("failed to redefine domain: %w", err)
	}

	// Clean up temporary file
	_, cleanupErr := vp.runVirshCommand(ctx, "!", "rm", "-f", remotePath)
	if cleanupErr != nil {
		log.Printf("WARN Failed to cleanup sync XML file: %v", cleanupErr)
	}

	log.Printf("INFO Successfully synced persistent XML for domain: %s", domainName)
	return nil
}

// Reconfigure updates VM configuration using virsh.
//
// Topology dispatch (ADR-0007 Addendum A, slice 2): a single-host provider
// reconfigures on p.virshProvider exactly as before (vm.HostID and vm.Owner are
// ignored); a clustered one leases vm.HostID's connection, checks the domain's
// owner stamp against vm.Owner, and runs the same core there on the checked
// domain — including the online disk grow and its in-guest filesystem grow,
// whose guest-agent commands go to that host (reconfigureClustered).
func (p *Provider) Reconfigure(ctx context.Context, vm contracts.VMRef, desired contracts.CreateRequest) (taskRef string, err error) {
	id := vm.ID
	log.Printf("INFO Reconfiguring VM: %s", id)

	if p.clustered() {
		return "", p.withHostConn(ctx, vm.HostID, func(c libvirtConn) error {
			return p.reconfigureClustered(ctx, c, id, vm.Owner, desired)
		})
	}

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	return "", p.reconfigureOn(ctx, p.singleHostConn(), byName(id), desired)
}

// reconfigureClustered is the routed, OWNER-CHECKED Reconfigure of a clustered
// VM on its bound host's leased connection c: nothing about a domain is read or
// changed unless its owner stamp records owner's UID, and every command then
// addresses it by its UUID (ownedDomainTarget).
func (p *Provider) reconfigureClustered(ctx context.Context, c libvirtConn, id string, owner contracts.ObjectIdentity, desired contracts.CreateRequest) error {
	vp, err := virshOf(c)
	if err != nil {
		return err
	}
	d, err := ownedDomainTarget(ctx, vp, c.HostID(), id, owner, "reconfigure")
	if err != nil {
		return err
	}
	return p.reconfigureOn(ctx, c, d, desired)
}

// reconfigureOn is the Reconfigure core, run on connection c — p.virshProvider's
// connection in single-host mode, the leased host connection in clustered
// mode — against domain d. Every helper it reaches (the online CPU/memory
// change, the offline and online disk resize, the guest agent that grows the
// in-guest filesystem) runs on that same connection. The virsh/host command
// sequence is the historical one, unchanged.
func (p *Provider) reconfigureOn(ctx context.Context, c libvirtConn, d domainTarget, desired contracts.CreateRequest) error {
	vp, err := virshOf(c)
	if err != nil {
		return err
	}
	id := d.name

	hasChanges := false
	requiresRestart := false

	// Get current domain state
	domainState, err := vp.getDomainState(ctx, d.handle)
	if err != nil {
		return contracts.NewRetryableError("failed to get domain state", err)
	}

	isRunning := domainState == "running"
	log.Printf("INFO Domain %s current state: %s", id, domainState)

	// Get current domain info for comparison
	currentInfo, err := vp.getDomainInfo(ctx, d.handle)
	if err != nil {
		return contracts.NewRetryableError("failed to get current domain info", err)
	}

	// Handle CPU changes
	if desired.Class.CPU > 0 {
		currentCPUs, err := p.extractCPUCount(currentInfo)
		if err == nil && currentCPUs != desired.Class.CPU {
			log.Printf("INFO CPU change requested for %s: %d -> %d", id, currentCPUs, desired.Class.CPU)

			if isRunning {
				// Try online CPU change with --live flag
				_, err = vp.runVirshCommand(ctx, "setvcpus", d.handle,
					fmt.Sprintf("%d", desired.Class.CPU), "--live")
				if err != nil {
					// A `setvcpus --live` failure here means the desired vCPU
					// count exceeds the hotplug headroom provisioned at create
					// (the <vcpu> max), or the VM was created without
					// CPUHotAddEnabled (no headroom at all). Either way the
					// increase requires a power cycle to take effect (#203).
					log.Printf("WARN Online CPU change to %d failed for %s: exceeds provisioned hotplug headroom (the <vcpu> max) or the VM was created without CPUHotAddEnabled; a power cycle is required to apply this increase: %v",
						desired.Class.CPU, id, err)
					requiresRestart = true
				} else {
					log.Printf("INFO Successfully changed CPUs online for domain: %s", id)
					hasChanges = true
				}
			} else {
				// Domain is off, change config
				_, err = vp.runVirshCommand(ctx, "setvcpus", d.handle,
					fmt.Sprintf("%d", desired.Class.CPU), "--config")
				if err != nil {
					log.Printf("WARN Failed to set CPUs in config: %v", err)
					requiresRestart = true
				} else {
					hasChanges = true
				}
			}
		}
	}

	// Handle Memory changes
	if desired.Class.MemoryMiB > 0 {
		currentMemoryKB, err := p.extractMemoryKB(currentInfo)
		desiredMemoryKB := int64(desired.Class.MemoryMiB) * 1024 // Convert MiB to KiB

		if err == nil && currentMemoryKB != desiredMemoryKB {
			log.Printf("INFO Memory change requested for %s: %d KiB -> %d KiB", id, currentMemoryKB, desiredMemoryKB)

			if isRunning {
				// Try online memory change with --live flag
				_, err = vp.runVirshCommand(ctx, "setmem", d.handle,
					fmt.Sprintf("%dK", desiredMemoryKB), "--live")
				if err != nil {
					// `setmem --live` inflates the balloon up to the <memory>
					// ceiling. A failure here means the desired memory exceeds
					// the hotplug headroom provisioned at create (the <memory>
					// balloon maximum), or the VM was created without
					// MemoryHotAddEnabled (no headroom at all). Either way the
					// increase requires a power cycle to take effect (#203).
					log.Printf("WARN Online memory change to %d KiB failed for %s: exceeds provisioned hotplug headroom (the <memory> balloon maximum) or the VM was created without MemoryHotAddEnabled; a power cycle is required to apply this increase: %v",
						desiredMemoryKB, id, err)
					requiresRestart = true
				} else {
					log.Printf("INFO Successfully changed memory online for domain: %s", id)
					hasChanges = true
				}
			} else {
				// Domain is off, change config
				_, err = vp.runVirshCommand(ctx, "setmem", d.handle,
					fmt.Sprintf("%dK", desiredMemoryKB), "--config")
				if err != nil {
					log.Printf("WARN Failed to set memory in config: %v", err)
					requiresRestart = true
				} else {
					// Also update max memory
					_, _ = vp.runVirshCommand(ctx, "setmaxmem", d.handle,
						fmt.Sprintf("%dK", desiredMemoryKB), "--config")
					hasChanges = true
				}
			}
		}
	}

	// Handle Disk changes.
	//
	// Online (domain running): grow the live block device via `virsh
	// blockresize` so QEMU exposes the new size immediately, then best-effort
	// extend the in-guest filesystem via the guest agent. The target device and
	// current size are resolved from the live domain (domblklist/domblkinfo),
	// not the "<vmid>-disk" volume-name guess. Grow-only: shrinks are rejected
	// (libvirt/qcow2 cannot shrink live) and resizing to the current size is a
	// no-op. A blockresize failure is fatal to the disk step; the in-guest FS
	// grow is non-fatal (#201).
	//
	// Offline (domain stopped): resize the backing volume so the larger size
	// applies on next boot.
	if len(desired.Disks) > 0 || (desired.Class.DiskDefaults != nil && desired.Class.DiskDefaults.SizeGiB > 0) {
		storageProvider := NewStorageProvider(vp)

		// Get desired disk size
		var desiredDiskGB int
		if desired.Class.DiskDefaults != nil && desired.Class.DiskDefaults.SizeGiB > 0 {
			desiredDiskGB = int(desired.Class.DiskDefaults.SizeGiB)
		}

		if desiredDiskGB > 0 {
			if isRunning {
				// Online live grow (grow-only + idempotent guards inside).
				log.Printf("INFO Attempting online disk grow for running VM %s to %dGB", id, desiredDiskGB)
				grew, gerr := growDiskOnline(ctx, vp, d, desiredDiskGB, storageProvider)
				if gerr != nil {
					// The live block-device resize failing IS fatal to the disk
					// step: the guest would not see the requested capacity.
					log.Printf("WARN Online disk grow failed for VM %s: %v", id, gerr)
					return contracts.NewRetryableError("online disk grow failed", gerr)
				}
				if grew {
					hasChanges = true
				}
			} else {
				// Offline: resize the backing volume so the larger size applies
				// on next boot. Single-host finds the VM's disk volume by the pool
				// convention (historical). A clustered target resizes the
				// owner-checked domain's own primary disk, by its path.
				log.Printf("INFO Attempting offline disk resize for VM %s to %dGB", id, desiredDiskGB)
				if d.diskByPath {
					err = resizePrimaryDiskOffline(ctx, vp, d, storageProvider, desiredDiskGB)
				} else {
					err = storageProvider.ResizeVolume(ctx, "default", vmDiskVolumeName(id), desiredDiskGB)
				}
				if err != nil {
					log.Printf("WARN Offline disk resize failed: %v", err)
					// Offline resize failure is not fatal, just log it.
				} else {
					log.Printf("INFO Successfully resized disk for VM: %s", id)
					hasChanges = true
				}
			}
		}
	}

	// Log reconfiguration results
	if !hasChanges && !requiresRestart {
		log.Printf("INFO No configuration changes needed for domain: %s", id)
		return nil
	}

	if requiresRestart {
		log.Printf("WARN Some changes for domain %s require a restart to take effect", id)
		// Note: The caller (controller) should handle restarting the VM if needed
	}

	log.Printf("INFO Successfully reconfigured domain: %s", id)
	return nil
}

// resizePrimaryDiskOffline grows the primary disk of the stopped domain d (the
// clustered, owner-checked target) to desiredDiskGB: the disk is read from the
// domain itself (domblklist --details) and resized by its path, so the resize
// can only ever act on this domain's own disk — never on a volume that merely
// matches a naming convention.
func resizePrimaryDiskOffline(ctx context.Context, vp *VirshProvider, d domainTarget, sp *StorageProvider, desiredDiskGB int) error {
	disk, err := domainPrimaryDisk(ctx, vp, d.handle)
	if err != nil {
		return fmt.Errorf("resolve primary disk of domain %s: %w", d.name, err)
	}
	return sp.ResizeVolumeByPath(ctx, disk.path, desiredDiskGB)
}

// getVNCPort extracts the VNC port from domain XML on vp's host
func (p *Provider) getVNCPort(ctx context.Context, vp *VirshProvider, domainName string) (int, error) {
	// Get domain XML
	result, err := vp.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		return 0, fmt.Errorf("failed to get domain XML: %w", err)
	}

	// Parse XML to find VNC port
	// Look for <graphics type='vnc' port='XXXX'/>
	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "<graphics") && strings.Contains(line, "type='vnc'") {
			// Extract port attribute
			if portIdx := strings.Index(line, "port='"); portIdx != -1 {
				portStart := portIdx + 6 // len("port='")
				portEnd := strings.Index(line[portStart:], "'")
				if portEnd > 0 {
					portStr := line[portStart : portStart+portEnd]
					port, err := strconv.Atoi(portStr)
					if err == nil && port > 0 {
						return port, nil
					}
				}
			}
		}
	}

	return 0, fmt.Errorf("VNC port not found in domain XML")
}

// extractCPUCount extracts the CPU count from domain info map
func (p *Provider) extractCPUCount(domainInfo map[string]string) (int32, error) {
	cpuStr, exists := domainInfo["CPU(s)"]
	if !exists {
		return 0, fmt.Errorf("CPU count not found in domain info")
	}

	cpuCount, err := strconv.ParseInt(cpuStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse CPU count: %w", err)
	}

	return int32(cpuCount), nil
}

// extractMemoryKB extracts the memory in KiB from domain info map
func (p *Provider) extractMemoryKB(domainInfo map[string]string) (int64, error) {
	memStr, exists := domainInfo["Max memory"]
	if !exists {
		// Try alternative key
		memStr, exists = domainInfo["Used memory"]
		if !exists {
			return 0, fmt.Errorf("memory not found in domain info")
		}
	}

	// Parse memory string (format: "XXXXXX KiB")
	memStr = strings.TrimSpace(memStr)
	memStr = strings.TrimSuffix(memStr, " KiB")
	memStr = strings.TrimSuffix(memStr, " kB")

	memKB, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse memory: %w", err)
	}

	return memKB, nil
}

// Describe returns comprehensive VM information using virsh (enhanced monitoring like vSphere)
//
// Topology dispatch (ADR-0007 Addendum A, A1): a single-host provider describes
// on p.virshProvider exactly as before (vm.HostID and vm.Owner are ignored); a
// clustered one leases vm.HostID's connection, checks the domain's owner stamp
// against vm.Owner, and runs the same core there, so the virsh read and the
// shadow read share that lease (describeClustered).
func (p *Provider) Describe(ctx context.Context, vm contracts.VMRef) (contracts.DescribeResponse, error) {
	id := vm.ID
	log.Printf("INFO Describing VM with comprehensive monitoring: %s", id)

	if p.clustered() {
		var resp contracts.DescribeResponse
		err := p.withHostConn(ctx, vm.HostID, func(c libvirtConn) error {
			var derr error
			resp, derr = p.describeClustered(ctx, c, id, vm.Owner)
			return derr
		})
		return resp, err
	}

	if p.virshProvider == nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	return p.describeOn(ctx, p.singleHostConn(), id)
}

// describeClustered is the routed, OWNER-CHECKED Describe of a clustered VM on
// its bound host's leased connection c (ADR-0007 Addendum A).
//
// Before any state is read it checks the domain's owner stamp against owner
// (checkDomainOwner). A domain that is absent, or whose stamp is missing,
// unreadable or records another owner, is reported as exists=false and none of
// its state is returned — so if a VirtualMachine's domain vanished and another
// tenant's same-named VM was later created on the host, the first
// VirtualMachine never sees the second one's IPs, power state or console. The
// operator then applies A4 (never re-create a clustered VM).
//
// After the shared core ran, the domain UUID it read is compared with the one
// read alongside the stamp, so a domain replaced between the two reads is
// reported as absent too. The single-host Describe keeps its historical
// behavior (no owner check, error on an absent domain).
func (p *Provider) describeClustered(ctx context.Context, c libvirtConn, id string, owner contracts.ObjectIdentity) (contracts.DescribeResponse, error) {
	vp, err := virshOf(c)
	if err != nil {
		return contracts.DescribeResponse{}, err
	}
	host := c.HostID()

	own, err := checkDomainOwner(ctx, vp, host, id, owner, "describe")
	if err != nil {
		return contracts.DescribeResponse{}, err
	}
	if !own.present || !own.owned {
		log.Printf("INFO Domain %s is not present on host %s for this VirtualMachine; reporting exists=false", id, host)
		return contracts.DescribeResponse{Exists: false}, nil
	}
	if own.uuid == "" {
		return contracts.DescribeResponse{}, contracts.NewRetryableError(
			fmt.Sprintf("cannot verify the identity of domain %q on host %s (no UUID in its definition)", id, host), nil)
	}

	resp, err := p.describeOn(ctx, c, id)
	if err != nil {
		return contracts.DescribeResponse{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(resp.ProviderRaw["UUID"]), own.uuid) {
		log.Printf("WARN Domain %s on host %s changed identity during describe; reporting exists=false", id, host)
		return contracts.DescribeResponse{Exists: false}, nil
	}
	return resp, nil
}

// describeOn is the Describe core, run on connection c — p.virshProvider's
// connection in single-host mode, the leased host connection in clustered
// mode. The virsh command sequence is the historical one, unchanged; the
// ADR-0008 shadow read (when enabled) is dispatched on the same connection.
func (p *Provider) describeOn(ctx context.Context, c libvirtConn, id string) (contracts.DescribeResponse, error) {
	vp, err := virshOf(c)
	if err != nil {
		return contracts.DescribeResponse{}, err
	}

	// Get comprehensive domain information (now includes enhanced monitoring)
	domainInfo, err := vp.getDomainInfo(ctx, id)
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain info", err)
	}

	// Initialize guest agent provider for enhanced guest information
	guestAgent := NewGuestAgentProvider(vp)

	// Extract power state (libvirt uses different names than vSphere)
	powerState := p.mapLibvirtPowerState(domainInfo["State"])

	// Extract IP addresses from enhanced domain info
	var ips []string
	if guestIPs := domainInfo["guest_ip_addresses"]; guestIPs != "" {
		ips = strings.Split(guestIPs, ",")
		// Filter out empty strings
		var validIPs []string
		for _, ip := range ips {
			if strings.TrimSpace(ip) != "" {
				validIPs = append(validIPs, strings.TrimSpace(ip))
			}
		}
		ips = validIPs
	}

	// Get primary IP (first valid IP)
	primaryIP := ""
	if len(ips) > 0 {
		primaryIP = ips[0]
	}

	// Extract comprehensive information for ProviderRawJson (like vSphere)
	hostname := domainInfo["guest_hostname"]
	if hostname == "" {
		hostname = domainInfo["Name"] // fallback to domain name
	}

	// If VM is running, try to get enhanced guest information via QEMU Guest Agent
	if powerState == "On" {
		if guestInfo, err := guestAgent.GetGuestInfo(ctx, id); err == nil {
			// Enhanced Guest OS Information
			if guestInfo.OSName != "" {
				domainInfo["guest_os"] = guestInfo.OSName
				domainInfo["guest_os_version"] = guestInfo.OSVersion
				domainInfo["guest_os_pretty_name"] = guestInfo.OSPrettyName
				domainInfo["guest_kernel_release"] = guestInfo.OSKernelRelease
				domainInfo["guest_machine"] = guestInfo.OSMachine
			}

			// Guest Agent Status and Version
			domainInfo["guest_agent_status"] = guestInfo.AgentStatus
			if guestInfo.AgentVersion != "" {
				domainInfo["guest_agent_version"] = guestInfo.AgentVersion
			}

			// Enhanced Network Information from Guest Agent
			if len(guestInfo.NetworkInterfaces) > 0 {
				var guestIPs []string
				var interfaceNames []string

				for _, iface := range guestInfo.NetworkInterfaces {
					interfaceNames = append(interfaceNames, iface.Name)
					guestIPs = append(guestIPs, iface.IPAddresses...)

					// Add detailed network statistics
					domainInfo[fmt.Sprintf("net_%s_rx_bytes", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.RxBytes)
					domainInfo[fmt.Sprintf("net_%s_tx_bytes", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.TxBytes)
					domainInfo[fmt.Sprintf("net_%s_rx_packets", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.RxPackets)
					domainInfo[fmt.Sprintf("net_%s_tx_packets", iface.Name)] = fmt.Sprintf("%d", iface.Statistics.TxPackets)
					domainInfo[fmt.Sprintf("net_%s_mac", iface.Name)] = iface.HardwareAddr
				}

				// Use guest agent IPs if available (more accurate than virsh)
				if len(guestIPs) > 0 {
					ips = guestIPs
					primaryIP = guestIPs[0]
				}
				domainInfo["guest_network_interfaces"] = strings.Join(interfaceNames, ",")
			}

			// Guest Filesystem Information
			if len(guestInfo.Filesystems) > 0 {
				var mountpoints []string
				var totalDiskSpace uint64
				var usedDiskSpace uint64

				for _, fs := range guestInfo.Filesystems {
					mountpoints = append(mountpoints, fs.Mountpoint)
					totalDiskSpace += fs.TotalBytes
					usedDiskSpace += fs.UsedBytes

					// Add per-filesystem statistics
					safeMountpoint := strings.ReplaceAll(strings.ReplaceAll(fs.Mountpoint, "/", "_"), "-", "_")
					domainInfo[fmt.Sprintf("fs_%s_total", safeMountpoint)] = fmt.Sprintf("%d", fs.TotalBytes)
					domainInfo[fmt.Sprintf("fs_%s_used", safeMountpoint)] = fmt.Sprintf("%d", fs.UsedBytes)
					domainInfo[fmt.Sprintf("fs_%s_free", safeMountpoint)] = fmt.Sprintf("%d", fs.FreeBytes)
					domainInfo[fmt.Sprintf("fs_%s_type", safeMountpoint)] = fs.Type
				}

				domainInfo["guest_filesystems"] = strings.Join(mountpoints, ",")
				domainInfo["guest_disk_total"] = fmt.Sprintf("%d", totalDiskSpace)
				domainInfo["guest_disk_used"] = fmt.Sprintf("%d", usedDiskSpace)
				domainInfo["guest_disk_free"] = fmt.Sprintf("%d", totalDiskSpace-usedDiskSpace)
			}

			// Guest Time Information
			if !guestInfo.GuestTime.IsZero() {
				domainInfo["guest_time"] = guestInfo.GuestTime.Format(time.RFC3339)
				domainInfo["guest_time_sync"] = "available"
			}

			log.Printf("INFO Enhanced guest information collected via QEMU Guest Agent for domain: %s", id)
		} else {
			log.Printf("DEBUG QEMU Guest Agent not available for domain %s: %v", id, err)
		}
	}

	// Add comprehensive monitoring fields to domain info for ProviderRaw
	domainInfo["primary_ip"] = primaryIP
	domainInfo["hostname"] = hostname
	domainInfo["tools_status"] = p.getToolsStatus(domainInfo)
	domainInfo["power_state_mapped"] = string(powerState)

	// Ensure guest OS is properly set
	if domainInfo["guest_os"] == "" && domainInfo["OS Type"] != "" {
		domainInfo["guest_os"] = domainInfo["OS Type"]
	}

	// Generate console URL (VNC/SPICE access info)
	consoleURL := ""
	if powerState == "On" {
		// Try to get VNC display information
		vncPort, err := p.getVNCPort(ctx, vp, id)
		if err == nil && vncPort > 0 {
			// Build VNC URL
			// Extract host from libvirt URI
			host := "localhost" // Default to localhost
			if vp.uri != "" {
				if parsedURI, err := url.Parse(vp.uri); err == nil && parsedURI.Host != "" {
					host = parsedURI.Host
					// Remove port from host if present
					if colonIdx := strings.Index(host, ":"); colonIdx != -1 {
						host = host[:colonIdx]
					}
				}
			}
			consoleURL = fmt.Sprintf("vnc://%s:%d", host, vncPort)
			domainInfo["vnc_port"] = fmt.Sprintf("%d", vncPort)
		}
	}

	// Convert virsh domain info to contracts format
	response := contracts.DescribeResponse{
		Exists:      true,
		PowerState:  string(powerState),
		IPs:         ips,
		ConsoleURL:  consoleURL,
		ProviderRaw: domainInfo, // Pass the enhanced domain info as provider-specific data
	}

	log.Printf("INFO Domain %s comprehensive state: power=%s, ips=%v, monitoring_data=collected", id, response.PowerState, ips)

	// ADR-0008 PR 4b: when the describe family is in shadow mode, also run the
	// go-libvirt Describe and meter any semantic divergence against this
	// authoritative virsh response. This returns immediately (the shadow runs on a
	// detached, time-bounded, panic-isolated goroutine) and NEVER alters what is
	// returned here — reads do not flip to native until PR 5. No-op when shadow is
	// off (the default), so pure virsh is unchanged.
	p.maybeShadowDescribe(ctx, c, id, response)

	return response, nil
}

// IsTaskComplete checks if a task is complete (virsh operations are usually synchronous)
func (p *Provider) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	// Most virsh operations are synchronous, so tasks are immediately complete
	return true, nil
}

// mapLibvirtPowerState maps libvirt power states to VirtRigaud standard power states
func (p *Provider) mapLibvirtPowerState(libvirtState string) contracts.PowerState {
	switch strings.ToLower(libvirtState) {
	case "running":
		return "On"
	case "shut off", "shutoff":
		return "Off"
	case "paused", "suspended":
		return "Off" // Treat paused/suspended as Off for consistency with vSphere
	case "in shutdown", "shutting down":
		return "Off" // Transitioning to off
	default:
		return "Off" // Default to Off for unknown states
	}
}

// getToolsStatus determines guest tools equivalent status
func (p *Provider) getToolsStatus(domainInfo map[string]string) string {
	// Check QEMU Guest Agent status first (most accurate)
	if agentStatus := domainInfo["guest_agent_status"]; agentStatus != "" {
		switch agentStatus {
		case "available":
			return "toolsOk"
		case "not_available":
			return "toolsNotInstalled"
		default:
			return "toolsNotRunning"
		}
	}

	// Fallback: Check if we have guest agent connectivity indicators
	if guestHost := domainInfo["guest_hostname"]; guestHost != "" {
		return "toolsOk" // Guest agent is working
	}
	if guestIPs := domainInfo["guest_ip_addresses"]; guestIPs != "" {
		return "toolsOk" // We can get IP addresses from guest
	}
	if guestOS := domainInfo["guest_os"]; guestOS != "" && guestOS != domainInfo["OS Type"] {
		return "toolsOk" // We have enhanced guest OS info
	}

	return "toolsNotInstalled" // No guest agent connectivity
}

// ExecuteGuestCommand executes a command inside the guest via QEMU Guest Agent
func (p *Provider) ExecuteGuestCommand(ctx context.Context, id, command string) (string, error) {
	log.Printf("INFO Executing guest command in VM %s: %s", id, command)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	guestAgent := NewGuestAgentProvider(p.virshProvider)
	result, err := guestAgent.ExecuteGuestCommand(ctx, id, command)
	if err != nil {
		return "", contracts.NewRetryableError("failed to execute guest command", err)
	}

	log.Printf("INFO Successfully executed guest command in VM: %s", id)
	return result, nil
}

// SyncGuestTime synchronizes the guest time with the host
func (p *Provider) SyncGuestTime(ctx context.Context, id string) error {
	log.Printf("INFO Synchronizing guest time for VM: %s", id)

	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	guestAgent := NewGuestAgentProvider(p.virshProvider)
	if err := guestAgent.SetGuestTime(ctx, id); err != nil {
		return contracts.NewRetryableError("failed to sync guest time", err)
	}

	log.Printf("INFO Successfully synchronized guest time for VM: %s", id)
	return nil
}

// GetGuestInfo retrieves detailed guest information via QEMU Guest Agent
func (p *Provider) GetGuestInfo(ctx context.Context, id string) (*GuestAgentInfo, error) {
	log.Printf("INFO Retrieving detailed guest information for VM: %s", id)

	if p.virshProvider == nil {
		return nil, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	guestAgent := NewGuestAgentProvider(p.virshProvider)
	guestInfo, err := guestAgent.GetGuestInfo(ctx, id)
	if err != nil {
		return nil, contracts.NewRetryableError("failed to get guest info", err)
	}

	log.Printf("INFO Successfully retrieved guest information for VM: %s", id)
	return guestInfo, nil
}

// extractImageSpec extracts the image specification from the request
func (p *Provider) extractImageSpec(req contracts.CreateRequest) string {
	// Priority 1: Use explicit path from VMImage (for local template images)
	if req.Image.Path != "" {
		log.Printf("INFO Using image path from VMImage: %q", req.Image.Path)
		return req.Image.Path
	}

	// Priority 2: Use URL from VMImage (for remote images)
	if req.Image.URL != "" {
		log.Printf("INFO Using image URL from VMImage: %q", req.Image.URL)
		return req.Image.URL
	}

	// Priority 3: Use template name if provided
	if req.Image.TemplateName != "" {
		log.Printf("INFO Using template name from VMImage: %q", req.Image.TemplateName)
		return req.Image.TemplateName
	}

	// No image specified - will create empty disk
	log.Printf("INFO No image specified in VMImage, will create empty disk")
	return ""
}

// extractDiskSize extracts the disk size from VMClass DiskDefaults
func (p *Provider) extractDiskSize(req contracts.CreateRequest) int {
	// Check if VMClass has DiskDefaults with size specified
	if req.Class.DiskDefaults != nil && req.Class.DiskDefaults.SizeGiB > 0 {
		log.Printf("INFO Using disk size from VMClass: %dGB", req.Class.DiskDefaults.SizeGiB)
		return int(req.Class.DiskDefaults.SizeGiB)
	}

	// Default to 20GB if not specified
	log.Printf("INFO No disk size specified in VMClass, using default: 20GB")
	return 20
}

// generateDefaultCloudInit generates the cloud-init user-data applied when a
// VirtualMachine supplies none. It deliberately provisions NO login user, SSH
// key, password, or sudo grant: access to the guest is the VM owner's decision
// and must come from their own spec.userData. It only sets the hostname and
// installs + starts qemu-guest-agent, which IP discovery and in-guest
// operations (e.g. online filesystem growth) rely on.
//
// vmName is a Kubernetes object name (DNS-1123), so it cannot break out of the
// YAML scalar it is interpolated into.
func (p *Provider) generateDefaultCloudInit(vmName string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s

packages:
  - qemu-guest-agent

runcmd:
  - systemctl enable qemu-guest-agent
  - systemctl start qemu-guest-agent
`, vmName)
}

// buildDiskDevicesXML renders the primary disk `<disk>` element and, when
// cloudInitISOPath is non-empty, the cloud-init CD-ROM `<disk>` element that
// follows it, for the create-path domain XML.
//
// diskPath and cloudInitISOPath are host file paths computed by this
// provider (a storage-pool directory joined with a name derived from
// req.Name), not raw user input — but req.Name only carries a Kubernetes
// object-name pattern, not an XML-safety one, so both are escaped anyway
// (issue #260 defense in depth) rather than trusting the path-construction
// call chain to never change.
func buildDiskDevicesXML(diskPath, cloudInitISOPath string) string {
	diskDevicesXML := fmt.Sprintf(`    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </disk>`, xmlEscape(diskPath))

	// Add cloud-init ISO if available
	if cloudInitISOPath != "" {
		diskDevicesXML += fmt.Sprintf(`
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='hda' bus='ide'/>
      <readonly/>
      <address type='drive' controller='0' bus='0' target='0' unit='0'/>
    </disk>`, xmlEscape(cloudInitISOPath))
	}

	return diskDevicesXML
}

// generateNetworkInterfacesXML creates network interface XML from network
// attachments.
//
// net.Bridge, net.NetworkName, net.Model, and net.MacAddress are CR-derived
// (VMNetworkAttachment / VirtualMachine spec fields) and, at the time of
// issue #260, were not all pattern-validated against XML metacharacters at
// admission time — so every one is passed through xmlEscape before
// interpolation. pciSlot is computed from the loop index (numeric, safe);
// model falls back to the "virtio" constant when net.Model is empty.
func (p *Provider) generateNetworkInterfacesXML(networks []contracts.NetworkAttachment) string {
	if len(networks) == 0 {
		// Default to user network if no networks specified
		return `    <interface type='user'>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>`
	}

	var interfacesXML string
	for idx, net := range networks {
		// Determine network model (default to virtio)
		model := "virtio"
		if net.Model != "" {
			model = xmlEscape(net.Model)
		}

		// Determine PCI slot (start at 0x03, increment for each interface)
		pciSlot := fmt.Sprintf("0x%02x", 3+idx)

		// Generate MAC address if specified
		macXML := ""
		if net.MacAddress != "" {
			macXML = fmt.Sprintf("\n      <mac address='%s'/>", xmlEscape(net.MacAddress))
		}

		var interfaceXML string

		// Determine interface type and configuration
		if net.Bridge != "" {
			// Bridge network
			interfaceXML = fmt.Sprintf(`    <interface type='bridge'>%s
      <source bridge='%s'/>
      <model type='%s'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='%s' function='0x0'/>
    </interface>`, macXML, xmlEscape(net.Bridge), model, pciSlot)
		} else if net.NetworkName != "" {
			// Libvirt managed network
			interfaceXML = fmt.Sprintf(`    <interface type='network'>%s
      <source network='%s'/>
      <model type='%s'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='%s' function='0x0'/>
    </interface>`, macXML, xmlEscape(net.NetworkName), model, pciSlot)
		} else {
			// Default to user network (NAT)
			interfaceXML = fmt.Sprintf(`    <interface type='user'>%s
      <model type='%s'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='%s' function='0x0'/>
    </interface>`, macXML, model, pciSlot)
		}

		if idx > 0 {
			interfacesXML += "\n"
		}
		interfacesXML += interfaceXML
	}

	return interfacesXML
}

// generateDomainXMLWithStorage creates libvirt domain XML with proper storage configuration
func (p *Provider) generateDomainXMLWithStorage(ctx context.Context, vp *VirshProvider, req contracts.CreateRequest, diskPath, cloudInitISOPath string) (string, error) {
	// Extract specifications from request
	cpuCount := int32(1)    // default
	memoryMB := int64(1024) // default 1GB

	// Extract from VMClass
	if req.Class.CPU > 0 {
		cpuCount = req.Class.CPU
	}

	if req.Class.MemoryMiB > 0 {
		memoryMB = int64(req.Class.MemoryMiB)
	}

	// Extract performance and security features
	var nestedVirtualization bool
	var cpuHotAddEnabled bool
	var memoryHotAddEnabled bool
	var vtdEnabled bool
	var secureBoot bool
	var tpmEnabled bool

	if req.Class.PerformanceProfile != nil {
		nestedVirtualization = req.Class.PerformanceProfile.NestedVirtualization
		// CPU/MemoryHotAddEnabled provision hotplug headroom at create time so
		// the live Reconfigure path (`setvcpus/setmem --live`) can grow a
		// running VM up to the ~4× ceiling without a power cycle (#203). When
		// these are false the emitted XML is byte-identical to the historical
		// layout (no <vcpu current=...>, <memory>==<currentMemory>).
		cpuHotAddEnabled = req.Class.PerformanceProfile.CPUHotAddEnabled
		memoryHotAddEnabled = req.Class.PerformanceProfile.MemoryHotAddEnabled
	}

	if req.Class.SecurityProfile != nil {
		vtdEnabled = req.Class.SecurityProfile.VTDEnabled
		secureBoot = req.Class.SecurityProfile.SecureBoot
		tpmEnabled = req.Class.SecurityProfile.TPMEnabled
	}

	// A fresh, unpredictable RFC 4122 v4 UUID (crypto/rand) for the domain. This
	// does not by itself make concurrent same-named creates safe; see
	// generateRandomUUID.
	uuid, err := generateRandomUUID()
	if err != nil {
		return "", fmt.Errorf("generate domain UUID: %w", err)
	}

	// Stamp the requesting VirtualMachine's identity into <metadata> so a later
	// same-named create can prove ownership (bindExistingDomain). Empty when the
	// request carries no owner UID.
	ownerMetadataXML := renderOwnerMetadataXML(req.Owner)

	// Build disk devices XML. diskPath/cloudInitISOPath are provider-computed
	// host file paths (pool directory + a name derived from req.Name), but are
	// escaped anyway (see buildDiskDevicesXML) as defense in depth.
	devicesDiskXML := buildDiskDevicesXML(diskPath, cloudInitISOPath)

	// Build features XML based on configuration
	featuresXML := `    <acpi/>
    <apic/>`

	if secureBoot {
		featuresXML += `
    <smm state='on'>
      <tseg unit='MiB'>16</tseg>
    </smm>`
	}

	if vtdEnabled {
		featuresXML += `
    <iommu model='intel'/>` // or 'amd' for AMD systems
	}

	// Build CPU configuration with nested virtualization support
	cpuXML := `<cpu mode='host-model' check='partial'>`
	if nestedVirtualization {
		cpuXML += `
    <feature policy='require' name='vmx'/> <!-- Intel VT-x -->
    <feature policy='require' name='svm'/> <!-- AMD-V -->`
	}
	cpuXML += `</cpu>`

	// Build OS configuration with secure boot if needed
	osXML := `    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='hd'/>
    <boot dev='cdrom'/>`

	if secureBoot {
		osXML = `    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash' secure='yes'>/usr/share/OVMF/OVMF_CODE_4M.secboot.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS_4M.fd'/>
    <boot dev='hd'/>
    <boot dev='cdrom'/>`
	}

	// Build devices XML with TPM if needed.
	//
	// Intentionally omit <emulator>: libvirt's qemu driver fills in the host's
	// default emulator for the domain's (arch, machine, type) during define-time
	// post-parse. Hard-coding /usr/bin/qemu-system-x86_64 broke hosts where the
	// binary lives elsewhere (e.g. RHEL/CentOS at /usr/libexec/qemu-kvm). The
	// provider talks to a possibly-remote libvirtd over SSH, so letting the
	// remote daemon resolve its own emulator is both simpler and more correct.
	devicesXML := devicesDiskXML

	if tpmEnabled {
		devicesXML += `
    <tpm model='tpm-tis'>
      <backend type='emulator' version='2.0'/>
    </tpm>`
	}

	// Generate network interfaces based on request
	networkInterfacesXML := p.generateNetworkInterfacesXML(req.Networks)

	// Build the CPU/memory elements, provisioning hotplug headroom when the
	// VMClass opts into CPU/MemoryHotAddEnabled (#203). When hot-add is off the
	// rendered elements are byte-identical to the historical
	// <memory>==<currentMemory>, <vcpu placement='static'>N</vcpu> layout.
	cpuMem := buildCPUMemoryXML(cpuCount, memoryMB, cpuHotAddEnabled, memoryHotAddEnabled)

	// Pick the domain type from the host: KVM for hardware acceleration when
	// available, otherwise TCG software emulation. Hard-coding either value is
	// wrong — 'qemu' cripples guests on KVM hosts (~100% CPU, glacial boot),
	// while 'kvm' fails to start on hosts without /dev/kvm.
	domainType := p.detectDomainType(ctx, vp)

	domainXML := fmt.Sprintf(`<domain type='%s'>
  <name>%s</name>
  <uuid>%s</uuid>
%s  %s
  %s
  %s
  <os>
%s
  </os>
  <features>
%s
  </features>
  %s
  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
%s
    <controller type='usb' index='0' model='ich9-ehci1'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x7'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci1'>
      <master startport='0'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x0' multifunction='on'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci2'>
      <master startport='2'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x1'/>
    </controller>
    <controller type='usb' index='0' model='ich9-uhci3'>
      <master startport='4'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x2'/>
    </controller>
    <controller type='pci' index='0' model='pci-root'/>
    <controller type='ide' index='0'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x01' function='0x1'/>
    </controller>
    <controller type='virtio-serial' index='0'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x06' function='0x0'/>
    </controller>
%s
    <serial type='pty'>
      <target type='isa-serial' port='0'>
        <model name='isa-serial'/>
      </target>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
      <address type='virtio-serial' controller='0' bus='0' port='1'/>
    </channel>
    <input type='tablet' bus='usb'>
      <address type='usb' bus='0' port='1'/>
    </input>
    <input type='mouse' bus='ps2'/>
    <input type='keyboard' bus='ps2'/>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <sound model='ich6'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x04' function='0x0'/>
    </sound>
    <video>
      <model type='cirrus' vram='16384' heads='1' primary='yes'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x02' function='0x0'/>
    </video>
    <memballoon model='virtio'>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
    </memballoon>
  </devices>
</domain>`,
		domainType,
		// req.Name only carries a Kubernetes object-name validation pattern
		// (DNS-1123-ish), not an XML-safety one — escape it into the <name>
		// element as defense in depth (issue #260).
		xmlEscape(req.Name),
		uuid,
		ownerMetadataXML, // "" or a full "  <metadata>...</metadata>\n" block; values escaped
		cpuMem.Memory,
		cpuMem.CurrentMemory,
		cpuMem.VCPU,
		osXML,
		featuresXML,
		cpuXML,
		devicesXML,
		networkInterfacesXML)

	return domainXML, nil
}

// detectDomainType returns the libvirt <domain type> for new domains: "kvm" when
// the (possibly remote) host exposes /dev/kvm (hardware acceleration), otherwise
// "qemu" (TCG software emulation). qemu is the safe fallback — it starts on any
// host, including when the probe itself fails — whereas a kvm domain fails to
// start without /dev/kvm. The probe runs over the same ssh/local path as the
// provider's other host-side checks (cf. the `test -f <image>` existence probe).
func (p *Provider) detectDomainType(ctx context.Context, vp *VirshProvider) string {
	// Happy path: one round-trip. `test -r` covers both "exists" and "readable".
	if _, err := vp.runVirshCommand(ctx, "!", "test", "-r", "/dev/kvm"); err == nil {
		return domainTypeFromProbe(true, false)
	}
	// Not readable — second probe (only on the failure path) tells the operator
	// whether /dev/kvm is simply absent (expected on a TCG-only host) or present
	// but unopenable (a host-side permission/cgroup misconfiguration to fix).
	_, existsErr := vp.runVirshCommand(ctx, "!", "test", "-e", "/dev/kvm")
	return domainTypeFromProbe(false, existsErr == nil)
}

// domainTypeFromProbe maps the /dev/kvm probe outcomes to a domain type and logs
// the reason for any fallback. readable = `test -r /dev/kvm` succeeded; exists is
// consulted only when !readable (from `test -e`) to distinguish absent from
// present-but-unreadable. Split out so the decision/logging is unit-testable
// without execing the probe.
func domainTypeFromProbe(readable, exists bool) string {
	switch {
	case readable:
		return "kvm"
	case exists:
		log.Printf("WARN /dev/kvm is present but not readable by the libvirt/qemu user " +
			"(device-cgroup or permission restriction); using domain type 'qemu' (software emulation). " +
			"Grant the qemu user read access to /dev/kvm to enable hardware acceleration.")
	default:
		log.Printf("INFO Host has no /dev/kvm; using domain type 'qemu' (software emulation)")
	}
	return "qemu"
}

// createDomainDefinition writes the domain XML to a temporary file on the host
// backed by vp (the single host, or the leased target host in clustered mode).
func (p *Provider) createDomainDefinition(ctx context.Context, vp *VirshProvider, domainName, domainXML string) error {
	// Create temporary file path on remote server
	remotePath := fmt.Sprintf("/tmp/%s-domain.xml", domainName)

	// Write the domain XML over stdin (no heredoc, no shell interpolation of the
	// path or of any user-derived value inside the XML; see writeRemoteFile).
	if err := vp.writeRemoteFile(ctx, remotePath, []byte(domainXML)); err != nil {
		return fmt.Errorf("failed to create domain definition file: %w", err)
	}

	log.Printf("INFO Created domain definition file: %s", remotePath)
	return nil
}

// defineDomain defines the domain in libvirt using the XML file on the host
// backed by vp (the single host, or the leased target host in clustered mode).
func (p *Provider) defineDomain(ctx context.Context, vp *VirshProvider, domainName string) error {
	// Define domain from XML file
	remotePath := fmt.Sprintf("/tmp/%s-domain.xml", domainName)

	result, err := vp.runRemoteVirshCommand(ctx, "define", remotePath)
	if err != nil {
		return fmt.Errorf("failed to define domain: %w, output: %s", err, result.Stderr)
	}

	// Clean up temporary XML file
	_, cleanupErr := vp.runVirshCommand(ctx, "!", "rm", "-f", remotePath)
	if cleanupErr != nil {
		log.Printf("WARN Failed to cleanup domain XML file: %v", cleanupErr)
	}

	log.Printf("INFO Successfully defined domain: %s", domainName)
	return nil
}

// SnapshotCreate creates a VM snapshot using virsh
func (p *Provider) SnapshotCreate(ctx context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	vmID := req.VM.ID
	log.Printf("INFO Creating snapshot for VM: %s", vmID)

	if p.virshProvider == nil {
		return contracts.SnapshotCreateResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Generate snapshot name if not provided
	snapshotName := req.NameHint
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snapshot-%d", time.Now().Unix())
	}

	// Sanitize snapshot name for virsh
	snapshotName = sanitizeSnapshotName(snapshotName)

	// Prepare snapshot description
	description := req.Description
	if description == "" {
		description = fmt.Sprintf("Snapshot created by VirtRigaud at %s", time.Now().Format(time.RFC3339))
	}

	// Check if domain exists and get its state
	domainState, err := p.virshProvider.getDomainState(ctx, vmID)
	if err != nil {
		return contracts.SnapshotCreateResponse{}, contracts.NewRetryableError("failed to get domain state", err)
	}

	log.Printf("INFO Domain %s is in state: %s", vmID, domainState)

	// Build virsh snapshot-create-as command
	args := []string{
		"snapshot-create-as",
		vmID,
		snapshotName,
		"--description", description,
		"--atomic", // Ensure atomic operation
	}

	// Determine snapshot type based on domain state and request
	if req.IncludeMemory && domainState == "running" {
		// Memory snapshot (full system checkpoint including RAM)
		log.Printf("INFO Creating memory snapshot (includes RAM state)")
		// No --disk-only flag = full snapshot with memory
	} else {
		// Disk-only snapshot (faster, no memory state)
		log.Printf("INFO Creating disk-only snapshot")
		args = append(args, "--disk-only")
	}

	// Execute snapshot creation
	result, err := p.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return contracts.SnapshotCreateResponse{}, fmt.Errorf("failed to create snapshot: %w", err)
	}

	log.Printf("INFO Snapshot created successfully: %s\nOutput: %s", snapshotName, result.Stdout)

	// Return snapshot ID (synchronous operation for libvirt)
	return contracts.SnapshotCreateResponse{
		SnapshotId: snapshotName,
		// No task reference - libvirt snapshots are synchronous
	}, nil
}

// SnapshotDelete deletes a VM snapshot using virsh
func (p *Provider) SnapshotDelete(ctx context.Context, vm contracts.VMRef, snapshotId string) (taskRef string, err error) {
	vmId := vm.ID
	log.Printf("INFO Deleting snapshot %s from VM: %s", snapshotId, vmId)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if snapshot exists
	exists, err := p.virshProvider.snapshotExists(ctx, vmId, snapshotId)
	if err != nil {
		return "", fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		log.Printf("WARN Snapshot %s does not exist, considering deletion successful", snapshotId)
		return "", nil
	}

	// Delete the snapshot
	args := []string{
		"snapshot-delete",
		vmId,
		snapshotId,
	}

	result, err := p.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("failed to delete snapshot: %w", err)
	}

	log.Printf("INFO Snapshot deleted successfully: %s\nOutput: %s", snapshotId, result.Stdout)

	// Return empty task reference (synchronous operation)
	return "", nil
}

// SnapshotRevert reverts a VM to a snapshot using virsh
func (p *Provider) SnapshotRevert(ctx context.Context, vm contracts.VMRef, snapshotId string) (taskRef string, err error) {
	vmId := vm.ID
	log.Printf("INFO Reverting VM %s to snapshot: %s", vmId, snapshotId)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if snapshot exists
	exists, err := p.virshProvider.snapshotExists(ctx, vmId, snapshotId)
	if err != nil {
		return "", fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		return "", fmt.Errorf("snapshot %s does not exist", snapshotId)
	}

	// Get current domain state
	domainState, err := p.virshProvider.getDomainState(ctx, vmId)
	if err != nil {
		return "", fmt.Errorf("failed to get domain state: %w", err)
	}

	log.Printf("INFO Domain %s current state: %s", vmId, domainState)

	// Revert to snapshot
	args := []string{
		"snapshot-revert",
		vmId,
		snapshotId,
		"--force", // Force revert even if domain is running
	}

	// If domain was running, keep it running after revert
	if domainState == "running" {
		args = append(args, "--running")
	}

	result, err := p.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("failed to revert to snapshot: %w", err)
	}

	log.Printf("INFO Successfully reverted to snapshot: %s\nOutput: %s", snapshotId, result.Stdout)

	// Return empty task reference (synchronous operation)
	return "", nil
}

// TaskStatus returns the status of a task (libvirt operations are mostly synchronous)
func (p *Provider) TaskStatus(ctx context.Context, taskRef string) (contracts.TaskStatus, error) {
	// LibVirt operations are synchronous, so if we have a taskRef, it's completed
	return contracts.TaskStatus{
		IsCompleted: true,
		Error:       "",
		Message:     "Task completed",
	}, nil
}

// GetDiskInfo retrieves detailed information about a VM's disk
func (p *Provider) GetDiskInfo(ctx context.Context, req contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	log.Printf("INFO Getting disk info for VM: %s", req.VM.ID)

	if p.virshProvider == nil {
		return contracts.GetDiskInfoResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	storageProvider := NewStorageProvider(p.virshProvider)

	// Resolve the primary disk path + format from the LIVE domain topology
	// (domblklist via getDomainDiskPaths), NOT the fragile "<vmid>-disk" volume
	// guess — that guess only holds for VirtRigaud-created volumes and fails for
	// adopted/externally-created VMs (Bug G; same class as the clone #207 fix).
	// resolvePrimaryDisk also skips cloud-init/CDROM devices.
	diskPath, format, err := p.resolvePrimaryDisk(ctx, req.VM.ID, storageProvider)
	if err != nil {
		return contracts.GetDiskInfoResponse{}, fmt.Errorf("failed to resolve primary disk: %w", err)
	}
	// An explicit disk path (DiskId carrying a path) overrides the primary.
	if req.DiskId != "" && strings.Contains(req.DiskId, "/") {
		diskPath = req.DiskId
	}

	// Read virtual + actual size (and confirm format) from the disk file itself
	// via qemu-img — read-only with -U so a still-running source's write lock is
	// ignored. Best-effort: sizes default to 0 (status-only) if it fails.
	var virtualSize, actualSize int64
	if res, qerr := p.virshProvider.runVirshCommand(ctx, "!", "qemu-img", "info", "-U", "--output=json", diskPath); qerr == nil {
		var qi struct {
			VirtualSize int64  `json:"virtual-size"`
			ActualSize  int64  `json:"actual-size"`
			Format      string `json:"format"`
		}
		if jerr := json.Unmarshal([]byte(res.Stdout), &qi); jerr == nil {
			virtualSize, actualSize = qi.VirtualSize, qi.ActualSize
			if qi.Format != "" {
				format = qi.Format
			}
		} else {
			log.Printf("WARN Failed to parse qemu-img info for %s: %v", diskPath, jerr)
		}
	} else {
		log.Printf("WARN qemu-img info failed for %s (sizes default to 0): %v", diskPath, qerr)
	}

	// Get snapshots for this domain
	snapshots, err := p.virshProvider.listSnapshots(ctx, req.VM.ID)
	if err != nil {
		log.Printf("WARN Failed to list snapshots: %v", err)
		snapshots = []string{}
	}

	response := contracts.GetDiskInfoResponse{
		DiskId:           diskPath,
		Format:           format,
		VirtualSizeBytes: virtualSize,
		ActualSizeBytes:  actualSize,
		Path:             diskPath,
		IsBootable:       true, // resolvePrimaryDisk returns the primary/boot disk
		Snapshots:        snapshots,
		Metadata: map[string]string{
			"pool": "default",
			"type": "file",
		},
	}

	log.Printf("INFO Disk info retrieved: path=%s (format=%s, virtual=%d bytes, actual=%d bytes)",
		response.Path, response.Format, response.VirtualSizeBytes, response.ActualSizeBytes)

	return response, nil
}

// ExportDisk exports a VM disk for migration
func (p *Provider) ExportDisk(ctx context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	log.Printf("INFO Exporting disk for VM: %s to %s", req.VM.ID, req.DestinationURL)

	if p.virshProvider == nil {
		return contracts.ExportDiskResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Get disk information first
	diskInfo, err := p.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{
		VM:         req.VM,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return contracts.ExportDiskResponse{}, fmt.Errorf("failed to get disk info: %w", err)
	}

	// Validate format compatibility
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = diskInfo.Format // Use source format
	}
	if targetFormat != "qcow2" && targetFormat != "raw" {
		return contracts.ExportDiskResponse{}, fmt.Errorf("unsupported export format: %s (libvirt supports qcow2, raw)", targetFormat)
	}

	exportId := fmt.Sprintf("export-%s-%d", req.VM.ID, time.Now().Unix())
	diskPath := diskInfo.Path

	// If snapshot is specified, use snapshot path
	if req.SnapshotId != "" {
		// For snapshot export, we need to find the snapshot backing file
		// For now, we'll use the same path (snapshot-aware export would be more complex)
		log.Printf("INFO Exporting from snapshot: %s", req.SnapshotId)
	}

	// Conversion is needed when the export format differs from the source, OR
	// when compression is requested for a compressible target (qcow2): qemu-img
	// applies `-c` only during a convert pass, so we force one to honor
	// req.Compress even when the format is unchanged (#199). Raw cannot be
	// compressed, so compression of a raw target is a no-op (qemu-img ignores
	// `-c` for raw) and does not force a conversion.
	needsConversion := (targetFormat != diskInfo.Format) ||
		(req.Compress && targetFormat == "qcow2")

	var uploadPath string
	var cleanup func()

	if needsConversion {
		// Convert disk format using qemu-img
		log.Printf("INFO Converting disk from %s to %s", diskInfo.Format, targetFormat)
		tempPath := fmt.Sprintf("/tmp/%s.%s", exportId, targetFormat)

		// Use diskutil package for conversion
		qemuImg := diskutil.NewQemuImg()
		err := qemuImg.Convert(ctx, diskutil.ConvertOptions{
			SourcePath:        diskPath,
			DestinationPath:   tempPath,
			SourceFormat:      diskutil.SupportedFormat(diskInfo.Format),
			DestinationFormat: diskutil.SupportedFormat(targetFormat),
			Compression:       req.Compress, // honor the caller's request (#199); diskutil applies it only to compressible formats (qcow2)
		})
		if err != nil {
			return contracts.ExportDiskResponse{}, fmt.Errorf("failed to convert disk format: %w", err)
		}

		uploadPath = tempPath
		cleanup = func() {
			_ = os.Remove(tempPath)
		}
		defer cleanup()
	} else {
		uploadPath = diskPath
	}

	// Upload to destination using PVC storage layer
	log.Printf("INFO Uploading disk to: %s", req.DestinationURL)

	// Configure storage client
	// URL format: pvc://<pvc-name>/<file-path>
	// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
	// Extract PVC name from URL to construct the correct mount path
	pvcName, err := extractPVCNameFromURL(req.DestinationURL)
	if err != nil {
		return contracts.ExportDiskResponse{}, fmt.Errorf("failed to extract PVC name from URL: %w", err)
	}

	// Mount path matches where the controller mounts PVCs: /mnt/migration-storage/<pvc-name>
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)

	storageConfig := storage.StorageConfig{
		Type:      "pvc",
		MountPath: mountPath,
	}

	// Create PVC storage client
	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return contracts.ExportDiskResponse{}, fmt.Errorf("failed to create storage client: %w", err)
	}
	defer storageClient.Close()

	// Upload the disk
	uploadReq := storage.UploadRequest{
		SourcePath:     uploadPath,
		DestinationURL: req.DestinationURL,
		ContentLength:  diskInfo.ActualSizeBytes,
		ProgressCallback: func(transferred, total int64) {
			if total > 0 {
				progress := float64(transferred) / float64(total) * 100
				log.Printf("INFO Upload progress: %.2f%% (%d/%d bytes)", progress, transferred, total)
			}
		},
	}

	uploadResp, err := storageClient.Upload(ctx, uploadReq)
	if err != nil {
		return contracts.ExportDiskResponse{}, fmt.Errorf("failed to upload disk: %w", err)
	}

	checksum := uploadResp.Checksum
	log.Printf("INFO Disk export completed: %s (checksum=%s, uploaded=%d bytes)", exportId, checksum, uploadResp.BytesTransferred)

	response := contracts.ExportDiskResponse{
		ExportId:           exportId,
		TaskRef:            "", // Synchronous operation
		EstimatedSizeBytes: diskInfo.ActualSizeBytes,
		Checksum:           checksum,
	}

	return response, nil
}

// ImportDisk imports a disk from an external source
func (p *Provider) ImportDisk(ctx context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	log.Printf("INFO Importing disk from %s to storage: %s", req.SourceURL, req.StorageHint)

	if p.virshProvider == nil {
		return contracts.ImportDiskResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	storageProvider := NewStorageProvider(p.virshProvider)

	// Determine storage pool
	storagePool := "default"
	if req.StorageHint != "" {
		storagePool = req.StorageHint
	}

	// Ensure storage pool exists and is active
	if err := storageProvider.ensurePoolActive(ctx, storagePool); err != nil {
		return contracts.ImportDiskResponse{}, fmt.Errorf("failed to ensure storage pool: %w", err)
	}

	// Determine target format
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = "qcow2" // Default to qcow2
	}
	if targetFormat != "qcow2" && targetFormat != "raw" {
		return contracts.ImportDiskResponse{}, fmt.Errorf("unsupported import format: %s (libvirt supports qcow2, raw)", targetFormat)
	}

	// Generate disk ID
	diskId := req.TargetName
	if diskId == "" {
		diskId = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}

	// Download to temporary location
	tempPath := fmt.Sprintf("/tmp/%s-download.img", diskId)

	log.Printf("INFO Downloading disk from %s to %s", req.SourceURL, tempPath)

	// Configure storage client
	// URL format: pvc://<pvc-name>/<file-path>
	// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
	// Extract PVC name from URL to construct the correct mount path
	pvcName, err := extractPVCNameFromURL(req.SourceURL)
	if err != nil {
		return contracts.ImportDiskResponse{}, fmt.Errorf("failed to extract PVC name from URL: %w", err)
	}

	// Mount path matches where the controller mounts PVCs: /mnt/migration-storage/<pvc-name>
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)

	storageConfig := storage.StorageConfig{
		Type:      "pvc",
		MountPath: mountPath,
	}

	// Create PVC storage client
	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return contracts.ImportDiskResponse{}, fmt.Errorf("failed to create storage client: %w", err)
	}
	defer storageClient.Close()

	// Download the disk
	downloadReq := storage.DownloadRequest{
		SourceURL:        req.SourceURL,
		DestinationPath:  tempPath,
		VerifyChecksum:   req.VerifyChecksum,
		ExpectedChecksum: req.ExpectedChecksum,
		ProgressCallback: func(transferred, total int64) {
			if total > 0 {
				progress := float64(transferred) / float64(total) * 100
				log.Printf("INFO Download progress: %.2f%% (%d/%d bytes)", progress, transferred, total)
			}
		},
	}

	downloadResp, err := storageClient.Download(ctx, downloadReq)
	if err != nil {
		return contracts.ImportDiskResponse{}, fmt.Errorf("failed to download disk: %w", err)
	}

	checksum := downloadResp.Checksum
	fileSizeBytes := downloadResp.ContentLength

	log.Printf("INFO Download completed: %d bytes, checksum=%s", fileSizeBytes, checksum)

	// Cleanup temp file on exit
	defer func() {
		_, _ = p.virshProvider.runVirshCommand(ctx, "!", "rm", "-f", tempPath)
	}()

	// Create volume in storage pool from the downloaded file
	log.Printf("INFO Creating volume %s in pool %s from downloaded file", diskId, storagePool)

	// First, copy the file to the storage pool directory
	poolPath := "/var/lib/libvirt/images" // Default path, should be queried from pool
	diskPath := fmt.Sprintf("%s/%s.%s", poolPath, diskId, targetFormat)

	// Copy with conversion using diskutil
	log.Printf("INFO Converting disk to target format: %s", targetFormat)
	qemuImg := diskutil.NewQemuImg()
	err = qemuImg.Convert(ctx, diskutil.ConvertOptions{
		SourcePath:        tempPath,
		DestinationPath:   diskPath,
		DestinationFormat: diskutil.SupportedFormat(targetFormat),
		Compression:       false, // No compression for migration (faster)
	})
	if err != nil {
		return contracts.ImportDiskResponse{}, fmt.Errorf("failed to convert/copy disk: %w", err)
	}

	// The disk is now in place at diskPath
	// For libvirt, the file being in the correct location is sufficient
	// The volume will be referenced when creating a VM with this disk
	finalPath := diskPath

	log.Printf("INFO Disk import completed: %s (path=%s)", diskId, finalPath)

	response := contracts.ImportDiskResponse{
		DiskId:          diskId,
		Path:            finalPath,
		TaskRef:         "", // Synchronous operation
		ActualSizeBytes: fileSizeBytes,
		Checksum:        checksum,
	}

	return response, nil
}

// ListVMs returns all VMs managed by this provider
func (p *Provider) ListVMs(ctx context.Context) ([]contracts.VMInfo, error) {
	log.Printf("INFO Listing all virtual machines")

	if p.virshProvider == nil {
		return nil, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// List all domains
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list domains: %w", err)
	}

	log.Printf("INFO Found %d domains", len(domains))

	var vmInfos []contracts.VMInfo

	for _, domain := range domains {
		// Everything ListVMs/adoption needs (cpu, memory, disk path+format, NIC
		// MAC, uuid) lives in the domain XML, so one `virsh dumpxml` per domain
		// replaces the old dominfo + 5 monitoring calls + 2 guest-agent probes
		// that blocked on agent timeouts and blew the gRPC deadline. Power state
		// comes from `virsh list` (already fetched in listDomains). IPs are not
		// needed here — the normal VM reconcile discovers them after adoption.
		raw, err := p.virshProvider.runVirshCommand(ctx, "dumpxml", domain.Name)
		if err != nil {
			log.Printf("WARN Failed to dump XML for %s: %v", domain.Name, err)
			continue
		}
		dx, err := parseDomainXML(raw.Stdout)
		if err != nil {
			log.Printf("WARN Failed to parse XML for %s: %v", domain.Name, err)
			continue
		}

		powerState := string(p.mapLibvirtPowerState(domain.State))

		cpu := dx.VCPU
		if cpu == 0 {
			cpu = 1 // Default to 1 CPU
		}
		memoryMiB, err := dx.MemoryMiB()
		if err != nil {
			log.Printf("WARN %s: %v; skipping", domain.Name, err)
			continue
		}

		providerRaw := map[string]string{
			"domain_name": domain.Name,
			"domain_id":   domain.ID,
			"state":       domain.State,
			"power_state": powerState,
		}
		if dx.UUID != "" {
			providerRaw["uuid"] = dx.UUID
		}

		vmInfos = append(vmInfos, contracts.VMInfo{
			ID:          domain.Name, // Use domain name as ID
			Name:        domain.Name,
			PowerState:  powerState,
			CPU:         cpu,
			MemoryMiB:   memoryMiB,
			Disks:       dx.Disks(domain.Name),
			Networks:    dx.Networks(),
			ProviderRaw: providerRaw,
		})
	}

	// ADR-0008 PR 4c: when the list family is in shadow mode, also run the go-libvirt
	// ListVMs and meter any semantic divergence against this authoritative virsh
	// answer. This returns immediately (the shadow runs on a detached, time-bounded,
	// panic-isolated goroutine) and NEVER alters what is returned here — reads do not
	// flip to native until PR 5. No-op when shadow is off (the default), so pure virsh
	// is unchanged.
	p.maybeShadowList(ctx, vmInfos)

	return vmInfos, nil
}
