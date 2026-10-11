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

package vsphere

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Which VMs use a datastore's files (disk_guard.go, rule 3 and its uses).
//
// The in-use check reads only the VMs and templates vCenter lists as having
// files on the datastore that holds the file (Datastore.vm), and on every
// datastore that is the same storage under another name: the same
// summary.url, or the same NFS server and export. One VM whose configuration
// vCenter cannot load fails the check closed — but only for operations on a
// datastore that VM has files on, not for the whole vCenter.
//
// A scan is shared by concurrent callers (single flight) and reused for
// diskScanTTL. Every operation of this provider that can change who uses a
// datastore's files (an in-place attach, an import) invalidates the
// datastore's entry when it is done, and the re-check before a failed
// import's cleanup always scans afresh, so the cache never hides this
// provider's own work; it can hide a change made through vCenter by someone
// else for at most diskScanTTL.

const (
	// diskScanTTL is how long a datastore's scan is reused.
	diskScanTTL = 2 * time.Second
	// datastoreMoType is the vSphere managed object type of a datastore.
	datastoreMoType = "Datastore"
	// propSummary, propInfo and propVM are the datastore properties the scan
	// reads: identity (url), NFS backing, and the VMs with files on it.
	propSummary = "summary"
	propInfo    = "info"
	propVM      = "vm"
)

// diskScanCache shares and briefly reuses datastore scans, keyed by the
// case-folded datastore name. The zero value is ready to use.
type diskScanCache struct {
	mu      sync.Mutex
	entries map[string]*diskScanEntry
}

// diskScanEntry is one scan, in flight until done is closed.
type diskScanEntry struct {
	done  chan struct{}
	users *inventoryFileUsers
	err   error
	at    time.Time
}

// get returns the scan for key: a finished, successful one younger than
// diskScanTTL, or the one in flight, unless fresh; otherwise it runs scan
// (once, for every concurrent caller). A failed scan is never reused.
func (c *diskScanCache) get(ctx context.Context, key string, fresh bool,
	scan func(context.Context) (*inventoryFileUsers, error)) (*inventoryFileUsers, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*diskScanEntry)
	}
	if e, ok := c.entries[key]; ok && !fresh {
		select {
		case <-e.done:
			if e.err == nil && time.Since(e.at) < diskScanTTL {
				c.mu.Unlock()
				return e.users, nil
			}
		default:
			c.mu.Unlock()
			select {
			case <-e.done:
				return e.users, e.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	e := &diskScanEntry{done: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()

	e.users, e.err = scan(ctx)
	e.at = time.Now()
	close(e.done)
	return e.users, e.err
}

// invalidate drops key's scan, so the next get scans afresh.
func (c *diskScanCache) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// diskScanKey is the diskScanCache key of datastore dsName.
func diskScanKey(dsName string) string {
	return strings.ToLower(dsName)
}

// datastoreFileUsers returns who uses the files of datastore dsName
// (scanDatastoreFileUsers), through the provider's scan cache.
func (p *Provider) datastoreFileUsers(ctx context.Context, dsName string, fresh bool) (*inventoryFileUsers, error) {
	return p.diskScans.get(ctx, diskScanKey(dsName), fresh, func(ctx context.Context) (*inventoryFileUsers, error) {
		return p.scanDatastoreFileUsers(ctx, dsName)
	})
}

// nasKey identifies an NFS datastore's export ("host:/path"), or "".
func nasKey(info types.BaseDatastoreInfo) string {
	nas, ok := info.(*types.NasDatastoreInfo)
	if !ok || nas.Nas == nil || nas.Nas.RemoteHost == "" {
		return ""
	}
	return strings.ToLower(nas.Nas.RemoteHost) + ":" + path.Clean("/"+nas.Nas.RemotePath)
}

// scanDatastoreFileUsers reads the VMs and templates with files on datastore
// dsName or on any datastore that is the same storage (same summary.url, or
// the same NFS export), and records which datastore files each references. It
// fails — and callers fail closed — when the datastore is not found, a
// property cannot be read, or one of those VMs has no readable configuration
// (an inaccessible or orphaned VM, whose disks cannot be known).
func (p *Provider) scanDatastoreFileUsers(ctx context.Context, dsName string) (*inventoryFileUsers, error) {
	v, err := view.NewManager(p.client.Client).CreateContainerView(ctx, p.client.ServiceContent.RootFolder,
		[]string{datastoreMoType}, true)
	if err != nil {
		return nil, fmt.Errorf("create datastore view: %w", err)
	}
	defer func() { _ = v.Destroy(context.WithoutCancel(ctx)) }()
	var dss []mo.Datastore
	if err := v.Retrieve(ctx, []string{datastoreMoType}, []string{propName, propSummary, propInfo, propVM}, &dss); err != nil {
		return nil, fmt.Errorf("retrieve datastores: %w", err)
	}

	urls, nases := map[string]bool{}, map[string]bool{}
	found := false
	for i := range dss {
		if !strings.EqualFold(dss[i].Name, dsName) {
			continue
		}
		found = true
		if u := dss[i].Summary.Url; u != "" {
			urls[u] = true
		}
		if k := nasKey(dss[i].Info); k != "" {
			nases[k] = true
		}
	}
	if !found {
		return nil, fmt.Errorf("datastore %q not found", dsName)
	}
	seen := map[types.ManagedObjectReference]bool{}
	var refs []types.ManagedObjectReference
	for i := range dss {
		ds := &dss[i]
		same := strings.EqualFold(ds.Name, dsName) || (ds.Summary.Url != "" && urls[ds.Summary.Url]) ||
			(nasKey(ds.Info) != "" && nases[nasKey(ds.Info)])
		if !same {
			continue
		}
		for _, ref := range ds.Vm {
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
	}

	users := newInventoryFileUsers()
	if len(refs) == 0 {
		return users, nil
	}
	var vms []mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).Retrieve(ctx, refs,
		[]string{propName, propConfigHardwareDevice, propLayoutExFile}, &vms); err != nil {
		return nil, fmt.Errorf("retrieve devices of the VMs on datastore %q: %w", dsName, err)
	}
	for i := range vms {
		vm := &vms[i]
		if vm.Config == nil {
			return nil, fmt.Errorf("VM %s (%q) on datastore %q has no readable configuration", vm.Self.Value, vm.Name, dsName)
		}
		users.addVM(vm)
	}
	return users, nil
}
