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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostinventory"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Host inventory and readiness (development builds). For a host of a
// Provider whose type runs a per-host agent, every heartbeat asks the host
// for its inventory inside the signed host answer. The manager verifies it,
// mirrors it into Host.status.agentInventory (an output only), sets the
// AgentInventoryVerified and ComponentsReady conditions from the verified
// answer of this heartbeat alone (never from status, never carried over a
// failed heartbeat), and lets the host be Ready only when the Provider says
// it is ready and both conditions are True.

// reasonAgentInventoryNotReady is Ready=False on a host the Provider reports
// ready whose inventory conditions are not both True.
const reasonAgentInventoryNotReady = "AgentInventoryNotReady"

// Inventory backoff after the host's agent signed an inventory that breaks a
// field rule: the manager stops asking for the inventory for
// inventoryBackoffMin, doubling up to inventoryBackoffMax, so the rest of the
// host answer keeps working.
const (
	inventoryBackoffMin = time.Minute
	inventoryBackoffMax = 5 * time.Minute
)

// inventoryEventInterval is the least time between two inventory Events of
// one host.
const inventoryEventInterval = time.Minute

// inventoryBackoff is the inventory backoff of one host.
type inventoryBackoff struct {
	// host is the Host (namespace and name), to forget it with the Host.
	host     types.NamespacedName
	failures int
	until    time.Time
	// enrolment and agentVersion are the host's when the backoff started or
	// was last confirmed: a new enrolment or agent version ends it.
	enrolment    []byte
	agentVersion string
}

// inventoryBackoffDelay is the backoff after failures consecutive invalid
// inventories: 1, 2, 4, then 5 minutes.
func inventoryBackoffDelay(failures int) time.Duration {
	d := inventoryBackoffMin
	for i := 1; i < failures && d < inventoryBackoffMax; i++ {
		d *= 2
	}
	return min(d, inventoryBackoffMax)
}

// inventoryAsk returns the inventory version to ask the host for: the highest
// the manager implements, or 0 during the host's inventory backoff.
func (h *HostAnswers) inventoryAsk(provider *infravirtrigaudiov1beta1.Provider, hostID string) uint32 {
	h.imu.Lock()
	defer h.imu.Unlock()
	if b := h.invBackoff[hostAnswerKey(provider, hostID)]; b != nil && h.now().Before(b.until) {
		return 0
	}
	return hostidentity.InventoryVersion
}

// inventoryFault records that the host's agent signed an inventory that
// breaks a field rule: the backoff starts or doubles.
func (h *HostAnswers) inventoryFault(provider *infravirtrigaudiov1beta1.Provider, hostID string) {
	h.imu.Lock()
	defer h.imu.Unlock()
	if h.invBackoff == nil {
		h.invBackoff = map[string]*inventoryBackoff{}
	}
	k := hostAnswerKey(provider, hostID)
	b := h.invBackoff[k]
	if b == nil {
		b = &inventoryBackoff{host: types.NamespacedName{Namespace: provider.Namespace, Name: hostID}}
		h.invBackoff[k] = b
	}
	b.failures++
	b.until = h.now().Add(inventoryBackoffDelay(b.failures))
}

// inventoryAnswered updates the host's backoff with a verified answer to a
// read that asked for asked: an inventory received ends it; during the
// backoff, an answer from a new enrolment or agent version ends it (the next
// read asks again), any other answer records the enrolment and version.
func (h *HostAnswers) inventoryAnswered(provider *infravirtrigaudiov1beta1.Provider, hostID string, v hostidentity.VerifiedAnswer, asked uint32) {
	h.imu.Lock()
	defer h.imu.Unlock()
	k := hostAnswerKey(provider, hostID)
	b := h.invBackoff[k]
	if b == nil {
		return
	}
	enrol, version := v.Context.GetEnrolmentId(), v.Host.GetAgentVersion()
	switch {
	case asked > 0 && v.Host.GetInventory() != nil:
		delete(h.invBackoff, k)
	case b.enrolment != nil && (!bytes.Equal(b.enrolment, enrol) || b.agentVersion != version):
		delete(h.invBackoff, k)
	default:
		b.enrolment, b.agentVersion = bytes.Clone(enrol), version
	}
}

// forgetInventoryBackoff drops the inventory backoff of the Host key (a Host
// lives in its Provider's namespace).
func (h *HostAnswers) forgetInventoryBackoff(key types.NamespacedName) {
	h.imu.Lock()
	defer h.imu.Unlock()
	for k, b := range h.invBackoff {
		if b.host == key {
			delete(h.invBackoff, k)
		}
	}
}

