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

package libvirt

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/cloneidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A clone's own cloud-init seed (R26), on both topologies.
//
// A clone of a VM that boots from a NoCloud seed ISO gets a seed of its own,
// made like Create's (PrepareCloudInit: a fresh per-domain directory in the
// host staging directory, named after the CLONE's domain, so the clone's
// Delete and the seed reclaim remove it with the clone) — never the source's
// ISO (single-host clones used to keep pointing at it, coupling the two VMs'
// lifecycles, R24) nor a byte copy of it (clustered clones used to copy it,
// which kept the source's identity).
//
// The seed carries a NEW cloud-init identity: its meta-data instance-id is
// derived from the target VirtualMachine (cloneidentity.InstanceID: its UID),
// and its local-hostname — and the top-level hostname/fqdn keys of a
// #cloud-config user-data — name the target. The clone's disk still records
// the source's instance-id, so on its first boot cloud-init (NoCloud
// datasource) sees a new instance and runs its per-instance modules: the ssh
// module regenerates the SSH host keys (ssh_deletekeys defaults to true) and
// the hostname modules set the clone's name. cloud-init does NOT regenerate
// /etc/machine-id; that needs a sealed source image (docs/libvirt-clones.md).
//
// The clone's user-data is the VMClone's spec.customization.userData.
// cloudInit.inline when set, else the source seed's user-data; the source's
// meta-data keeps every other key, and its network-config and vendor-data
// files are carried as they are. A Clone sent again for the same target
// renders the same seed content.
//
// The source seed is read (isoinfo, from the genisoimage package Create
// already needs) BEFORE anything is copied, and a seed that cannot be read is
// a failed clone, never a clone with the source's identity.

const (
	// seedFileUserData, seedFileMetaData, seedFileNetworkConfig and
	// seedFileVendorData are the NoCloud seed's files a clone carries.
	seedFileUserData      = "user-data"
	seedFileMetaData      = "meta-data"
	seedFileNetworkConfig = "network-config"
	seedFileVendorData    = "vendor-data"

	// isoinfoTool lists and extracts the files of the source's seed ISO.
	isoinfoTool = "isoinfo"

	// maxSeedFileBytes bounds each file read from a source seed. PVE caps a
	// whole seed at 3 MiB; cloud-init user-data is typically a few KiB.
	maxSeedFileBytes = 1 << 20

	// seedFileReadScript extracts one file ($3) of the ISO $1 with the name
	// flag $2 (-R or -J), at most $4 bytes. The extraction runs once to
	// /dev/null first, so a failing isoinfo fails the script (a POSIX
	// pipeline would report head's status). All values are positional
	// parameters, never interpolated into the text.
	seedFileReadScript = `isoinfo "$2" -i "$1" -x "$3" >/dev/null && isoinfo "$2" -i "$1" -x "$3" | head -c "$4"`
)

// seedFileNames are the files read from a source seed, in a fixed order.
var seedFileNames = []string{seedFileUserData, seedFileMetaData, seedFileNetworkConfig, seedFileVendorData}

// isoNameFlags are the isoinfo name modes tried, in order: Rock Ridge
// (genisoimage -rock, as Create and cloud-localds build seeds), then Joliet.
// cloud-init reads a seed's files by these names; a plain ISO 9660 image
// (USER_DAT.;1) is not a working NoCloud seed.
var isoNameFlags = []string{"-R", "-J"}

// cloneSeedPlan is a clone's own seed, decided before anything is copied.
type cloneSeedPlan struct {
	// srcISO is the source's seed ISO in the clone's definition, re-pointed
	// at the clone's own.
	srcISO string
	// config is the clone's seed content.
	config CloudInitConfig
}

