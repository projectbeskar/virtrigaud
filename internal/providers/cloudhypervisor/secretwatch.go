//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/clustered/inventoryfile"
)

// Bounds of the inventory Secret watch.
const (
	// secretRetryMin is the first wait after a failed read or watch of the
	// inventory Secret; each further failure doubles it, up to the
	// inventory reload interval.
	secretRetryMin = 250 * time.Millisecond
	// secretStaleAfter is how many reload intervals without a successful
	// read of the inventory Secret make Validate report it as not
	// synchronised.
	secretStaleAfter = 3
)

// secretFollower is the state of the inventory Secret watch.
type secretFollower struct {
	secrets corev1client.SecretInterface
	name    string

	mu sync.Mutex
	// synced is when the Secret was last read or a watch event received.
	synced time.Time
	// lastErr is a coarse description of the last failure, never Secret
	// content.
	lastErr string
}

// NewFromSecret returns a Provider that fronts the hosts of the inventory
// Secret name, read through secrets (a client scoped to the Secret's
// namespace), and follows the Secret through the API: each change is applied
// as soon as the watch delivers it, so that a host removed from the inventory
// (a revoked or deleted Host) has its connection closed and is refused within
// the API server's watch latency, not after the kubelet's Secret volume sync.
//
// The Provider needs only get and watch on that one Secret: it reads it by
// name and watches it with a metadata.name field selector, which an RBAC rule
// scoped with resourceNames authorises. It never lists Secrets.
//
// The Secret is read again, by a quorum read, at the start of every watch
// cycle, and a cycle lasts at most Options.InventoryReloadInterval, so a
// watch that stops delivering events without failing delays a change by at
// most that interval. A Secret that cannot be read at start is an error. One
// that later cannot be read or parsed keeps the last good host set, and
// Validate reports it once no read succeeded for three reload intervals. A
// deleted Secret fronts no host.
//
// Secret content is never logged: a log line names the Secret and a host
// count or a coarse reason only.
func NewFromSecret(ctx context.Context, secrets corev1client.SecretInterface, name string, opts Options) (*Provider, error) {
	if secrets == nil || name == "" {
		return nil, errors.New("cloudhypervisor: the inventory Secret client and name are required")
	}
	s, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("cloudhypervisor: read the inventory Secret %s: %w", name, err)
	}
	inv, err := secretInventory(s, name)
	if err != nil {
		return nil, fmt.Errorf("cloudhypervisor: inventory Secret %s: %w", name, err)
	}
	p, err := New(opts, inv)
	if err != nil {
		return nil, err
	}
	f := &secretFollower{secrets: secrets, name: name}
	f.markSynced(p.opts.Now())
	p.secret = f
	wctx, cancel := context.WithCancel(context.Background())
	p.stopReload, p.reloadDone = cancel, make(chan struct{})
	go p.followSecret(wctx, s.ResourceVersion)
	return p, nil
}

// secretInventory decodes the inventory document of Secret s, which must be
// the Secret name. A Secret without the document is an error.
func secretInventory(s *corev1.Secret, name string) (hostsecret.Inventory, error) {
	if s == nil || s.Name != name {
		return hostsecret.Inventory{}, errors.New("not the inventory Secret")
	}
	data, ok := s.Data[hostsecret.SecretDataKey]
	if !ok {
		return hostsecret.Inventory{}, fmt.Errorf("no %s key", hostsecret.SecretDataKey)
	}
	return inventoryfile.DecodeInventory(data)
}

// markSynced records a successful read or event at now.
func (f *secretFollower) markSynced(now time.Time) {
	f.mu.Lock()
	f.synced, f.lastErr = now, ""
	f.mu.Unlock()
}

// markFailed records a failure.
func (f *secretFollower) markFailed(why string) {
	f.mu.Lock()
	f.lastErr = why
	f.mu.Unlock()
}

// staleness returns how long ago the Secret was last read and the last
// failure since.
func (f *secretFollower) staleness(now time.Time) (time.Duration, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return now.Sub(f.synced), f.lastErr
}

// followSecret runs watch cycles until ctx ends. Each cycle starts from a
// quorum read of the Secret, except the first, which starts from the read
// NewFromSecret made (resourceVersion rv).
func (p *Provider) followSecret(ctx context.Context, rv string) {
	defer close(p.reloadDone)
	f := p.secret
	wait := secretRetryMin
	for ctx.Err() == nil {
		if rv == "" {
			gctx, cancel := context.WithTimeout(ctx, p.opts.InventoryReloadInterval)
			s, err := f.secrets.Get(gctx, f.name, metav1.GetOptions{})
			cancel()
			switch {
			case err == nil:
				p.applySecret(s)
				rv = s.ResourceVersion
			case apierrors.IsNotFound(err):
				p.applySecretDeleted()
			default:
				p.secretFailed(ctx, "read", err, &wait)
				continue
			}
		}
		ok := p.watchSecret(ctx, rv)
		rv = ""
		if ok {
			wait = secretRetryMin
		} else if !p.pause(ctx, &wait) {
			return
		}
	}
}