// inventoryPolicy returns the policy (the default without one).
func (h *HostAnswers) inventoryPolicy() *HostInventoryPolicy {
	if h == nil || h.Inventory == nil {
		return DefaultHostInventoryPolicy()
	}
	return h.Inventory
}

// inventoryObservation is what one heartbeat of a host of a Provider whose
// type runs a per-host agent observed for its inventory.
type inventoryObservation struct {
	// inScope reports a host of such a Provider; nothing else is set
	// otherwise.
	inScope bool
	// refresh is the signed read; nil when none was made.
	refresh *hostRefresh
	// noAnswer says why there is no verified answer when refresh is nil.
	noAnswer string
}

// conditionVerdict is one condition's computed value.
type conditionVerdict struct {
	status  metav1.ConditionStatus
	reason  string
	message string
}

// inventoryVerdict is what one heartbeat decides about a host's inventory.
type inventoryVerdict struct {
	verified   conditionVerdict
	components conditionVerdict
	// component names the component the ComponentsReady reason is about.
	component string
	// mirror is the new Host.status.agentInventory; nil keeps the last one.
	mirror *infravirtrigaudiov1beta1.HostAgentInventoryStatus
}

// ready reports whether both conditions are True.
func (v inventoryVerdict) ready() bool {
	return v.verified.status == metav1.ConditionTrue && v.components.status == metav1.ConditionTrue
}

// evaluateHostInventory decides the inventory conditions of one heartbeat
// from its observation alone.
func evaluateHostInventory(p *HostInventoryPolicy, obs inventoryObservation, now time.Time) inventoryVerdict {
	out := inventoryVerdict{}
	out.verified = verifiedCondition(p, obs, now)
	r := obs.refresh
	var inv *providerv1.HostInventory
	if r != nil && r.verdict == answerVerified {
		inv = r.answer.Host.GetInventory()
	}
	var results map[string]hostinventory.Result
	if inv != nil {
		results = allowListResults(p, inv)
		out.mirror = inventoryMirror(p, r, inv, results)
	}
	out.components, out.component = componentsCondition(p, out.verified, r, inv, results)
	return out
}

// verifiedCondition is the AgentInventoryVerified condition.
func verifiedCondition(p *HostInventoryPolicy, obs inventoryObservation, now time.Time) conditionVerdict {
	no := func(reason, msg string) conditionVerdict {
		return conditionVerdict{status: metav1.ConditionFalse, reason: reason, message: msg}
	}
	r := obs.refresh
	switch {
	case r == nil:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer, obs.noAnswer)
	case r.verdict == answerRegressed:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer,
			"the host is held for an operation store regression; its answers are not used")
	case r.verdict != answerVerified && r.inventoryBackoff:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryInvalid,
			"the host's agent signed an inventory that breaks a field rule; the inventory is asked for again after a backoff")
	case r.verdict != answerVerified:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer,
			"the heartbeat got no verified signed host answer (none, unsigned, failed verification, or an inventory "+
				"that does not echo the request)")
	}
	inv := r.answer.Host.GetInventory()
	switch {
	case inv == nil && r.inventoryBackoff:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryInvalid,
			"the host's agent recently signed an inventory that breaks a field rule; the inventory is not asked for until "+
				"the backoff ends or the agent's enrolment or version changes")
	case inv == nil:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryMissing,
			"the verified host answer carries no inventory (the agent does not report one)")
	case inv.GetState() == providerv1.InventoryState_INVENTORY_STATE_PENDING:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryPending, "the host's agent has not finished its first collection")
	case inv.GetState() == providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryOverLimit,
			fmt.Sprintf("the host's agent observed more than an inventory can hold (%s)", inv.GetStateReason()))
	case inv.GetState() == providerv1.InventoryState_INVENTORY_STATE_UNREADABLE:
		return no(infravirtrigaudiov1beta1.HostReasonInventoryUnreadable,
			fmt.Sprintf("the host's agent could not complete its collection (%s)", inv.GetStateReason()))
	}
	age := time.Duration(inv.GetSnapshotAgeSeconds())*time.Second + now.Sub(r.verifiedAt)
	if age > p.maxAge() {
		return no(infravirtrigaudiov1beta1.HostReasonInventoryStale,
			fmt.Sprintf("the inventory is %s old, older than %s", age.Truncate(time.Second), p.maxAge()))
	}
	return conditionVerdict{status: metav1.ConditionTrue, reason: infravirtrigaudiov1beta1.HostReasonInventoryVerified,
		message: fmt.Sprintf("signed host inventory verified (version %d, collected %ds before the answer)",
			inv.GetInventoryVersion(), inv.GetSnapshotAgeSeconds())}
}