// planCloneSeed decides the clone's own seed: nil when the source has none
// (srcISO ""), else the source seed's files on the host behind h with the
// clone's identity. A seed that cannot be read is an error.
func planCloneSeed(ctx context.Context, h hostCommandRunner, srcISO string, req contracts.CloneRequest, domainName string) (*cloneSeedPlan, error) {
	cust, err := cloneidentity.ParseCustomization(req.CustomizeJSON)
	if err != nil {
		return nil, contracts.NewInvalidSpecError("the clone's customization is not valid JSON", err)
	}
	if len(cust.Unapplied) > 0 {
		log.Printf("WARN Clone %s: the libvirt provider does not apply these customization fields: %s",
			domainName, strings.Join(cust.Unapplied, ", "))
	}
	if srcISO == "" {
		if cust.Hostname != "" || cust.Domain != "" || cust.InlineUserData != "" || cust.SecretUserData {
			log.Printf("WARN Clone %s: the source has no cloud-init seed, so the hostname and user-data customization "+
				"are not applied (the libvirt provider gives a clone a seed only when its source has one)", domainName)
		}
		return nil, nil
	}
	files, err := readSeedISO(ctx, h, srcISO)
	if err != nil {
		return nil, err
	}

	hostname := cloneidentity.Hostname(cust, req.TargetVM, req.TargetName)
	localHostname := cloneidentity.LocalHostname(hostname, cust.Domain)
	instanceID := cloneidentity.InstanceID(req.TargetVM, domainName)

	userData := files[seedFileUserData]
	switch {
	case cust.InlineUserData != "":
		userData = cust.InlineUserData
	default:
		if cust.SecretUserData {
			log.Printf("WARN Clone %s: user-data given by a Secret reference cannot be resolved by the provider; "+
				"the clone keeps its source's user-data", domainName)
		}
		var skipped []string
		userData, _, skipped = cloneidentity.RewriteUserDataHostname(userData, hostname, cust.Domain)
		if len(skipped) > 0 {
			log.Printf("WARN Clone %s: the source's user-data sets %s in a form the provider does not rewrite; "+
				"cloud-init prefers it over the clone's meta-data hostname", domainName, strings.Join(skipped, ", "))
		}
	}

	metaData := files[seedFileMetaData]
	if strings.TrimSpace(metaData) == "" {
		// The provider's own meta-data, as Create writes it.
		metaData = (&CloudInitProvider{}).generateMetaData(instanceID, localHostname)
	} else if metaData, err = cloneidentity.RewriteMetaData(metaData, instanceID, localHostname); err != nil {
		return nil, contracts.NewInvalidSpecError(fmt.Sprintf(
			"the clone source's cloud-init seed %s has meta-data the provider cannot give a new identity", srcISO), err)
	}

	return &cloneSeedPlan{srcISO: srcISO, config: CloudInitConfig{
		UserData: userData,
		MetaData: metaData,
		// Names the seed directory after the clone's domain (PrepareCloudInit).
		InstanceID:    domainName,
		Hostname:      hostname,
		NetworkConfig: files[seedFileNetworkConfig],
		VendorData:    files[seedFileVendorData],
	}}, nil
}

