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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Host conditions of signed host answers (development builds).
const (
	// HostConditionAgentAnswer is True while the host's last answer to the
	// heartbeat was a verified signed host answer, False otherwise.
	HostConditionAgentAnswer = "AgentAnswerVerified"
	// HostConditionAgentStoreRegressed is True while the host is held because
	// its operation store was replaced, rolled back or forked: nothing is
	// concluded from it and nothing new is signed for it until it is
	// re-enrolled.
	HostConditionAgentStoreRegressed = "AgentStoreRegressed"
)

// Reasons of the signed host answer conditions.
const (
	reasonHostAnswerVerified    = "Verified"
	reasonHostAnswerMissing     = "NoVerifiedAnswer"
	reasonHostAnswerNoTrust     = "NoTrust"
	reasonHostStoreRegressed    = "StoreRegressed"
	reasonHostStoreNotRegressed = "NotRegressed"
)

// getHostInfo is the heartbeat's host read. For a Provider whose type
// requires operation identity, with signed host answers configured, it is a
// GetHostInfo that asks the host for a signed host answer with its host
// inventory, which is verified and recorded (the VirtualMachine controller
// signs nothing for a host without a recent one); every other Provider is
// read as before. The inventory observation says what the heartbeat learnt
// of the host's inventory (applyHostInventory).
func (r *HostReconciler) getHostInfo(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	provider *infravirtrigaudiov1beta1.Provider, instance contracts.Provider) (contracts.HostInfo, inventoryObservation, error) {
	if !identityRequired(provider) {
		info, err := instance.GetHostInfo(ctx, host.Name)
		return info, inventoryObservation{}, err
	}
	obs := inventoryObservation{inScope: true}
	if r.HostAnswers == nil {
		obs.noAnswer = "per-host agent enrolment is not configured on this manager, so the host's answers cannot be verified"
		info, err := instance.GetHostInfo(ctx, host.Name)
		return info, obs, err
	}
	reader, ok := instance.(contracts.HostAnswerReader)
	if !ok {
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing,
			"the Provider's client cannot ask for signed host answers")
		obs.noAnswer = "the Provider's client cannot ask for signed host answers"
		info, err := instance.GetHostInfo(ctx, host.Name)
		return info, obs, err
	}
	refresh, err := r.HostAnswers.refreshHost(ctx, provider, reader, host.Name)
	switch {
	case err != nil && errors.Is(err, errHostTrust):
		log.FromContext(ctx).Info("No signed host answer can be verified for the host", "host", host.Name, "error", err.Error())
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerNoTrust,
			"the manager holds no enrolment, pins or CA for this host, so its answers cannot be verified; nothing new is signed for it")
		obs.noAnswer = "the manager holds no enrolment, pins or CA for this host, so its answers cannot be verified"
		info, gerr := instance.GetHostInfo(ctx, host.Name)
		return info, obs, gerr
	case err != nil:
		return refresh.info, obs, err
	}
	obs.refresh = &refresh
	v := refresh.answer
	switch refresh.verdict {
	case answerRegressed:
		setHostCondition(host, HostConditionAgentStoreRegressed, metav1.ConditionTrue, reasonHostStoreRegressed,
			"the host's operation store was replaced, rolled back or forked since the manager last verified it; "+
				"its VMs are held and nothing new is signed for it until it is re-enrolled")
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostStoreRegressed, "the host is held")
	case answerVerified:
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionTrue, reasonHostAnswerVerified,
			fmt.Sprintf("signed host answer verified (agent state %s, store position %d)", v.Context.GetAgentState(), v.Position.Seq))
		if c := meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentStoreRegressed); c != nil {
			setHostCondition(host, HostConditionAgentStoreRegressed, metav1.ConditionFalse, reasonHostStoreNotRegressed, "")
		}
		// A predecessor whose freeze was confirmed is removed once the host
		// shows, signed, that it discarded it.
		// A renewal advances on a verified answer of this manager's own
		// request: recorded, the new key pinned, the old key unpinned.
		for _, ev := range r.HostAnswers.checkRenewal(ctx, provider, host.Name, v) {
			if r.Recorder != nil {
				r.Recorder.Event(host, ev.typ, ev.reason, ev.message)
			}
		}
		for _, id := range r.HostAnswers.checkFreezeDiscards(ctx, provider, reader, host.Name) {
			if r.Recorder != nil {
				r.Recorder.Event(host, corev1.EventTypeNormal, eventReasonAgentPredecessorDiscarded,
					"the host discarded the records of predecessor enrolment "+id+" (signed answer); it is no longer listed")
			}
		}
	default:
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing,
			"the host's answer was missing or failed verification; nothing new is signed for it")
	}
	return refresh.info, obs, nil
}

// setHostCondition upserts a Host condition with ObservedGeneration.
func setHostCondition(host *infravirtrigaudiov1beta1.Host, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{Type: typ, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: host.Generation})
}