// allowListResults compares every component with the allow-list.
func allowListResults(p *HostInventoryPolicy, inv *providerv1.HostInventory) map[string]hostinventory.Result {
	out := make(map[string]hostinventory.Result, len(inv.GetComponents()))
	for _, c := range inv.GetComponents() {
		if p.AllowList == nil {
			out[c.GetName()] = hostinventory.NotChecked
			continue
		}
		out[c.GetName()] = p.AllowList.Match(c)
	}
	return out
}

// allowListNote names the allow-list in force for a ComponentsReady message.
func allowListNote(p *HostInventoryPolicy) string {
	switch {
	case p.AllowList != nil:
		return "allow-list sha256 " + p.AllowList.SHA256()
	case p.AllowListErr != nil:
		return "the configured allow-list is not in force"
	default:
		return "no allow-list"
	}
}

// componentsCondition is the ComponentsReady condition and the component its
// reason is about: the first check that fails, in the documented order.
//
//nolint:gocyclo // one check per reason, in the documented order
func componentsCondition(p *HostInventoryPolicy, verified conditionVerdict, r *hostRefresh, inv *providerv1.HostInventory,
	results map[string]hostinventory.Result) (conditionVerdict, string) {
	note := allowListNote(p)
	no := func(reason, component, msg string) (conditionVerdict, string) {
		return conditionVerdict{status: metav1.ConditionFalse, reason: reason, message: msg + "; " + note}, component
	}
	if verified.status != metav1.ConditionTrue {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsInventoryNotVerified, "",
			"the host inventory is not verified ("+verified.reason+")")
	}
	if !r.answer.Ready() {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsAgentNotReady, "",
			fmt.Sprintf("the host's agent is %s", r.answer.Context.GetAgentState()))
	}
	if len(inv.GetComponents()) == 0 {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsNoneReported, "",
			"the host inventory lists no component, so nothing on the host can be checked")
	}
	if e := inv.GetUnexpectedExtensions(); len(e) > 0 {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsUnexpectedExtension, "",
			fmt.Sprintf("system extension %q is merged on the host and no component declares it", e[0].GetName()))
	}
	firstIn := func(states ...providerv1.ComponentState) *providerv1.HostComponent {
		for _, c := range inv.GetComponents() {
			if slices.Contains(states, c.GetState()) {
				return c
			}
		}
		return nil
	}
	if c := firstIn(providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED); c != nil {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsUnmanifested, c.GetName(),
			fmt.Sprintf("component %q is present but no manifest declares it", c.GetName()))
	}
	if c := firstIn(providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID); c != nil {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsManifestInvalid, c.GetName(),
			fmt.Sprintf("the manifest of component %q cannot be used (%s)", c.GetName(), c.GetStateReason()))
	}
	for _, c := range inv.GetComponents() {
		o := c.GetManifestOrigin()
		if o != providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE && !slices.Contains(p.AcceptedOrigins, o) {
			return no(infravirtrigaudiov1beta1.HostReasonComponentsManifestOriginNotAccepted, c.GetName(),
				fmt.Sprintf("component %q is declared by a manifest from the %s directory, which is not accepted", c.GetName(), originName(o)))
		}
	}
	for _, check := range []struct {
		state  providerv1.ComponentState
		reason string
		what   string
	}{
		{providerv1.ComponentState_COMPONENT_STATE_MISSING, infravirtrigaudiov1beta1.HostReasonComponentsMissing, "is missing"},
		{providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE, infravirtrigaudiov1beta1.HostReasonComponentsIncompatible, "is incompatible"},
		{providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, infravirtrigaudiov1beta1.HostReasonComponentsUnhealthy, "is unhealthy"},
	} {
		if c := firstIn(check.state); c != nil {
			return no(check.reason, c.GetName(), fmt.Sprintf("component %q %s (%s)", c.GetName(), check.what, c.GetStateReason()))
		}
	}
	switch {
	case p.AllowListErr != nil:
		return no(infravirtrigaudiov1beta1.HostReasonComponentsAllowListUnverified, "",
			"the manager's component allow-list could not be loaded with its pinned digest and signature")
	case p.AllowList == nil && !p.SkipAllowList:
		return no(infravirtrigaudiov1beta1.HostReasonComponentsAllowListNotConfigured, "",
			"the manager has no component allow-list, so host components cannot be checked")
	}
	if p.AllowList != nil {
		for _, c := range inv.GetComponents() {
			if res := results[c.GetName()]; res == hostinventory.NotAllowed || res == hostinventory.NotListed {
				return no(infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed, c.GetName(),
					fmt.Sprintf("component %q version %q: %s by the allow-list", c.GetName(), c.GetObservedVersion(), allowListWords(res)))
			}
		}
		for _, c := range inv.GetComponents() {
			if results[c.GetName()] == hostinventory.NotChecked {
				return no(infravirtrigaudiov1beta1.HostReasonComponentsDigestsNotReported, c.GetName(),
					fmt.Sprintf("component %q reports no digest, so it cannot be compared with the allow-list", c.GetName()))
			}
		}
		for _, req := range p.AllowList.Required() {
			ok := false
			for _, c := range inv.GetComponents() {
				ok = ok || (c.GetName() == req && c.GetState() == providerv1.ComponentState_COMPONENT_STATE_READY)
			}
			if !ok {
				return no(infravirtrigaudiov1beta1.HostReasonComponentsRequiredAbsent, req,
					fmt.Sprintf("component %q is required by the allow-list but not reported ready", req))
			}
		}
	}
	for _, name := range p.RequiredFeatures {
		present := false
		for _, f := range inv.GetFeatures() {
			present = present || (f.GetName() == name && f.GetResult() == providerv1.FeatureResult_FEATURE_RESULT_PRESENT)
		}
		if !present {
			return no(infravirtrigaudiov1beta1.HostReasonComponentsFeatureMissing, "",
				fmt.Sprintf("required feature %q is not present on the host", name))
		}
	}
	if !slices.Contains(p.AcceptedTiers, inv.GetProcessIdentityTier()) {
		return no(infravirtrigaudiov1beta1.HostReasonComponentsIdentityTierNotAccepted, "",
			fmt.Sprintf("process identity tier %s is not accepted", tierName(inv.GetProcessIdentityTier())))
	}
	if p.AllowList == nil {
		return conditionVerdict{status: metav1.ConditionTrue, reason: infravirtrigaudiov1beta1.HostReasonComponentsAllowListNotConfigured,
			message: "every component is ready, but none was compared with an allow-list (--" +
				InsecureSkipComponentAllowListFlagName + "; development only)"}, ""
	}
	return conditionVerdict{status: metav1.ConditionTrue, reason: infravirtrigaudiov1beta1.HostReasonComponentsVerified,
		message: "every component is ready and allow-listed; " + note}, ""
}

