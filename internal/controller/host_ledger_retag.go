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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// defaultRetagInterval is the least time between two writes of the re-tag
// sweep.
const defaultRetagInterval = 100 * time.Millisecond

// HostLedgerRetagResult is the outcome of a re-tag sweep.
type HostLedgerRetagResult struct {
	// Ledgers and Indexes are the ledger objects and indexes re-tagged.
	Ledgers, Indexes int
	// Remaining are the ledger objects and indexes still under another key
	// than the current one: those that fail their tag under every key the
	// manager holds, or whose write failed. 0 means the previous key may be
	// removed.
	Remaining int
}

// RetagSweep re-tags under the current tag key every ledger object in the
// enrolment namespace, archived ones included, and every ledger index in
// the pins Secrets there, that is tagged under the previous key. Each is
// verified under the key it names first, a ledger object is checked as a
// read of it would be (retagChecked: a restored older copy is refused, and
// holds its Host), and each is written with the resourceVersion it was read
// with (a conflict is read again). Writes are
// paced by RetagInterval. Objects that verify under no key are counted as
// remaining and left as they are. The result is logged and published on
// virtrigaud_manager_host_ledger_retag_remaining.
func (l *HostLedger) RetagSweep(ctx context.Context) (HostLedgerRetagResult, error) {
	var res HostLedgerRetagResult
	interval := l.RetagInterval
	if interval <= 0 {
		interval = defaultRetagInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	pace := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			return nil
		}
	}
	var ledgers corev1.SecretList
	if err := l.APIReader.List(ctx, &ledgers, client.InNamespace(l.Config.Namespace),
		client.MatchingLabels{k8sLabelComponent: hostLedgerComponent}); err != nil {
		return res, fmt.Errorf("list host ledgers: %w", err)
	}
	for i := range ledgers.Items {
		done, err := l.retagLedger(ctx, ledgers.Items[i].Name, pace)
		switch {
		case err != nil:
			log.FromContext(ctx).Info("A host ledger could not be re-tagged", "secret", ledgers.Items[i].Name, "error", err.Error())
			res.Remaining++
		case done:
			res.Ledgers++
		}
	}
	var pins corev1.SecretList
	if err := l.APIReader.List(ctx, &pins, client.InNamespace(l.Config.Namespace),
		client.MatchingLabels{k8sLabelComponent: hostEnrolmentComponent}); err != nil {
		return res, fmt.Errorf("list pins Secrets: %w", err)
	}
	for i := range pins.Items {
		done, err := l.retagIndex(ctx, pins.Items[i].Name, pace)
		switch {
		case err != nil:
			log.FromContext(ctx).Info("A host ledger index could not be re-tagged", "secret", pins.Items[i].Name, "error", err.Error())
			res.Remaining++
		case done:
			res.Indexes++
		}
	}
	metrics.SetHostLedgerRetagRemaining(res.Remaining)
	msg := "Host ledger re-tag sweep finished; the previous host ledger key may be removed"
	if res.Remaining > 0 {
		msg = "Host ledger re-tag sweep finished with objects left under another key; keep the previous host ledger key"
	}
	log.FromContext(ctx).Info(msg, "currentKeyID", l.Keys.CurrentKeyID(), "ledgers", res.Ledgers, "indexes", res.Indexes,
		"remaining", res.Remaining)
	return res, nil
}

// retagLedger re-tags one ledger object if it is under the previous key,
// after the checks of retagChecked.
func (l *HostLedger) retagLedger(ctx context.Context, name string, pace func() error) (bool, error) {
	for range maxLedgerWriteAttempts {
		sec := &corev1.Secret{}
		if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		rec, err := l.verifySecret(ctx, sec, [16]byte{})
		if err != nil {
			return false, err
		}
		if rec.TagKeyID == l.Keys.CurrentKeyID() {
			return false, nil
		}
		if err := pace(); err != nil {
			return false, err
		}
		done, err := l.retagChecked(ctx, rec)
		if apierrors.IsConflict(err) {
			continue
		}
		return done, err
	}
	return false, errHostLedgerConflict
}

// retagChecked re-tags one ledger object after the checks a read of it gets.
// When the Host it serves resolves, the object is read again through
// readRecord for that Host (its index, write counter, stored hold and guard;
// a regression holds the Host, persistently) and the new counter is recorded
// in the Host's index. Otherwise the object is compared with the index in
// the pins Secret of the Host it names, when that Secret is still the
// Host's; a counter below the index is refused, and the re-tagged object
// does not seed this process's guard.
func (l *HostLedger) retagChecked(ctx context.Context, rec *HostLedgerRecord) (bool, error) {
	m := l.storeLock(rec.StoreID)
	m.Lock()
	defer m.Unlock()
	if lh := l.retagHost(ctx, rec); lh != nil {
		st, err := l.storeState(ctx, lh, rec.StoreID)
		if err != nil {
			return false, err
		}
		obj, err := l.readRecord(ctx, lh, rec.StoreID, st.LedgerIndex)
		if err != nil {
			return false, err
		}
		if obj.rec.TagKeyID == l.Keys.CurrentKeyID() {
			return false, nil
		}
		got, err := l.writeObject(ctx, lh, obj.sec, obj.rec.clone(), nil, false)
		if err != nil {
			return false, err
		}
		if got.HostUID == lh.uid() && st.LedgerIndex.Reserves(got.StoreID) {
			if err := l.persistIndex(ctx, lh, indexChange{stores: map[[16]byte]uint64{got.StoreID: got.WriteCounter}}); err != nil {
				return true, fmt.Errorf("record store %s in the host's ledger index: %w", got.StorePrefix(), err)
			}
		}
		return true, nil
	}
	// The Host does not resolve (deleted, revoked, without an enrolment):
	// compare with its index if its pins Secret is still its own.
	sec := &corev1.Secret{}
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: hostLedgerSecretName(rec.StoreID)}, sec); err != nil {
		return false, err
	}
	fresh, err := l.verifySecret(ctx, sec, rec.StoreID)
	if err != nil {
		return false, err
	}
	if fresh.TagKeyID == l.Keys.CurrentKeyID() {
		return false, nil
	}
	if idx, err := l.pinsIndexOf(ctx, fresh); err != nil {
		return false, err
	} else if e, ok := idx.entry(fresh.StoreID); ok && fresh.WriteCounter < e.Counter {
		return false, fmt.Errorf("store %s: the ledger's write counter is below its Host's index: an older copy was restored; "+
			"it is not re-tagged: %w", fresh.StorePrefix(), errLedgerMalformed)
	}
	next := fresh.clone()
	data, err := l.prepare(next)
	if err != nil {
		return false, err
	}
	out := sec.DeepCopy()
	out.Data[hostLedgerDataKey] = data
	setLedgerLabels(out, next, nil)
	if err := l.Client.Update(ctx, out); err != nil {
		return false, err
	}
	if _, err := l.verifySecret(ctx, out, rec.StoreID); err != nil {
		return false, fmt.Errorf("verify the host ledger of store %s as written: %w", rec.StorePrefix(), err)
	}
	return true, nil
}

