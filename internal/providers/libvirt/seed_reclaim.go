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
	"fmt"
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
// references a file in it any more. When a clone is the last domain to
// reference its source's seed, deleting the clone therefore removes the
// SOURCE's seed directory: that is intended.

const (
	// seedDirNameSeparator follows the domain name in a seed directory's name:
	// virtrigaud-cloudinit-<domain>.<random>.
	seedDirNameSeparator = "."

	// orphanSeedMinAge is how old a seed directory found only by its NAME (not
	// referenced by the deleted domain's definition) must be before it is
	// removed: a create of that name that is still in flight has prepared its
	// seed but not yet defined the domain, and its seed must not be taken from
	// under it. Single-host Create and Delete of one domain name are also
	// serialized (lockDomain); this age bound is the second line, for a create
	// that is not in this process. A seed the deleted domain's own definition
	// referenced (and the plan kept) is removed whatever its age.
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
// directories are listed (`find -type d` does not follow a symlink; -H lets the
// staging directory itself be one), and with a positive minAge only those not
// modified for that long. The name filter runs here, not in find, so no part
// of the domain name is ever a glob.
func ownSeedDirs(ctx context.Context, h hostCommandRunner, stagingDir, domainName string, minAge time.Duration) ([]string, error) {
	argv := []string{"find", "-H", stagingDir, "-mindepth", "1", "-maxdepth", "1", "-type", "d",
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

// isRealDir reports whether path is a directory itself, not a symlink to one
// (`find -maxdepth 0 -type d` does not follow it). An error is returned when
// the host command fails, so the caller can keep the path.
func isRealDir(ctx context.Context, h hostCommandRunner, path string) (bool, error) {
	res, err := runHost(ctx, h, "find", path, "-maxdepth", "0", "-type", "d", "-print0")
	if err != nil {
		return false, err
	}
	return len(splitNULList(res.Stdout)) == 1, nil
}

// seedDirsReferenced reports, for each of dirs (absolute, as they will be
// removed), whether any domain defined on the host behind h references a file
// in it — or in a directory it lies under that a domain shares in. It reads
// each domain's PERSISTENT definition (`virsh dumpxml --inactive`: what the
// domain starts from) and canonicalizes the candidate directories and every
// referenced path in one `realpath` call, so a staging directory reached through
// a symlink is compared like for like. It fails closed: any host failure is an
// error, never "unreferenced".
func seedDirsReferenced(ctx context.Context, h hostCommandRunner, dirs []string) (map[string]bool, error) {
	uuids, err := listDomainUUIDs(ctx, h)
	if err != nil {
		return nil, err
	}
	var files, shared []string
	for _, uuid := range uuids {
		xmlRes, err := h.runVirshCommand(ctx, "dumpxml", uuid, "--inactive")
		if err != nil {
			gone, gerr := domainGone(ctx, h, uuid)
			if gerr != nil {
				return nil, gerr
			}
			if gone {
				continue
			}
			return nil, hostCheckFailed(fmt.Sprintf("read definition of domain %s", uuid), err)
		}
		refs, err := parseDomainPathRefs(xmlRes.Stdout)
		if err != nil {
			return nil, hostCheckFailed(fmt.Sprintf("parse definition of domain %s", uuid), err)
		}
		files = append(files, refs.files...)
		shared = append(shared, refs.dirs...)
	}
	all := append(append(append([]string(nil), dirs...), files...), shared...)
	canon, err := canonicalizeOnHost(ctx, h, all)
	if err != nil {
		return nil, err
	}
	set := inUseSet{files: map[string]bool{}}
	for _, f := range canon[len(dirs) : len(dirs)+len(files)] {
		set.files[f] = true
	}
	set.dirs = canon[len(dirs)+len(files):]
	used := make(map[string]bool, len(dirs))
	for i, dir := range dirs {
		used[dir] = set.contains(canon[i]) || otherDomains{{refs: set}}.useUnder(canon[i])
	}
	return used, nil
}

// reclaimSeedDirs removes the cloud-init seed directories a deleted domain left
// behind on the host behind h: extra, a seed directory its definition
// referenced that the delete plan kept because another domain used it (removed
// whatever its age), and the directories named after exactly this domain
// (ownSeedDirs; with minAge, only the old ones — see orphanSeedMinAge). A
// directory is removed only when it is a real directory directly inside the
// staging directory and no domain defined on the host references a file in it
// (seedDirsReferenced); when that cannot be established nothing is removed.
//
// It never fails the delete: every problem is logged at Warn and the directory
// stays (a leaked seed directory is recoverable, a domain whose CD-ROM points
// at a removed ISO is not). It never follows a symlink.
func (p *Provider) reclaimSeedDirs(ctx context.Context, h hostCommandRunner, domainName, extra string, minAge time.Duration) {
	staging := p.stagingDir()
	var candidates []string
	if extra != "" && isSeedDirIn(extra, staging) {
		extra = filepath.Clean(extra)
		real, err := isRealDir(ctx, h, extra)
		switch {
		case err != nil:
			log.Printf("WARN Keeping the cloud-init seed directory %s of deleted domain %s: could not inspect it: %v", extra, domainName, err)
		case !real:
			log.Printf("WARN Keeping %s: it is not a directory (or is a symlink), so it is not a seed directory of deleted domain %s", extra, domainName)
		default:
			candidates = append(candidates, extra)
		}
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

	used, err := seedDirsReferenced(ctx, h, candidates)
	if err != nil {
		log.Printf("WARN Keeping the cloud-init seed directories %v of deleted domain %s: could not verify that no domain uses them: %v",
			candidates, domainName, err)
		return
	}
	for _, dir := range candidates {
		if used[dir] {
			log.Printf("WARN Keeping the cloud-init seed directory %s of deleted domain %s: a domain still references it", dir, domainName)
			continue
		}
		log.Printf("INFO Removing leftover cloud-init seed directory %s of deleted domain %s", dir, domainName)
		removeHostPath(ctx, h, dir, true)
	}
}