// allowListWords says what an allow-list result means.
func allowListWords(r hostinventory.Result) string {
	if r == hostinventory.NotListed {
		return "not listed"
	}
	return "its digests are not allowed"
}

// Status mirror names of the inventory enums.
var (
	inventoryStateNames = map[providerv1.InventoryState]string{
		providerv1.InventoryState_INVENTORY_STATE_COMPLETE: "Complete", providerv1.InventoryState_INVENTORY_STATE_PENDING: "Pending",
		providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT: "OverLimit", providerv1.InventoryState_INVENTORY_STATE_UNREADABLE: "Unreadable",
	}
	componentStateNames = map[providerv1.ComponentState]string{
		providerv1.ComponentState_COMPONENT_STATE_READY: "Ready", providerv1.ComponentState_COMPONENT_STATE_MISSING: "Missing",
		providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE: "Incompatible", providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY: "Unhealthy",
		providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED:     "Unmanifested",
		providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID: "ManifestInvalid",
	}
	originNames = map[providerv1.ManifestOrigin]string{
		providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE: "None", providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY: "ReadOnly",
		providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE: "Writable",
	}
	featureResultNames = map[providerv1.FeatureResult]string{
		providerv1.FeatureResult_FEATURE_RESULT_PRESENT: "Present", providerv1.FeatureResult_FEATURE_RESULT_ABSENT: "Absent",
		providerv1.FeatureResult_FEATURE_RESULT_ERROR: "Error",
	}
	tierNames = map[providerv1.ProcessIdentityTier]string{
		providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFD_STORE:  "PidfdStore",
		providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE: "PidfsHandle",
		providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE:  "PidfsInode",
		providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME:   "StartTime",
	}
	extensionKindNames = map[providerv1.ExtensionKind]string{
		providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY:          "Directory",
		providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE:   "UndeclaredImage",
		providerv1.ExtensionKind_EXTENSION_KIND_OVERLAYS_COMPONENT: "OverlaysComponent",
	}
)

