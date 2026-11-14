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
// GetHostInfo that asks the host for a signed host answer, which is verified
// and recorded (the VirtualMachine controller signs nothing for a host
// without a recent one); every other Provider is read as before.
func (r *HostReconciler) getHostInfo(ctx context.Context, host *infravirtrigaudiov1beta1.Host,
	provider *infravirtrigaudiov1beta1.Provider, instance contracts.Provider) (contracts.HostInfo, error) {
	if !identityRequired(provider) || r.HostAnswers == nil {
		return instance.GetHostInfo(ctx, host.Name)
	}
	reader, ok := instance.(contracts.HostAnswerReader)
	if !ok {
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing,
			"the Provider's client cannot ask for signed host answers")
		return instance.GetHostInfo(ctx, host.Name)
	}
	info, v, verdict, err := r.HostAnswers.RefreshHostAnswer(ctx, provider, reader, host.Name)
	switch {
	case err != nil && errors.Is(err, errHostTrust):
		log.FromContext(ctx).Info("No signed host answer can be verified for the host", "host", host.Name, "error", err.Error())
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerNoTrust,
			"the manager holds no enrolment, pins or CA for this host, so its answers cannot be verified; nothing new is signed for it")
		return instance.GetHostInfo(ctx, host.Name)
	case err != nil:
		return info, err
	}
	switch verdict {
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
	default:
		setHostCondition(host, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing,
			"the host's answer was missing or failed verification; nothing new is signed for it")
	}
	return info, nil
}

// setHostCondition upserts a Host condition with ObservedGeneration.
func setHostCondition(host *infravirtrigaudiov1beta1.Host, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{Type: typ, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: host.Generation})
}
