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

// The upgrade pre-flight of the host ledger: before a manager is replaced,
// every host's last verified store position this manager holds in memory
// and has not yet persisted is written to the host's pins Secret. The next
// manager starts a host's ledger from the persisted position (once the host
// confirms it), so a fresher position narrows the window in which the
// agent's first attestation must go through a checkpoint because the
// position lagged its chain. The manager runs it when it stops (a rolling
// upgrade stops the old manager first), bounded in time.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// preflightTimeout bounds the pre-flight a stopping manager runs.
const preflightTimeout = 10 * time.Second

// PreflightRaisePositions persists, for every host this process verified an
// answer of, its in-memory store position when it is above the persisted
// one (persistPosition, which never lowers a position). It returns how many
// hosts it raised and the joined errors of those it could not.
func (h *HostAnswers) PreflightRaisePositions(ctx context.Context) (int, error) {
	type target struct {
		provider *infravirtrigaudiov1beta1.Provider
		hostID   string
	}
	var targets []target
	h.mu.Lock()
	for k, s := range h.hosts {
		if s == nil || s.provider == nil || !s.verified || !s.loaded || s.store.Current.Empty() || s.store.Current.Seq <= s.persistedSeq {
			continue
		}
		if _, host, ok := strings.Cut(k, "/"); ok {
			targets = append(targets, target{s.provider, host})
		}
	}
	h.mu.Unlock()
	raised := 0
	var errs []error
	for _, t := range targets {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := h.persistPosition(ctx, t.provider, t.hostID); err != nil {
			errs = append(errs, fmt.Errorf("host %s: %w", t.hostID, err))
			continue
		}
		raised++
	}
	return raised, errors.Join(errs...)
}

// PreflightRunnable returns the runnable that runs PreflightRaisePositions
// when the manager stops (nil for a manager without a host ledger), but
// only on a replica that held the leader lease at some point: elected is
// the manager's election channel (closed once this replica leads, or at
// once when leader election is off). A replica that never led wrote no
// position and writes none on stop.
func PreflightRunnable(h *HostAnswers, elected <-chan struct{}) manager.Runnable {
	if h == nil || h.Ledger == nil {
		return nil
	}
	return preflightRunnable{h: h, elected: elected}
}

// preflightRunnable runs the pre-flight on stop.
type preflightRunnable struct {
	h       *HostAnswers
	elected <-chan struct{}
}

// led reports whether this replica held the leader lease at some point.
func (p preflightRunnable) led() bool {
	if p.elected == nil {
		return false
	}
	select {
	case <-p.elected:
		return true
	default:
		return false
	}
}

// Start waits for the manager to stop, then runs the pre-flight if this
// replica led.
func (p preflightRunnable) Start(ctx context.Context) error {
	<-ctx.Done()
	if !p.led() {
		log.FromContext(ctx).Info("The upgrade pre-flight is skipped: this replica never held the leader lease")
		return nil
	}
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), preflightTimeout)
	defer cancel()
	n, err := p.h.PreflightRaisePositions(stop)
	logger := log.FromContext(ctx)
	if err != nil {
		logger.Error(err, "The upgrade pre-flight could not raise every host's persisted store position", "raised", n)
		return nil
	}
	logger.Info("The upgrade pre-flight raised the hosts' persisted store positions", "raised", n)
	return nil
}

// NeedLeaderElection reports false: the runnable starts on every replica
// and writes only on one that led (a stopping leader may already have lost
// its lease when it runs; a position is only ever raised, from answers this
// replica verified, so the write is safe).
func (preflightRunnable) NeedLeaderElection() bool { return false }