// originName is the mirror name of a manifest origin.
func originName(o providerv1.ManifestOrigin) string { return originNames[o] }

// tierName is the mirror name of a process identity tier ("" for none).
func tierName(t providerv1.ProcessIdentityTier) string { return tierNames[t] }

// digestString renders a raw SHA-256 as "sha256:<hex>" ("" for none).
func digestString(d []byte) string {
	if len(d) == 0 {
		return ""
	}
	return "sha256:" + hex.EncodeToString(d)
}

// inventoryMirror renders a verified inventory as Host.status.agentInventory.
// Every value comes from the verified inventory, whose fields the verifier
// bounded like the CRD.
func inventoryMirror(p *HostInventoryPolicy, r *hostRefresh, inv *providerv1.HostInventory,
	results map[string]hostinventory.Result) *infravirtrigaudiov1beta1.HostAgentInventoryStatus {
	at := metav1.NewTime(r.verifiedAt)
	m := &infravirtrigaudiov1beta1.HostAgentInventoryStatus{
		ObservedTime: &at, AnswerSHA256: hex.EncodeToString(r.answer.BlobSHA256[:]), AllowListSHA256: p.AllowListSHA256(),
		EnrolmentID: hex.EncodeToString(r.answer.Context.GetEnrolmentId()),
		InventoryVersion: int64(inv.GetInventoryVersion()), SupportedVersion: int64(inv.GetSupportedVersion()),
		State: inventoryStateNames[inv.GetState()], StateReason: inv.GetStateReason(),
		SnapshotAgeSeconds: int64(inv.GetSnapshotAgeSeconds()), ProcessIdentityTier: tierName(inv.GetProcessIdentityTier()),
	}
	for _, c := range inv.GetComponents() {
		mc := infravirtrigaudiov1beta1.HostInventoryComponent{Name: c.GetName(), State: componentStateNames[c.GetState()],
			Reason: c.GetStateReason(), ObservedVersion: c.GetObservedVersion(), DeclaredVersion: c.GetDeclaredVersion(),
			RequiredRange: c.GetRequiredRange(), Source: c.GetSource(), ManifestOrigin: originName(c.GetManifestOrigin()),
			ArtifactDigest: digestString(c.GetArtifactSha256()), AllowList: infravirtrigaudiov1beta1.HostInventoryAllowList(results[c.GetName()])}
		for _, f := range c.GetFiles() {
			mc.Files = append(mc.Files, infravirtrigaudiov1beta1.HostInventoryFile{Name: f.GetName(), Digest: digestString(f.GetSha256())})
		}
		m.Components = append(m.Components, mc)
	}
	for _, f := range inv.GetFeatures() {
		m.Features = append(m.Features, infravirtrigaudiov1beta1.HostInventoryFeature{Name: f.GetName(),
			Result: featureResultNames[f.GetResult()], Reason: f.GetReason()})
	}
	for _, e := range inv.GetUnexpectedExtensions() {
		m.UnexpectedExtensions = append(m.UnexpectedExtensions, infravirtrigaudiov1beta1.HostInventoryExtension{Name: e.GetName(),
			Kind: extensionKindNames[e.GetKind()], ImageDigest: digestString(e.GetImageSha256())})
	}
	return m
}

// hostInventoryInScope reports whether the Host controller judges the
// inventory of provider's hosts: a Provider whose type runs a per-host agent.
func hostInventoryInScope(provider *infravirtrigaudiov1beta1.Provider) bool {
	return identityRequired(provider)
}

// applyHostInventory writes one heartbeat's inventory verdict into host, in
// the same status write as the Provider's host information: the mirror, both
// conditions and, when the Provider reports the host ready but the conditions
// are not both True, health NotReady with Ready=False/AgentInventoryNotReady.
// Unsigned host information can only make a host not ready, never ready.
func (r *HostReconciler) applyHostInventory(ctx context.Context, host *infravirtrigaudiov1beta1.Host, obs inventoryObservation) {
	if !obs.inScope {
		clearHostInventory(host)
		return
	}
	p := r.HostAnswers.inventoryPolicy()
	v := evaluateHostInventory(p, obs, r.inventoryNow())
	before := slices.Clone(host.Status.Conditions)
	if v.mirror != nil {
		host.Status.AgentInventory = v.mirror
	} else {
		dropStaleInventoryMirror(host)
	}
	setHostCondition(host, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, v.verified.status, v.verified.reason, v.verified.message)
	setHostCondition(host, infravirtrigaudiov1beta1.HostConditionComponentsReady, v.components.status, v.components.reason, v.components.message)
	if host.Status.Health == infravirtrigaudiov1beta1.HostHealthReady && !v.ready() {
		failing, c := infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, v.verified
		if c.status == metav1.ConditionTrue {
			failing, c = infravirtrigaudiov1beta1.HostConditionComponentsReady, v.components
		}
		host.Status.Health = infravirtrigaudiov1beta1.HostHealthNotReady
		setHostReady(host, metav1.ConditionFalse, reasonAgentInventoryNotReady,
			fmt.Sprintf("the Provider reports the host ready, but %s is False (%s): %s", failing, c.reason, c.message))
	}
	r.auditInventory(ctx, host, before, v, obs.refresh)
}

