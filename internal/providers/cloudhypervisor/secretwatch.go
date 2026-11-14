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
	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// Bounds of the inventory Secret watch.
const (
	// secretRetryMin is the first wait after a failed read or watch of the
	// inventory Secret; each further failure doubles it, up to the
	// inventory reload interval.
	secretRetryMin = 250 * time.Millisecond
	// secretStaleAfter is how many reload intervals without a successful
	// apply of the inventory make Validate and the metrics report it stale.
	secretStaleAfter = 3
	// DefaultInventoryFailClosedAfter is how long the Provider keeps
	// routing with its last good inventory when it can apply no newer one
	// (the API server unreachable, or an invalid document) before it
	// refuses every host.
	DefaultInventoryFailClosedAfter = 5 * time.Minute
	// failClosedCheckEvery bounds how often the fail-closed bound and the
	// metrics are evaluated.
	failClosedCheckEvery = time.Second
)

// secretFollower is the state of the inventory Secret watch.
type secretFollower struct {
	secrets corev1client.SecretInterface
	name    string

	// applyMu serialises applying an inventory and failing closed, so that
	// the two never interleave.
	applyMu sync.Mutex

	mu sync.Mutex
	// applied is when a valid inventory was last applied (or the Secret was
	// seen deleted). Only that refreshes it: a watch event that carries no
	// valid inventory, or a bookmark, proves the watch alive and nothing
	// more.
	applied time.Time
	// applyErr is why the last inventory seen could not be applied (an
	// invalid document, a failed reconcile); cleared only by a successful
	// apply.
	applyErr string
	// readErr is the last failed read or watch; cleared by the next
	// successful read or watch event.
	readErr string
	// failedClosed is set while every host is refused because no inventory
	// was applied within the fail-closed bound.
	failedClosed bool
}

// followerState is a snapshot of the follower.
type followerState struct {
	applied      time.Time
	applyErr     string
	readErr      string
	failedClosed bool
}

// snapshot returns the follower's state.
func (f *secretFollower) snapshot() followerState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return followerState{applied: f.applied, applyErr: f.applyErr, readErr: f.readErr, failedClosed: f.failedClosed}
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
	if opts.InventoryFailClosedAfter == 0 {
		opts.InventoryFailClosedAfter = DefaultInventoryFailClosedAfter
	}
	reload := opts.InventoryReloadInterval
	if reload == 0 {
		reload = DefaultInventoryReloadInterval
	}
	if opts.InventoryFailClosedAfter < 2*reload {
		return nil, fmt.Errorf("cloudhypervisor: the inventory fail-closed bound %s is shorter than two reload intervals (%s)",
			opts.InventoryFailClosedAfter, 2*reload)
	}
	p, err := New(opts, inv)
	if err != nil {
		return nil, err
	}
	f := &secretFollower{secrets: secrets, name: name, applied: p.opts.Now()}
	p.secret = f
	p.recordInventoryMetrics()
	wctx, cancel := context.WithCancel(context.Background())
	p.stopReload, p.reloadDone = cancel, make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.followSecret(wctx, s.ResourceVersion) }()
	go func() { defer wg.Done(); p.guardFailClosed(wctx) }()
	go func() { wg.Wait(); close(p.reloadDone) }() // ends once both above end with wctx
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

// markApplied records a successful apply at now: it clears every failure
// and, when the Provider had failed closed, routing resumes.
func (f *secretFollower) markApplied(now time.Time) {
	f.mu.Lock()
	f.applied, f.applyErr, f.readErr, f.failedClosed = now, "", "", false
	f.mu.Unlock()
}

// markApplyFailed records an inventory that could not be applied.
func (f *secretFollower) markApplyFailed(why string) {
	f.mu.Lock()
	f.applyErr = why
	f.mu.Unlock()
}

// markReadFailed records a failed read or watch.
func (f *secretFollower) markReadFailed(why string) {
	f.mu.Lock()
	f.readErr = why
	f.mu.Unlock()
}

// markAlive records a successful read or watch event that applied nothing
// (a bookmark): the API is reachable. It never clears applyErr and never
// refreshes applied.
func (f *secretFollower) markAlive() {
	f.mu.Lock()
	f.readErr = ""
	f.mu.Unlock()
}

// inventoryFailedClosed reports whether the Provider refuses every host
// because its inventory is older than the fail-closed bound.
func (p *Provider) inventoryFailedClosed() bool {
	return p.secret != nil && p.secret.snapshot().failedClosed
}

// guardFailClosed evaluates the fail-closed bound and the metrics every
// failClosedCheckEvery until ctx ends.
func (p *Provider) guardFailClosed(ctx context.Context) {
	t := time.NewTicker(failClosedCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.checkFailClosed()
	}
}

