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

package controller

import (
	"sync"
	"time"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// imagePrepareBackoffPolicy is how long image preparation waits before it
// sends a prepare again after a failure of one kind: Base after the first
// failure, doubling with each consecutive one, never more than Max.
type imagePrepareBackoffPolicy struct {
	// Base is the wait after the first failure.
	Base time.Duration
	// Max bounds the wait.
	Max time.Duration
}

// delay returns the wait after failures consecutive failures (at least 1).
func (p imagePrepareBackoffPolicy) delay(failures int) time.Duration {
	d := p.Base
	for i := 1; i < failures && d < p.Max; i++ {
		d *= 2
	}
	if d > p.Max {
		return p.Max
	}
	return d
}

var (
	// importFailedBackoff paces the prepare sent again after an asynchronous
	// import ended in failure: every attempt is a download (multi-GB for an
	// OVA), and a source that always fails (a 404, a checksum mismatch) must
	// not be downloaded again on every task end.
	importFailedBackoff = imagePrepareBackoffPolicy{Base: time.Minute, Max: 30 * time.Minute}
	// notConfirmedBackoff paces the prepare sent again after an answer that
	// did not confirm the requested identity (ADR-0009 D7): a provider that
	// reports artifact identity but does not echo it must not be called on
	// every reconcile of every VM.
	notConfirmedBackoff = imagePrepareBackoffPolicy{Base: 30 * time.Second, Max: 5 * time.Minute}
)

// imagePrepareBackoffForget is how long after its wait ended a failure record
// is kept: a failure older than that no longer lengthens the next wait.
const imagePrepareBackoffForget = time.Hour

// Kinds of failure imagePrepareBackoff tracks; each has its own policy and
// its own count.
const (
	// backoffImportFailed: an asynchronous import ended in failure.
	backoffImportFailed = "import-failed"
	// backoffNotConfirmed: an answer did not confirm the requested identity.
	backoffNotConfirmed = "not-confirmed"
)

// imagePrepareBackoffKey identifies what a failure is about: one VMImage
// object, one Provider object and one spec.source (its digest). A changed
// source, a re-created VMImage or a re-created Provider starts afresh.
func imagePrepareBackoffKey(vmImage *infravirtrigaudiov1beta1.VMImage, provider *infravirtrigaudiov1beta1.Provider, digest string) string {
	return string(vmImage.UID) + "|" + imageProviderKey(provider) + "|" + string(provider.UID) + "|" + digest
}

// imagePrepareBackoffEntry is the failure record of one kind for one key.
type imagePrepareBackoffEntry struct {
	// failures is the number of consecutive failures.
	failures int
	// lastID identifies the last failure counted (a task ref), so reconciles
	// that observe the same failed task count it once. Empty for failures
	// that are one per call.
	lastID string
	// notBefore is when a prepare may be sent again.
	notBefore time.Time
	// msg says why the prepare waits; it is shown on the VM.
	msg string
}

// imagePrepareBackoff remembers, within this manager, the prepares that
// failed and when each may be sent again. The zero value is ready to use and
// it is safe for concurrent use. It is in memory: after a manager restart the
// first failure is counted again, and the entry's own record (the failed
// import's lastUpdated, see EnsureImageOnProvider) still enforces the first
// wait.
type imagePrepareBackoff struct {
	mu      sync.Mutex
	entries map[string]*imagePrepareBackoffEntry
}

// fail records a failure of kind for key at now — once per id when id is not
// empty — with msg, and returns the consecutive failures and when a prepare
// may be sent again. Records whose wait ended more than
// imagePrepareBackoffForget ago are dropped first.
func (b *imagePrepareBackoff) fail(kind, key, id, msg string, policy imagePrepareBackoffPolicy, now time.Time) (int, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]*imagePrepareBackoffEntry{}
	}
	for k, e := range b.entries {
		if now.Sub(e.notBefore) > imagePrepareBackoffForget {
			delete(b.entries, k)
		}
	}
	e, ok := b.entries[kind+"|"+key]
	if !ok {
		e = &imagePrepareBackoffEntry{}
		b.entries[kind+"|"+key] = e
	}
	if id == "" || id != e.lastID {
		e.failures++
		e.lastID = id
		e.notBefore = now.Add(policy.delay(e.failures))
	}
	e.msg = msg
	return e.failures, e.notBefore
}

// wait returns how long a prepare for key must still wait at now, and why,
// over every kind of failure; 0 when it may be sent.
func (b *imagePrepareBackoff) wait(key string, now time.Time) (time.Duration, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var longest time.Duration
	msg := ""
	for _, kind := range []string{backoffImportFailed, backoffNotConfirmed} {
		e, ok := b.entries[kind+"|"+key]
		if !ok {
			continue
		}
		if d := e.notBefore.Sub(now); d > longest {
			longest, msg = d, e.msg
		}
	}
	return longest, msg
}

// clear forgets the failures of the given kinds for key (all kinds when none
// is given).
func (b *imagePrepareBackoff) clear(key string, kinds ...string) {
	if len(kinds) == 0 {
		kinds = []string{backoffImportFailed, backoffNotConfirmed}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, kind := range kinds {
		delete(b.entries, kind+"|"+key)
	}
}