// withdrawHostInventory sets both inventory conditions False/NoSignedAnswer
// for a heartbeat that ended without a host answer. With inScope false it
// does so only for a host that carries an inventory condition from an
// earlier heartbeat (the Provider's type is unknown, for example because the
// Provider is gone).
func withdrawHostInventory(host *infravirtrigaudiov1beta1.Host, inScope bool, why string) {
	dropStaleInventoryMirror(host)
	if !inScope && meta.FindStatusCondition(host.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified) == nil {
		return
	}
	setHostCondition(host, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer, why)
	setHostCondition(host, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonComponentsInventoryNotVerified,
		"the host inventory is not verified ("+infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer+")")
}

// clearHostInventory removes the inventory mirror and conditions of a host
// whose Provider does not run a per-host agent (it moved to another
// Provider).
func clearHostInventory(host *infravirtrigaudiov1beta1.Host) {
	host.Status.AgentInventory = nil
	meta.RemoveStatusCondition(&host.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified)
	meta.RemoveStatusCondition(&host.Status.Conditions, infravirtrigaudiov1beta1.HostConditionComponentsReady)
}

// dropStaleInventoryMirror clears the inventory mirror when it was verified
// under another enrolment than the host's current one (status.agent, the
// mirror of the pins Secret; none: no current enrolment).
func dropStaleInventoryMirror(host *infravirtrigaudiov1beta1.Host) {
	m := host.Status.AgentInventory
	if m == nil {
		return
	}
	current := ""
	if host.Status.Agent != nil {
		current = host.Status.Agent.EnrolmentID
	}
	if m.EnrolmentID != current {
		host.Status.AgentInventory = nil
	}
}

// inventoryNow is the clock of the inventory verdicts.
func (r *HostReconciler) inventoryNow() time.Time {
	if r.HostAnswers != nil {
		return r.HostAnswers.now()
	}
	return time.Now()
}

// inventoryBlobLogInterval is the least time between two log lines of one
// host that carry the whole signed answer; the lines in between carry its
// SHA-256 only.
const inventoryBlobLogInterval = 10 * time.Minute

// maxSuppressedReasons bounds the reasons a summary Event lists.
const maxSuppressedReasons = 8

// reasonInventoryTransitionsSuppressed is the reason of the Event that
// summarises the transitions of one condition suppressed within an Event
// window.
const reasonInventoryTransitionsSuppressed = "InventoryTransitionsSuppressed"

// inventoryAudit is the audit state of the host inventory conditions, per
// Host: the last observed reason and Event window of each condition, the
// transitions suppressed within the window, and the last signed answer
// logged in full. The zero value is ready to use.
type inventoryAudit struct {
	mu    sync.Mutex
	hosts map[string]*hostAuditState
}

// hostAuditState is the audit state of one Host.
type hostAuditState struct {
	conditions map[string]*conditionAuditState
	// blobSHA and blobAt are the SHA-256 of the last signed answer logged in
	// full and when.
	blobSHA string
	blobAt  time.Time
}

// conditionAuditState is the audit state of one condition of one Host.
type conditionAuditState struct {
	// observed is the last reason observed (Event or not).
	observed string
	// eventAt is when the last Event of the condition was recorded.
	eventAt time.Time
	// suppressed are the reasons of the transitions observed since eventAt
	// whose Event was suppressed (the count, and at most
	// maxSuppressedReasons of them).
	suppressed        int
	suppressedReasons []string
}

// eventDecision says which Event, if any, one observation records.
type eventDecision struct {
	// transition: record the observed transition.
	transition bool
	// summary: record a summary of the suppressed transitions.
	summary bool
	// suppressed and reasons are the suppressed transitions being reported.
	suppressed int
	reasons    []string
}