// readSeedISO reads the NoCloud files (seedFileNames) of the seed ISO iso on
// the host behind h, as the SSH user. It lists the ISO with each name mode
// (isoNameFlags) and reads the files of the first that shows a user-data or
// meta-data file. A failure to read the ISO is retryable (a host that could
// not be reached, a missing isoinfo); an ISO without NoCloud files, or a file
// over maxSeedFileBytes, is an InvalidSpec. The files' contents (user-data may
// carry secrets) are never logged.
func readSeedISO(ctx context.Context, h hostCommandRunner, iso string) (map[string]string, error) {
	var (
		flag    string
		present []string
		lastErr error
		listed  bool
	)
	for _, f := range isoNameFlags {
		res, err := runHost(ctx, h, isoinfoTool, f, "-f", "-i", iso)
		if err != nil {
			// A Rock Ridge-only image has no Joliet tree to list: that
			// error only matters when no listing succeeded.
			lastErr = seedReadFailure(err)
			continue
		}
		listed = true
		var names []string
		for _, line := range strings.Split(res.Stdout, "\n") {
			if name, ok := strings.CutPrefix(strings.TrimSpace(line), "/"); ok && slices.Contains(seedFileNames, name) {
				names = append(names, name)
			}
		}
		if slices.Contains(names, seedFileUserData) || slices.Contains(names, seedFileMetaData) {
			flag, present = f, names
			break
		}
	}
	if flag == "" {
		if !listed {
			return nil, contracts.NewRetryableError(fmt.Sprintf(
				"could not read the clone source's cloud-init seed %s on the host (is %s, from the genisoimage package, installed?); "+
					"nothing was copied", iso, isoinfoTool), lastErr)
		}
		return nil, contracts.NewInvalidSpecError(fmt.Sprintf(
			"the clone source's cloud-init seed %s holds no NoCloud user-data or meta-data", iso), nil)
	}

	files := make(map[string]string, len(present))
	for _, name := range present {
		res, err := runHost(ctx, h, "sh", "-c", seedFileReadScript, "sh", iso, flag, "/"+name, strconv.Itoa(maxSeedFileBytes+1))
		if err != nil {
			return nil, contracts.NewRetryableError(fmt.Sprintf(
				"could not read %s from the clone source's cloud-init seed %s on the host; nothing was copied", name, iso), seedReadFailure(err))
		}
		if len(res.Stdout) > maxSeedFileBytes {
			return nil, contracts.NewInvalidSpecError(fmt.Sprintf(
				"the %s of the clone source's cloud-init seed %s is larger than %d bytes", name, iso, maxSeedFileBytes), nil)
		}
		files[name] = res.Stdout
	}
	return files, nil
}

// seedReadFailure is the cause a failed read of a source seed is reported
// with: the host command's exit code and stderr only. A read that failed
// after it started printing — the SSH session dropped, or the context was
// cancelled, in the middle of `isoinfo | head -c` — leaves part of the
// source's user-data (which may carry secrets) in the VirshError's Stdout,
// which its Error() prints, and the Clone error mappers log the cause: it is
// never kept. Any other error is reduced to a constant text.
func seedReadFailure(err error) error {
	var ve *VirshError
	switch {
	case errors.As(err, &ve):
		return fmt.Errorf("%w (exit code %d): %s", errSeedReadFailed, ve.ExitCode, strings.TrimSpace(ve.Stderr))
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", errSeedReadFailed, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", errSeedReadFailed, context.DeadlineExceeded)
	}
	return errSeedReadFailed
}

// errSeedReadFailed is the cause of every failed read of a source seed
// (seedReadFailure).
var errSeedReadFailed = errors.New("the host command reading the seed failed")

// writeCloneSeed makes the clone's own seed on vp's host (PrepareCloudInit, in
// the provider's staging directory) and re-points the clone's CD-ROM in
// domainXML from the source's seed at it. It returns the rewritten definition
// and the seed directory ("" and domainXML unchanged when plan is nil). On a
// failure nothing of the seed is left behind.
func (p *Provider) writeCloneSeed(ctx context.Context, vp *VirshProvider, domainXML string, plan *cloneSeedPlan) (string, string, error) {
	if plan == nil {
		return domainXML, "", nil
	}
	cip := NewCloudInitProvider(vp)
	cip.stagingDir = p.stagingDir()
	iso, err := cip.PrepareCloudInit(ctx, plan.config)
	if err != nil {
		return "", "", fmt.Errorf("make the clone's cloud-init seed: %w", err)
	}
	out, ok := replaceSourceFile(domainXML, plan.srcISO, iso)
	if !ok {
		cip.CleanupCloudInit(ctx, iso)
		return "", "", fmt.Errorf("re-point the clone's cloud-init CD-ROM: seed %q not found in the clone's definition", plan.srcISO)
	}
	return out, filepath.Dir(iso), nil
}