// retagHost resolves the Host a ledger serves for the sweep: the Host named
// by its HostKey, of the same UID, resolvable as a ledger Host (its
// Provider's, not revoked, with an identity and a current enrolment). It
// returns nil otherwise.
func (l *HostLedger) retagHost(ctx context.Context, rec *HostLedgerRecord) *ledgerHost {
	ns, name, ok := strings.Cut(rec.HostKey, "/")
	if !ok {
		return nil
	}
	h := &infravirtrigaudiov1beta1.Host{}
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, h); err != nil || string(h.UID) != rec.HostUID {
		return nil
	}
	p := &infravirtrigaudiov1beta1.Provider{}
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: h.Spec.ProviderRef.Name}, p); err != nil {
		return nil
	}
	lh, err := l.resolve(ctx, p, name)
	if err != nil || lh.uid() != rec.HostUID {
		return nil
	}
	return lh
}

// pinsIndexOf returns the verified ledger index in the pins Secret of the
// Host a ledger names, when that Secret is still that Host's (nil when there
// is none). An index that fails its tag is an error.
func (l *HostLedger) pinsIndexOf(ctx context.Context, rec *HostLedgerRecord) (*HostLedgerIndex, error) {
	ns, name, ok := strings.Cut(rec.HostKey, "/")
	if !ok {
		return nil, nil
	}
	sec := &corev1.Secret{}
	if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: hostEnrolmentSecretName(ns, name)}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var enrol hostEnrolmentState
	if err := json.Unmarshal(sec.Data[hostEnrolmentStateKey], &enrol); err != nil || enrol.HostUID != rec.HostUID {
		return nil, nil //nolint:nilerr // another Host's (or no) state: no index of this Host to compare with
	}
	st, err := decodeStoreState(sec.Data[hostStoreStateKey])
	if err != nil {
		return nil, err
	}
	if len(st.ledgerIndexRaw) > 0 || (st.LedgerIndex != nil && !st.LedgerIndex.verifyTag(l.Keys, rec.HostUID)) {
		return nil, fmt.Errorf("the ledger index of Host %s fails its tag or form: %w", rec.HostKey, errLedgerMalformed)
	}
	return st.LedgerIndex, nil
}

// retagIndex re-tags the ledger index of one pins Secret if it is under the
// previous key, bound to the Host UID the Secret's enrolment state names.
func (l *HostLedger) retagIndex(ctx context.Context, name string, pace func() error) (bool, error) {
	for range maxLedgerWriteAttempts {
		sec := &corev1.Secret{}
		if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		st, err := decodeStoreState(sec.Data[hostStoreStateKey])
		if err != nil {
			return false, err
		}
		x := st.LedgerIndex
		if x == nil || x.TagKeyID == l.Keys.CurrentKeyID() {
			return false, nil
		}
		var enrol hostEnrolmentState
		if err := json.Unmarshal(sec.Data[hostEnrolmentStateKey], &enrol); err != nil || enrol.HostUID == "" {
			return false, fmt.Errorf("the pins Secret names no Host UID: %w", errLedgerMalformed)
		}
		if !x.verifyTag(l.Keys, enrol.HostUID) {
			return false, fmt.Errorf("the ledger index fails its tag: %w", errLedgerMalformed)
		}
		if err := pace(); err != nil {
			return false, err
		}
		x.sign(l.Keys, enrol.HostUID)
		data, err := encodeStoreState(st)
		if err != nil {
			return false, err
		}
		out := sec.DeepCopy()
		out.Data[hostStoreStateKey] = data
		err = l.Client.Update(ctx, out)
		if apierrors.IsConflict(err) {
			continue
		}
		return err == nil, err
	}
	return false, errHostLedgerConflict
}

// HostLedgerRetagRunnable runs RetagSweep once when the manager becomes
// leader, when a previous tag key is configured (nil otherwise). A failed
// sweep is logged; it does not stop the manager.
func HostLedgerRetagRunnable(l *HostLedger) manager.Runnable {
	if l == nil || l.Keys == nil || l.Keys.PreviousKeyID() == "" {
		return nil
	}
	return manager.RunnableFunc(func(ctx context.Context) error {
		if _, err := l.RetagSweep(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "Host ledger re-tag sweep failed; keep the previous host ledger key")
		}
		return nil
	})
}