// host returns the audit state of host. The caller holds a.mu.
func (a *inventoryAudit) host(host string) *hostAuditState {
	if a.hosts == nil {
		a.hosts = map[string]*hostAuditState{}
	}
	s := a.hosts[host]
	if s == nil {
		s = &hostAuditState{conditions: map[string]*conditionAuditState{}}
		a.hosts[host] = s
	}
	return s
}

// observe records that host's condition has reason at now and decides the
// Event: a transition (a reason other than the last observed one) is
// recorded when the condition's last Event is at least inventoryEventInterval
// old, and otherwise suppressed and counted; the first observation after the
// window ends with suppressed transitions records them, as part of the
// transition Event or as a summary Event. So recovering and failing again
// within a window is never lost, and each condition records at most about
// one Event per window.
func (a *inventoryAudit) observe(host, condition, reason string, now time.Time) eventDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	cs := a.host(host).conditions[condition]
	if cs == nil {
		cs = &conditionAuditState{}
		a.host(host).conditions[condition] = cs
	}
	open := cs.eventAt.IsZero() || now.Sub(cs.eventAt) >= inventoryEventInterval
	changed := cs.observed != reason
	cs.observed = reason
	switch {
	case changed && open:
		d := eventDecision{transition: true, suppressed: cs.suppressed, reasons: cs.suppressedReasons}
		cs.eventAt, cs.suppressed, cs.suppressedReasons = now, 0, nil
		return d
	case changed:
		cs.suppressed++
		if len(cs.suppressedReasons) < maxSuppressedReasons {
			cs.suppressedReasons = append(cs.suppressedReasons, reason)
		}
		return eventDecision{}
	case open && cs.suppressed > 0:
		d := eventDecision{summary: true, suppressed: cs.suppressed, reasons: cs.suppressedReasons}
		cs.eventAt, cs.suppressed, cs.suppressedReasons = now, 0, nil
		return d
	}
	return eventDecision{}
}

// blob reports whether a log line of host may carry the whole signed answer
// with SHA-256 sha at now: never one already logged in full, and at most one
// per inventoryBlobLogInterval. It records it when it may.
func (a *inventoryAudit) blob(host, sha string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.host(host)
	if sha == "" || s.blobSHA == sha || (!s.blobAt.IsZero() && now.Sub(s.blobAt) < inventoryBlobLogInterval) {
		return false
	}
	s.blobSHA, s.blobAt = sha, now
	return true
}

// forget drops the audit state of host.
func (a *inventoryAudit) forget(host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.hosts, host)
}

// inventoryReasons is the closed set of reasons the inventory metric may
// carry; anything else is published as metricReasonOther.
var inventoryReasons = map[string]bool{
	infravirtrigaudiov1beta1.HostReasonInventoryVerified: true, infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer: true,
	infravirtrigaudiov1beta1.HostReasonInventoryMissing: true, infravirtrigaudiov1beta1.HostReasonInventoryInvalid: true,
	infravirtrigaudiov1beta1.HostReasonInventoryPending: true, infravirtrigaudiov1beta1.HostReasonInventoryStale: true,
	infravirtrigaudiov1beta1.HostReasonInventoryOverLimit: true, infravirtrigaudiov1beta1.HostReasonInventoryUnreadable: true,
	infravirtrigaudiov1beta1.HostReasonComponentsVerified: true, infravirtrigaudiov1beta1.HostReasonComponentsInventoryNotVerified: true,
	infravirtrigaudiov1beta1.HostReasonComponentsAgentNotReady: true, infravirtrigaudiov1beta1.HostReasonComponentsNoneReported: true,
	infravirtrigaudiov1beta1.HostReasonComponentsUnexpectedExtension: true, infravirtrigaudiov1beta1.HostReasonComponentsUnmanifested: true,
	infravirtrigaudiov1beta1.HostReasonComponentsManifestInvalid: true, infravirtrigaudiov1beta1.HostReasonComponentsManifestOriginNotAccepted: true,
	infravirtrigaudiov1beta1.HostReasonComponentsMissing: true, infravirtrigaudiov1beta1.HostReasonComponentsIncompatible: true,
	infravirtrigaudiov1beta1.HostReasonComponentsUnhealthy: true, infravirtrigaudiov1beta1.HostReasonComponentsAllowListNotConfigured: true,
	infravirtrigaudiov1beta1.HostReasonComponentsAllowListUnverified: true, infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed: true,
	infravirtrigaudiov1beta1.HostReasonComponentsDigestsNotReported: true, infravirtrigaudiov1beta1.HostReasonComponentsRequiredAbsent: true,
	infravirtrigaudiov1beta1.HostReasonComponentsFeatureMissing: true, infravirtrigaudiov1beta1.HostReasonComponentsIdentityTierNotAccepted: true,
}