// watchSecret runs one watch cycle from resourceVersion rv (empty: from now)
// and reports whether it ended normally (its timeout, or the API server
// closing it) rather than by a failure.
func (p *Provider) watchSecret(ctx context.Context, rv string) bool {
	f := p.secret
	timeout := int64(p.opts.InventoryReloadInterval / time.Second)
	if timeout < 1 {
		timeout = 1
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+5*time.Second)
	defer cancel()
	w, err := f.secrets.Watch(wctx, metav1.ListOptions{
		FieldSelector:       fields.OneTermEqualSelector("metadata.name", f.name).String(),
		ResourceVersion:     rv,
		TimeoutSeconds:      &timeout,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		if ctx.Err() == nil {
			p.logSecretFailure("watch", err)
		}
		return false
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case ev, open := <-w.ResultChan():
			if !open {
				return true
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				if s, ok := ev.Object.(*corev1.Secret); ok && s.Name == f.name {
					p.applySecret(s)
				}
			case watch.Deleted:
				if s, ok := ev.Object.(*corev1.Secret); ok && s.Name == f.name {
					p.applySecretDeleted()
				}
			case watch.Bookmark:
				f.markSynced(p.opts.Now())
			case watch.Error:
				st := apierrors.FromObject(ev.Object)
				if apierrors.IsGone(st) || apierrors.IsResourceExpired(st) {
					// The resourceVersion is too old: start again from a
					// quorum read, at once.
					return true
				}
				p.logSecretFailure("watch", st)
				return false
			}
		}
	}
}

// applySecret makes the fronted hosts those of Secret s. A Secret whose
// document cannot be decoded keeps the last good host set.
func (p *Provider) applySecret(s *corev1.Secret) {
	f := p.secret
	inv, err := secretInventory(s, f.name)
	if err != nil {
		f.markFailed("the inventory document is invalid")
		p.log.Warn("host inventory Secret: invalid document; keeping the last good host set",
			"secret", f.name, "error", err.Error())
		return
	}
	if err := p.Reconcile(inv); err != nil {
		f.markFailed("reconcile failed")
		p.log.Warn("host inventory Secret: reconcile failed", "secret", f.name, "error", err.Error())
		return
	}
	f.markSynced(p.opts.Now())
}

// applySecretDeleted fronts no host: the inventory Secret is gone.
func (p *Provider) applySecretDeleted() {
	f := p.secret
	if err := p.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}); err != nil {
		f.markFailed("reconcile failed")
		p.log.Warn("host inventory Secret: reconcile failed", "secret", f.name, "error", err.Error())
		return
	}
	f.markSynced(p.opts.Now())
	p.log.Warn("host inventory Secret: the Secret is deleted; no host is fronted", "secret", f.name)
}

// secretFailed records and logs a failed operation, then waits before the
// next attempt.
func (p *Provider) secretFailed(ctx context.Context, op string, err error, wait *time.Duration) {
	if ctx.Err() != nil {
		return
	}
	p.logSecretFailure(op, err)
	p.pause(ctx, wait)
}

// logSecretFailure records and logs a failed read or watch of the Secret.
// The API error names the Secret and a status, never its content.
func (p *Provider) logSecretFailure(op string, err error) {
	f := p.secret
	f.markFailed(op + " failed")
	p.log.Warn("host inventory Secret: "+op+" failed; keeping the last good host set", "secret", f.name, "error", err.Error())
}

// pause waits *wait, then doubles it up to the inventory reload interval.
// It reports false when ctx ended first.
func (p *Provider) pause(ctx context.Context, wait *time.Duration) bool {
	t := time.NewTimer(*wait)
	defer t.Stop()
	*wait *= 2
	if *wait > p.opts.InventoryReloadInterval {
		*wait = p.opts.InventoryReloadInterval
	}
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// secretStaleness returns a Validate note when the inventory Secret was not
// read for secretStaleAfter reload intervals; empty otherwise.
func (p *Provider) secretStaleness() string {
	if p.secret == nil {
		return ""
	}
	age, why := p.secret.staleness(p.opts.Now())
	if age < secretStaleAfter*p.opts.InventoryReloadInterval {
		return ""
	}
	msg := fmt.Sprintf("; the host inventory Secret %s was not read for %s, the last good host set is kept",
		p.secret.name, age.Truncate(time.Second))
	if why != "" {
		msg += " (" + why + ")"
	}
	return msg
}