// checkFailClosed refuses every host once no inventory was applied for
// Options.InventoryFailClosedAfter: their connections are closed and every
// call is refused without dialing, until the next successful apply.
func (p *Provider) checkFailClosed() {
	f := p.secret
	f.applyMu.Lock()
	defer f.applyMu.Unlock()
	st := f.snapshot()
	if !st.failedClosed && p.opts.Now().Sub(st.applied) >= p.opts.InventoryFailClosedAfter {
		f.mu.Lock()
		f.failedClosed = true
		f.mu.Unlock()
		why := st.applyErr
		if why == "" {
			why = st.readErr
		}
		p.log.Error("host inventory Secret: no valid inventory applied within the fail-closed bound; every host is refused until one is",
			"secret", f.name, "bound", p.opts.InventoryFailClosedAfter.String(), "cause", why)
		if err := p.pool.reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}); err != nil && !errors.Is(err, errPoolClosed) {
			p.log.Warn("host inventory Secret: closing the hosts' connections failed", "secret", f.name, "error", err.Error())
		}
	}
	p.recordInventoryMetrics()
}

// recordInventoryMetrics records the inventory's age, staleness and
// fail-closed state.
func (p *Provider) recordInventoryMetrics() {
	st := p.secret.snapshot()
	age := p.opts.Now().Sub(st.applied)
	obsmetrics.SetProviderHostInventoryState(ProviderType, age.Seconds(), age >= secretStaleAfter*p.opts.InventoryReloadInterval, st.failedClosed)
}

// followSecret runs watch cycles until ctx ends. Each cycle starts from a
// quorum read of the Secret, except the first, which starts from the read
// NewFromSecret made (resourceVersion rv).
func (p *Provider) followSecret(ctx context.Context, rv string) {
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
	// The API server ends the watch after timeout; the context ends it a
	// second later should the server or the connection go silent.
	timeout := int64(p.opts.InventoryReloadInterval / time.Second)
	if timeout < 1 {
		timeout = 1
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+time.Second)
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
		case <-wctx.Done():
			// Shutdown, or the cycle's end without the server ending it.
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
				f.markAlive()
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
// document cannot be decoded keeps the last good host set (until the
// fail-closed bound) and is reported until a valid one is applied.
func (p *Provider) applySecret(s *corev1.Secret) {
	f := p.secret
	f.markAlive()
	inv, err := secretInventory(s, f.name)
	if err != nil {
		f.markApplyFailed("the inventory document is invalid")
		p.log.Warn("host inventory Secret: invalid document; keeping the last good host set",
			"secret", f.name, "error", err.Error())
		return
	}
	p.apply(inv)
}

// applySecretDeleted fronts no host: the inventory Secret is gone.
func (p *Provider) applySecretDeleted() {
	p.secret.markAlive()
	if p.apply(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}) {
		p.log.Warn("host inventory Secret: the Secret is deleted; no host is fronted", "secret", p.secret.name)
	}
}

// apply reconciles inv and records the outcome; it reports success.
func (p *Provider) apply(inv hostsecret.Inventory) bool {
	f := p.secret
	f.applyMu.Lock()
	defer f.applyMu.Unlock()
	wasClosed := f.snapshot().failedClosed
	if err := p.Reconcile(inv); err != nil {
		f.markApplyFailed("reconcile failed")
		p.log.Warn("host inventory Secret: reconcile failed", "secret", f.name, "error", err.Error())
		return false
	}
	f.markApplied(p.opts.Now())
	if wasClosed {
		p.log.Info("host inventory Secret: a valid inventory is applied again; hosts are routed", "secret", f.name, "hosts", len(inv.Hosts))
	}
	p.recordInventoryMetrics()
	return true
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
	f.markReadFailed(op + " failed")
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

// secretStaleness returns a Validate note when no inventory was applied
// for secretStaleAfter reload intervals, and says when every host is
// refused; empty otherwise.
func (p *Provider) secretStaleness() string {
	if p.secret == nil {
		return ""
	}
	st := p.secret.snapshot()
	age := p.opts.Now().Sub(st.applied)
	if !st.failedClosed && age < secretStaleAfter*p.opts.InventoryReloadInterval {
		return ""
	}
	msg := fmt.Sprintf("; no valid host inventory was applied from the Secret %s for %s", p.secret.name, age.Truncate(time.Second))
	for _, why := range []string{st.applyErr, st.readErr} {
		if why != "" {
			msg += " (" + why + ")"
		}
	}
	if st.failedClosed {
		return msg + fmt.Sprintf(", longer than the fail-closed bound %s: every host is refused until a valid inventory is applied",
			p.opts.InventoryFailClosedAfter)
	}
	return msg + ", the last good host set is kept"
}
