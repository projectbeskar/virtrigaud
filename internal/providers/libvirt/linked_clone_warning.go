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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Warning for the source of an existing linked clone.
//
// Linked clones are disabled (linkedClonesDisabledMessage), but ones made by an
// earlier release keep working, and powering their SOURCE on while they are
// shut off corrupts them: the source's guest writes to the file they read
// their unwritten blocks from. William chose not to refuse the start. Instead,
// after every start (Power On, Reboot) the provider counts the domains on the
// host that depend on the started domain's disks (diskDependents — the same
// check the delete guard runs) and remembers the count by the domain's UUID
// (a later domain reusing the name never inherits it); Describe reports it in
// ProviderRaw (contracts.ProviderRawLinkedCloneDependentsKey), from which the
// manager sets the LinkedClonesDependOnDisk condition and a Warning event.
// Deleting the domain drops its count. The count is only as fresh as the last
// start, and is lost when the provider restarts (Describe then reports
// nothing, and the manager keeps what it had).

// linkedCloneCheckTimeout bounds the dependents count after a start, which
// never fails or holds up the power operation for long.
const linkedCloneCheckTimeout = 30 * time.Second

// linkedCloneDependents is the provider's record of the dependents count per
// domain UUID, taken at the domain's last start. The zero value is ready to
// use.
type linkedCloneDependents struct {
	mu     sync.Mutex
	byUUID map[string]int
}

// uuidKey normalizes a domain UUID (virsh prints it lower-case; compare
// case-insensitively).
func uuidKey(uuid string) string {
	return strings.ToLower(strings.TrimSpace(uuid))
}

// set records n dependents for the domain with this UUID.
func (l *linkedCloneDependents) set(uuid string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUUID == nil {
		l.byUUID = map[string]int{}
	}
	l.byUUID[uuidKey(uuid)] = n
}

// get returns the recorded count for the domain with this UUID, if any.
func (l *linkedCloneDependents) get(uuid string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.byUUID[uuidKey(uuid)]
	return n, ok
}

// drop forgets the domain with this UUID (it was deleted).
func (l *linkedCloneDependents) drop(uuid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byUUID, uuidKey(uuid))
}

// recordLinkedCloneDependents counts, right after domain d was started on vp's
// host, the other domains that use one of its disks as a disk or backing file,
// and records the count by the domain's UUID for Describe. It only warns — the
// start has happened — and a count that cannot be taken (in at most
// linkedCloneCheckTimeout) is logged and leaves the record as it was.
func (p *Provider) recordLinkedCloneDependents(ctx context.Context, vp *VirshProvider, d domainTarget) {
	cctx, cancel := context.WithTimeout(ctx, linkedCloneCheckTimeout)
	defer cancel()
	res, err := vp.runVirshCommand(cctx, "dumpxml", d.handle)
	if err != nil {
		log.Printf("WARN Could not check domain %s for linked clones after its start: %v", d.name, err)
		return
	}
	doc, err := parseDomainDisks(res.Stdout)
	if err != nil {
		log.Printf("WARN Could not check domain %s for linked clones after its start: %v", d.name, err)
		return
	}
	if strings.TrimSpace(doc.UUID) == "" {
		log.Printf("WARN Could not check domain %s for linked clones after its start: its definition has no UUID", d.name)
		return
	}
	n, err := diskDependents(cctx, vp, doc.UUID, doc.diskFiles())
	if err != nil {
		log.Printf("WARN Could not check domain %s for linked clones after its start: %v", d.name, err)
		return
	}
	p.linkedDeps.set(doc.UUID, n)
	if n > 0 {
		log.Printf("WARN Domain %s was started while %d other domain(s) use its disk as their backing file (linked clones): "+
			"powering it on while they are shut off corrupts them", d.name, n)
	}
}

// reportLinkedCloneDependents adds the recorded dependents count of the domain
// with this UUID, if any, to a Describe response's ProviderRaw.
func (p *Provider) reportLinkedCloneDependents(uuid string, raw map[string]string) {
	if raw == nil || strings.TrimSpace(uuid) == "" {
		return
	}
	if n, ok := p.linkedDeps.get(uuid); ok {
		raw[contracts.ProviderRawLinkedCloneDependentsKey] = strconv.Itoa(n)
	}
}