// metricReasonOther is the metric reason of a reason outside the closed set.
const metricReasonOther = "Other"

// metricReason bounds a condition reason to the closed set.
func metricReason(reason string) string {
	if inventoryReasons[reason] {
		return reason
	}
	return metricReasonOther
}

// auditInventory writes the audit record of one heartbeat's inventory
// verdict: on a transition of either condition, one log line naming it, the
// component, and the SHA-256 of the signed answer and of the allow-list,
// which also carries the whole signed answer once per answer and at most once
// per host per inventoryBlobLogInterval; an Event per condition as observe
// decides; and the condition metric.
func (r *HostReconciler) auditInventory(ctx context.Context, host *infravirtrigaudiov1beta1.Host, before []metav1.Condition,
	v inventoryVerdict, refresh *hostRefresh) {
	answerSHA := ""
	if refresh != nil && refresh.verdict == answerVerified {
		answerSHA = hex.EncodeToString(refresh.answer.BlobSHA256[:])
	}
	allowSHA := r.HostAnswers.inventoryPolicy().AllowListSHA256()
	key := host.Namespace + "/" + host.Name
	now := r.inventoryNow()
	for _, c := range []struct {
		typ string
		cv  conditionVerdict
	}{
		{infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, v.verified},
		{infravirtrigaudiov1beta1.HostConditionComponentsReady, v.components},
	} {
		old := meta.FindStatusCondition(before, c.typ)
		previous := ""
		if old != nil {
			previous = metricReason(old.Reason)
		}
		metrics.SetHostInventoryCondition(host.Namespace, host.Name, c.typ, previous, metricReason(c.cv.reason), c.cv.status == metav1.ConditionTrue)
		if old == nil || old.Status != c.cv.status || old.Reason != c.cv.reason {
			kv := []any{"host", host.Name, "condition", c.typ, "status", c.cv.status, "reason", c.cv.reason, "component", v.component,
				"answerSHA256", answerSHA, "allowListSHA256", allowSHA}
			if r.inventoryEvents.blob(key, answerSHA, now) {
				if raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(blobOf(refresh.read.Answer)); err == nil {
					kv = append(kv, "signedAnswer", base64.StdEncoding.EncodeToString(raw))
				}
			}
			log.FromContext(ctx).Info("Host inventory condition changed", kv...)
		}
		d := r.inventoryEvents.observe(key, c.typ, c.cv.reason, now)
		if r.Recorder == nil || (!d.transition && !d.summary) {
			continue
		}
		eventType := corev1.EventTypeNormal
		if c.cv.status != metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		suppressed := ""
		if d.suppressed > 0 {
			suppressed = fmt.Sprintf("; %d earlier transition(s) suppressed in the last %s (reasons %v)", d.suppressed, inventoryEventInterval, d.reasons)
		}
		if d.summary {
			r.Recorder.Eventf(host, eventType, reasonInventoryTransitionsSuppressed,
				"%s: %d transition(s) suppressed in the last %s (reasons %v); current reason %s (%s), allow-list sha256 %q",
				c.typ, d.suppressed, inventoryEventInterval, d.reasons, c.cv.reason, c.cv.status, allowSHA)
			continue
		}
		r.Recorder.Eventf(host, eventType, c.cv.reason, "%s=%s (%s): component %q, answer sha256 %q, allow-list sha256 %q%s",
			c.typ, c.cv.status, c.cv.reason, v.component, answerSHA, allowSHA, suppressed)
	}
}

// forgetHostInventory drops what the manager holds in memory about a Host's
// inventory (its backoff, audit state and metric series), when the Host is
// gone or deleted.
func (r *HostReconciler) forgetHostInventory(key types.NamespacedName) {
	r.inventoryEvents.forget(key.Namespace + "/" + key.Name)
	metrics.DeleteHostInventoryConditions(key.Namespace, key.Name)
	if r.HostAnswers != nil {
		r.HostAnswers.forgetInventoryBackoff(key)
	}
}

// placementHeartbeatMaxAge is the scheduler's HeartbeatMaxAge for provider's
// hosts: the maximum host inventory age for a Provider whose type runs a
// per-host agent, 0 (no check) for every other Provider.
func placementHeartbeatMaxAge(provider *infravirtrigaudiov1beta1.Provider, answers *HostAnswers) time.Duration {
	if !hostInventoryInScope(provider) {
		return 0
	}
	return answers.inventoryPolicy().maxAge()
}
