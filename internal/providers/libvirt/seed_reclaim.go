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
	"log"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Reclaiming a deleted domain's cloud-init seed directory (R24).
//
// A single-host Clone does not make the clone a seed of its own: its CD-ROM
// keeps pointing at the SOURCE VM's seed ISO (rewriteDomainXMLForClone). The
// delete plan therefore keeps a seed directory while another domain still
// references a file in it (planDomainDeletion) — a domain whose CD-ROM file is
// gone no longer starts. That decision is made BEFORE the domain is undefined,
// and each of two deletes that run side by side (a source VM and its clone, as
// when a namespace is cleaned up) sees the other still defined: both keep the
// directory, both then undefine, and nothing is left to remove it. A delete
// that is retried after it already undefined the domain, or a domain whose
// CD-ROM no longer names the seed, leaks it the same way.
//
// reclaimSeedDirs closes that gap AFTER the undefine: it removes the seed
// directories the deleted domain left behind, each only when no defined domain
// references a file in it any more.

const (
	// seedDirNameSeparator follows the domain name in a seed directory's name:
	// virtrigaud-cloudinit-<domain>.<random>.
	seedDirNameSeparator = "."

	// orphanSeedMinAge is how old a domain's seed directory must be before the
	// cleanup of a domain that is ALREADY ABSENT may remove it: a create of that
	// name that is still in flight has prepared its seed but not yet defined the
	// domain, and its seed must not be taken from under it. (A domain that was
	// present when the delete began has no create in flight.)
	orphanSeedMinAge = 5 * time.Minute
)

// seedDirRandomLen is the length of the random part mktemp puts in a seed
// directory's name (the 'X's of mktempTemplateSuffix).
var seedDirRandomLen = len(mktempTemplateSuffix)

// isOwnSeedDirName reports whether name (a base name, no directory) is a seed
// directory PrepareCloudInit made for exactly the domain domainName:
// "virtrigaud-cloudinit-" + domainName + "." + the random characters mktemp
// puts in place of its template's 'X's (letters and digits, seedDirRandomLen of
// them). The match is exact on the domain name: "virtrigaud-cloudinit-ns.web.
// AbCdEfGhIj" belongs to "ns.web" and not to "ns.webapp", "ns" or "ns.web.x".
func isOwnSeedDirName(name, domainName string) bool {
	if domainName == "" {
		return false
	}
	rest, ok := strings.CutPrefix(name, cloudInitSeedDirPrefix+domainName+seedDirNameSeparator)
	if !ok || len(rest) != seedDirRandomLen {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// isSeedDirIn reports whether dir is a per-create seed directory directly
// inside stagingDir: the only kind of directory the seed cleanup may remove.
func isSeedDirIn(dir, stagingDir string) bool {
	dir = filepath.Clean(dir)
	return filepath.Dir(dir) == filepath.Clean(stagingDir) &&
		strings.HasPrefix(filepath.Base(dir), cloudInitSeedDirPrefix) &&
		len(filepath.Base(dir)) > len(cloudInitSeedDirPrefix)
}

// ownSeedDirs lists, on the host behind h, the seed directories directly inside
// stagingDir that are domainName's own (isOwnSeedDirName). Only real
// directories are listed (`find -type d` does not follow a symlink), and with a
// positive minAge only those not modified for that long. The name filter runs
// here, not in find, so no part of the domain name is ever a glob.
func ownSeedDirs(ctx context.Context, h hostCommandRunner, stagingDir, domainName string, minAge time.Duration) ([]string, error) {
	argv := []string{"find", stagingDir, "-mindepth", "1", "-maxdepth", "1", "-type", "d",
		"-name", cloudInitSeedDirPrefix + "*"}
	if minAge > 0 {
		argv = append(argv, "-mmin", "+"+strconv.FormatInt(int64(minAge/time.Minute), 10))
	}
	argv = append(argv, "-print0")
	res, err := runHost(ctx, h, argv...)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, p := range splitNULList(res.Stdout) {
		if filepath.Dir(p) == filepath.Clean(stagingDir) && isOwnSeedDirName(filepath.Base(p), domainName) {
			dirs = append(dirs, p)
		}
	}
	return dirs, nil
}

// reclaimSeedDirs removes the cloud-init seed directories a deleted domain left
// behind on the host behind h: its own (ownSeedDirs; with minAge, only the old
// ones — see orphanSeedMinAge) and extra, a seed directory its definition
// referenced that the delete plan kept because another domain used it. A
// directory is removed only when no domain defined on the host references a
// file in it; when that cannot be established nothing is removed.
//
// It never fails the delete: every problem is logged at Warn and the directory
// stays (a leaked seed directory is recoverable, a domain whose CD-ROM points
// at a removed ISO is not). It never removes anything but a seed directory
// directly inside the staging directory, and does not follow symlinks.
func (p *Provider) reclaimSeedDirs(ctx context.Context, h hostCommandRunner, domainName, extra string, minAge time.Duration) {
	staging := p.stagingDir()
	var candidates []string
	if extra != "" && isSeedDirIn(extra, staging) {
		candidates = append(candidates, filepath.Clean(extra))
	}
	own, err := ownSeedDirs(ctx, h, staging, domainName, minAge)
	if err != nil {
		log.Printf("WARN Could not look for leftover cloud-init seed directories of %s: %v", domainName, err)
	}
	for _, d := range own {
		if !slices.Contains(candidates, d) {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return
	}

	refs, err := domainRefsOnHost(ctx, h, "")
	if err != nil {
		log.Printf("WARN Keeping the cloud-init seed directories %v of deleted domain %s: could not verify that no domain uses them: %v",
			candidates, domainName, err)
		return
	}
	for _, dir := range candidates {
		if otherDomains(refs).useUnder(dir) {
			log.Printf("WARN Keeping the cloud-init seed directory %s of deleted domain %s: a domain still references it", dir, domainName)
			continue
		}
		log.Printf("INFO Removing leftover cloud-init seed directory %s of deleted domain %s", dir, domainName)
		removeHostPath(ctx, h, dir, true)
	}
}
