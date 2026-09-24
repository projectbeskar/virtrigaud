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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// hostInventoryVolumeName is the Pod volume name for the clustered
// host-inventory Secret. Referenced by both the Volume (buildPodVolumes) and the
// VolumeMount (buildProviderContainer) — the two must match.
const hostInventoryVolumeName = "host-inventory"

// Credential-Secret data keys the render loop extracts per host and inlines into
// the host-inventory Secret. They MIRROR the keys the libvirt provider reads
// today from its mounted credential Secret (internal/providers/libvirt:
// /etc/virtrigaud/credentials/<key>), so the clustered provider (a later PR)
// consumes the inlined material through the same code path as single-host
// credentials. "ssh-privatekey" follows the kubernetes.io/ssh-auth convention.
const (
	credentialSecretKeySSHPrivateKey = "ssh-privatekey"
	credentialSecretKeyKnownHosts    = "known_hosts"
)

// Condition/event vocabulary for per-host credential resolution surfaced on the
// Provider. The messages carry a host id and a coarse reason ONLY — never a
// credential value (ADR-0007 Security; #297).
const (
	// conditionHostCredentialsReady is True when every fronted Host's credentials
	// resolved and were inlined, False when one or more hosts were skipped for
	// unresolved/malformed/rejected credentials (the rendered inventory still
	// carries the hosts that did resolve).
	conditionHostCredentialsReady = "HostCredentialsReady"
	reasonCredentialsResolved     = "CredentialsResolved"
	reasonCredentialsUnresolved   = "CredentialsUnresolved"
	// reasonCredentialRefNamespaceRejected is the HostCredentialsReady=False
	// reason when at least one host was skipped because the credential Secret it
	// would use is referenced OUTSIDE the Provider's namespace (the clustered
	// Provider's own spec.credentialSecretRef.namespace set to another
	// namespace). The operator never reads such a Secret on the inventory's
	// behalf: doing so would let whoever can author the Provider copy SSH
	// material out of a namespace they cannot read. It takes precedence over
	// reasonCredentialsUnresolved because it is a policy decision, not a
	// transient absence.
	reasonCredentialRefNamespaceRejected = "CredentialRefNamespaceRejected"
	// eventReasonCredentialsUnresolved is the Warning event reason emitted when a
	// host is skipped for unresolved or rejected credentials.
	eventReasonCredentialsUnresolved = "HostCredentialsUnresolved"
	// eventReasonHostInventoryEntrySkipped is the Warning event reason emitted
	// when a host is skipped for a NON-credential reason: an endpoint outside the
	// accepted shapes (hostsecret.ValidateEndpoint) or a duplicated host id. Both
	// are defense in depth behind CRD admission and per-namespace name
	// uniqueness, so they are surfaced as an event + log rather than folded into
	// the credential condition.
	eventReasonHostInventoryEntrySkipped = "HostInventoryEntrySkipped"
)

// skipKind classifies why a host was dropped from the rendered inventory, so
// credential problems drive HostCredentialsReady while malformed entries are
// surfaced separately.
type skipKind int

const (
	// skipCredentialsUnresolved: the credential Secret is missing, unreadable,
	// or carries no SSH private key.
	skipCredentialsUnresolved skipKind = iota
	// skipCredentialRefRejected: the credential Secret reference points outside
	// the Provider's namespace and was deliberately NOT read.
	skipCredentialRefRejected
	// skipInvalidEntry: the Host itself is not renderable (invalid endpoint or a
	// duplicated host id).
	skipInvalidEntry
)

// RBAC for the clustered host-inventory pipeline (ADR-0007 D3). Least-privilege:
//   - Host/HostPool: get;list;watch only — the controller reads the admin's
//     inventory and watches it to re-render; it never writes Hosts/HostPools.
//   - secrets: get;create;update — the controller creates and updates the one
//     host-inventory Secret it owns per clustered Provider. No delete verb:
//     the Secret is owner-referenced to its Provider and reclaimed by
//     garbage collection on Provider deletion (no cluster-wide secret-delete
//     grant on the manager). The manager already holds secrets get;list;watch;
//     the net-new grant here is create;update (see charts manager-rbac.yaml).
//
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hostpools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update

// isClusterTopology reports whether the Provider runs in clustered topology
// (ADR-0007 D9). An unset topology ("") is the defaulted "single" — the
// apiserver applies the default=single, but the controller treats "" and
// "single" identically so it is correct even against un-defaulted objects (e.g.
// a fake client that does not run CRD defaulting).
func isClusterTopology(provider *infravirtrigaudiov1beta1.Provider) bool {
	return provider.Spec.Topology == infravirtrigaudiov1beta1.ProviderTopologyCluster
}

// getHostInventorySecretName returns the deterministic name of the Provider's
// host-inventory Secret: "<provider>-hosts". The Secret is namespaced (it lives
// in the Provider's namespace), so the provider name alone is unique.
func (r *ProviderReconciler) getHostInventorySecretName(provider *infravirtrigaudiov1beta1.Provider) string {
	return provider.Name + "-hosts"
}

// hostBelongsToProvider reports whether a Host CR is fronted by the given
// Provider. Under the same-namespace model (security) a Host belongs to a
// Provider only when it lives in the Provider's namespace AND its
// spec.providerRef (a LocalObjectReference) names that Provider. The namespace
// check is defense in depth: the render already lists Hosts with
// client.InNamespace(provider.Namespace), and the API no longer has a way to
// express a cross-namespace providerRef — but a Host from another namespace must
// never be able to enrol into this Provider's inventory even if a caller hands
// one in.
func hostBelongsToProvider(host *infravirtrigaudiov1beta1.Host, provider *infravirtrigaudiov1beta1.Provider) bool {
	return host.Namespace == provider.Namespace && host.Spec.ProviderRef.Name == provider.Name
}

// reconcileHostInventorySecret renders a clustered Provider's Host CRs into its
// projected host-inventory Secret (ADR-0007 D3): a versioned-schema document
// (hostsecret.Inventory) written to a Provider-owned Secret in the Provider's
// namespace, mounted read-only into the provider pod by buildPodVolumes /
// buildProviderContainer.
//
// For a single-topology Provider (the default) this is a no-op that issues NO
// API calls, so the single-host reconcile path is byte-for-byte the pre-ADR-0007
// behavior (D9). A rare cluster->single flip leaves the previously-rendered,
// owner-referenced Secret in place until the Provider is deleted (the manager
// deliberately holds no secrets delete verb). That Secret DOES carry the inlined
// SSH credentials of every host it rendered: it stays inside the same
// protection boundary as the source credential Secrets (Opaque, Provider-owned,
// in the Provider's namespace) and is no longer mounted by a single-topology
// pod, but an operator flipping a Provider back to single should delete
// "<provider>-hosts" by hand if the credentials should not linger.
//
// SAME-NAMESPACE MODEL (security): Hosts are listed in the Provider's namespace
// ONLY, and every credential Secret is resolved in the Provider's namespace
// ONLY. A Host in another namespace can therefore neither enrol itself into
// this Provider's inventory (and inherit its SSH identity) nor make the manager
// copy a Secret from a namespace its author cannot read.
//
// SECURITY: this renders host metadata (id/endpoint/labels) AND inlines each
// host's SSH connection material (private key + known_hosts) into the Secret so
// a thin, API-less provider can connect using only the mounted file (#297). The
// material is read from the referenced credential Secret and copied verbatim
// into the (Opaque, Provider-owned) inventory Secret; it is NEVER logged, put in
// Status, or put in an event — the log line records the host COUNT only, and the
// credential condition/event names host ids and coarse reasons only. A host
// whose credentials cannot be resolved (Secret missing, missing the SSH private
// key, or referenced outside the Provider namespace), whose endpoint fails
// hostsecret.ValidateEndpoint, or whose id is duplicated is SKIPPED from the
// render — never rendered half-usable and never failing the whole reconcile —
// and surfaced via a non-secret condition/event so siblings still render
// (fail-safe, never fail-open).
func (r *ProviderReconciler) reconcileHostInventorySecret(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) error {
	if !isClusterTopology(provider) {
		return nil
	}
	logger := log.FromContext(ctx)

	// Gather the Host CRs this Provider fronts: same namespace only (see the
	// SAME-NAMESPACE MODEL note above). hostBelongsToProvider re-checks the
	// namespace as defense in depth.
	hostList := &infravirtrigaudiov1beta1.HostList{}
	if err := r.List(ctx, hostList, client.InNamespace(provider.Namespace)); err != nil {
		return fmt.Errorf("list Hosts for provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	fronted := make([]*infravirtrigaudiov1beta1.Host, 0, len(hostList.Items))
	idCount := make(map[string]int, len(hostList.Items))
	for i := range hostList.Items {
		h := &hostList.Items[i]
		if !hostBelongsToProvider(h, provider) {
			continue
		}
		fronted = append(fronted, h)
		idCount[h.Name]++
	}
	// Deterministic processing order (and therefore deterministic skip
	// messages): stable by host id, independent of the List order.
	sort.SliceStable(fronted, func(i, j int) bool { return fronted[i].Name < fronted[j].Name })

	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	var skipped []skippedHost
	dupReported := make(map[string]struct{})
	for _, h := range fronted {
		// A duplicated id is ambiguous (which endpoint/credentials?), so EVERY
		// entry carrying it is skipped rather than "first wins". Unreachable via
		// the apiserver (names are unique per namespace) — defense in depth, and
		// it keeps hostsecret.Marshal's duplicate rejection from failing the
		// whole render.
		if n := idCount[h.Name]; n > 1 {
			// Every occurrence is skipped; the id is reported once.
			if _, done := dupReported[h.Name]; !done {
				dupReported[h.Name] = struct{}{}
				skipped = append(skipped, skippedHost{id: h.Name, kind: skipInvalidEntry,
					reason: fmt.Sprintf("duplicate host id (%d entries); none rendered", n)})
			}
			continue
		}
		// Re-validate the endpoint the CRD already admitted (defense in depth:
		// the endpoint's path becomes the libvirt URI forwarded to the host).
		// The error never echoes the endpoint value.
		if err := hostsecret.ValidateEndpoint(h.Spec.Endpoint); err != nil {
			skipped = append(skipped, skippedHost{id: h.Name, kind: skipInvalidEntry, reason: err.Error()})
			continue
		}
		creds, kind, skipReason := r.resolveHostCredentials(ctx, provider, h)
		if skipReason != "" {
			// Fail-safe: drop this one host from the inventory and record a
			// non-secret reason. Do NOT fail the reconcile — the other hosts
			// still render, and a Host/Secret edit re-triggers a re-render.
			skipped = append(skipped, skippedHost{id: h.Name, kind: kind, reason: skipReason})
			continue
		}
		inv.Hosts = append(inv.Hosts, hostsecret.Host{
			ID:          h.Name,
			Endpoint:    h.Spec.Endpoint,
			Labels:      h.Spec.Labels,
			Credentials: creds,
		})
	}

	data, err := hostsecret.Marshal(inv)
	if err != nil {
		return fmt.Errorf("marshal host inventory for provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.getHostInventorySecretName(provider),
			Namespace: provider.Namespace,
		},
	}
	// CreateOrUpdate skips the Update when the mutated object equals the fetched
	// one (semantic DeepEqual). Combined with hostsecret.Marshal's deterministic
	// output, an unchanged inventory produces no Secret write (idempotent).
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		if secret.Labels == nil {
			secret.Labels = make(map[string]string, 4)
		}
		secret.Labels["app.kubernetes.io/name"] = "virtrigaud-provider"
		secret.Labels["app.kubernetes.io/instance"] = provider.Name
		secret.Labels["app.kubernetes.io/component"] = "host-inventory"
		secret.Labels["app.kubernetes.io/managed-by"] = "virtrigaud"
		// The controller fully owns this Secret's data: replace, do not merge.
		secret.Data = map[string][]byte{hostsecret.SecretDataKey: data}
		// Owner reference so the Secret is garbage-collected with the Provider.
		return controllerutil.SetControllerReference(provider, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply host-inventory Secret for provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		// Log the host COUNT only — never endpoints, labels, or Secret contents.
		logger.Info("Reconciled clustered host-inventory Secret",
			"operation", op, "secret", secret.Name, "hosts", len(inv.Hosts))
	}
	// Surface credential-resolution health on the Provider (condition + event +
	// log). This mutates provider.Status.Conditions, which the Provider Reconcile
	// persists after reconcileRemoteRuntime returns.
	r.surfaceHostCredentialStatus(ctx, provider, len(inv.Hosts), skipped)
	return nil
}

// skippedHost records a host dropped from the rendered inventory, its skipKind,
// and the coarse, NON-SECRET reason (a host id and a short phrase — never a
// credential value or an endpoint).
type skippedHost struct {
	id     string
	kind   skipKind
	reason string
}

// resolveHostCredentials resolves and inlines one Host's SSH connection material
// for the rendered inventory. It reads the referenced credential Secret (the
// Host's own spec.credentialSecretRef when set, otherwise the Provider's default
// spec.credentialSecretRef) — ALWAYS in the Provider's namespace — and extracts
// the same keys the libvirt provider consumes today
// (credentialSecretKeySSHPrivateKey, credentialSecretKeyKnownHosts).
//
// On success it returns the populated Credentials and an empty reason. When the
// material cannot be resolved it returns the zero Credentials, the skipKind,
// and a coarse, NON-SECRET reason (reference outside the Provider namespace,
// Secret missing/unreadable, or no SSH private key present); the caller SKIPS
// that host rather than rendering it half-usable or failing the whole
// reconcile. It never logs or returns any credential value — the returned
// reason is safe to place in a condition/event.
func (r *ProviderReconciler) resolveHostCredentials(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, host *infravirtrigaudiov1beta1.Host) (hostsecret.Credentials, skipKind, string) {
	key, rejected := hostCredentialSecretKey(host, provider)
	if rejected != "" {
		// Deliberately NOT read: see hostCredentialSecretKey.
		return hostsecret.Credentials{}, skipCredentialRefRejected, rejected
	}
	if key.Name == "" {
		return hostsecret.Credentials{}, skipCredentialsUnresolved, "no credential secret reference on host or Provider"
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return hostsecret.Credentials{}, skipCredentialsUnresolved,
				fmt.Sprintf("credential secret %s/%s not found", key.Namespace, key.Name)
		}
		// A non-NotFound error (e.g. forbidden, transient) is API metadata only —
		// it never carries Secret data — so it is safe to surface coarsely.
		return hostsecret.Credentials{}, skipCredentialsUnresolved,
			fmt.Sprintf("credential secret %s/%s unreadable: %v", key.Namespace, key.Name, err)
	}

	// The SSH private key is the mandatory auth material: a Secret without it is
	// unusable for the key-based SSH the clustered model standardizes on. Presence
	// is judged after trimming surrounding whitespace, but the value is inlined
	// VERBATIM (untrimmed) so it round-trips byte-for-byte.
	privateKey := secret.Data[credentialSecretKeySSHPrivateKey]
	if len(bytes.TrimSpace(privateKey)) == 0 {
		return hostsecret.Credentials{}, skipCredentialsUnresolved,
			fmt.Sprintf("credential secret %s/%s has no %q", key.Namespace, key.Name, credentialSecretKeySSHPrivateKey)
	}
	creds := hostsecret.Credentials{SSHPrivateKey: privateKey}

	// known_hosts is inlined when present; when absent it is omitted and the
	// provider enforces its ADR-0004 host-key policy at connect time. Copied
	// verbatim (untrimmed) for a byte-for-byte round-trip.
	if kh := secret.Data[credentialSecretKeyKnownHosts]; len(bytes.TrimSpace(kh)) > 0 {
		creds.KnownHosts = kh
	}
	return creds, skipCredentialsUnresolved, ""
}

// hostCredentialSecretKey resolves which credential Secret a host's material
// comes from — the host's own spec.credentialSecretRef when set, otherwise the
// Provider's spec.credentialSecretRef (the default) — and ALWAYS resolves it in
// the Provider's namespace (security: same-namespace model).
//
// Host.spec.credentialSecretRef is a LocalObjectReference, so it can only name
// a Secret in the Host's namespace, which hostBelongsToProvider has already
// pinned to the Provider's. The Provider's own spec.credentialSecretRef is an
// ObjectRef on a RELEASED API and can still carry a namespace: when that
// namespace is set and differs from the Provider's, the Secret is NOT read and
// a non-empty, non-secret rejection reason is returned instead. Reading it
// would make the manager's cluster-wide Secret access copy SSH material out of
// a namespace the Provider's author may not be able to read (and a
// single-host provider pod could never mount a cross-namespace Secret anyway,
// so no working configuration relies on it).
func hostCredentialSecretKey(host *infravirtrigaudiov1beta1.Host, provider *infravirtrigaudiov1beta1.Provider) (types.NamespacedName, string) {
	if ref := host.Spec.CredentialSecretRef; ref != nil {
		return types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, ""
	}
	ref := provider.Spec.CredentialSecretRef
	if ref.Namespace != "" && ref.Namespace != provider.Namespace {
		return types.NamespacedName{}, fmt.Sprintf(
			"Provider credentialSecretRef names namespace %q; clustered host credentials are only read from the Provider's namespace %q",
			ref.Namespace, provider.Namespace)
	}
	return types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, ""
}

// surfaceHostCredentialStatus records per-host render health on the Provider:
//
//   - a HostCredentialsReady condition: True when no host was skipped for a
//     credential reason; False naming the credential-skipped hosts, with reason
//     CredentialRefNamespaceRejected when any was skipped for a cross-namespace
//     reference (a policy decision), else CredentialsUnresolved;
//   - a Warning event per class of skip (credentials vs. invalid entry);
//   - a structured log line per class.
//
// Every message names host ids and coarse reasons ONLY — never a credential
// value or an endpoint (ADR-0007 Security).
func (r *ProviderReconciler) surfaceHostCredentialStatus(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, rendered int, skipped []skippedHost) {
	var credSkips, entrySkips []string
	condReason := reasonCredentialsUnresolved
	for _, s := range skipped {
		part := fmt.Sprintf("%s (%s)", s.id, s.reason)
		switch s.kind {
		case skipInvalidEntry:
			entrySkips = append(entrySkips, part)
		case skipCredentialRefRejected:
			condReason = reasonCredentialRefNamespaceRejected
			credSkips = append(credSkips, part)
		default:
			credSkips = append(credSkips, part)
		}
	}
	logger := log.FromContext(ctx)

	if len(entrySkips) > 0 {
		detail := strings.Join(entrySkips, "; ")
		msg := fmt.Sprintf("skipped %d invalid host inventory entr(y/ies): %s", len(entrySkips), detail)
		if r.Recorder != nil {
			r.Recorder.Event(provider, corev1.EventTypeWarning, eventReasonHostInventoryEntrySkipped, msg)
		}
		logger.Info("clustered host-inventory: skipped invalid host entries",
			"provider", provider.Name, "skipped", len(entrySkips), "rendered", rendered, "detail", detail)
	}

	if len(credSkips) == 0 {
		k8s.SetCondition(&provider.Status.Conditions, conditionHostCredentialsReady, metav1.ConditionTrue,
			reasonCredentialsResolved, fmt.Sprintf("resolved credentials for all %d host(s)", rendered))
		return
	}

	detail := strings.Join(credSkips, "; ")
	msg := fmt.Sprintf("skipped %d host(s) with unresolved credentials: %s; %d host(s) rendered",
		len(credSkips), detail, rendered)

	k8s.SetCondition(&provider.Status.Conditions, conditionHostCredentialsReady, metav1.ConditionFalse,
		condReason, msg)
	if r.Recorder != nil {
		r.Recorder.Event(provider, corev1.EventTypeWarning, eventReasonCredentialsUnresolved, msg)
	}
	// Structured WARN-level signal; host ids + coarse reasons only.
	logger.Info("clustered host-inventory: skipped hosts with unresolved credentials",
		"provider", provider.Name, "skipped", len(credSkips), "rendered", rendered, "detail", detail)
}

// providersForHost maps a Host event to a reconcile request for the Provider it
// names in spec.providerRef — always in the Host's own namespace (same-namespace
// model) — so adding/removing/editing a Host re-renders that Provider's
// host-inventory Secret promptly (ADR-0007 D3) instead of waiting for the next
// resync.
func (r *ProviderReconciler) providersForHost(_ context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infravirtrigaudiov1beta1.Host)
	if !ok {
		return nil
	}
	return providerRequestForRef(host.Spec.ProviderRef, host.Namespace)
}

// providersForHostPool maps a HostPool event to a reconcile request for the
// Provider it names in spec.providerRef (in the pool's own namespace). The
// pool's fields do not enter the rendered Secret in this structural PR (only
// Host id/endpoint/labels do), so the re-render is idempotent today; the watch
// is wired so pool policy can enter the inventory additively in a later
// ADR-0007 slice without a plumbing change.
func (r *ProviderReconciler) providersForHostPool(_ context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*infravirtrigaudiov1beta1.HostPool)
	if !ok {
		return nil
	}
	return providerRequestForRef(pool.Spec.ProviderRef, pool.Namespace)
}

// providerRequestForRef builds the single reconcile request addressed by a
// namespace-local reference to a Provider: the Provider is always looked up in
// the referring object's own namespace.
func providerRequestForRef(ref infravirtrigaudiov1beta1.LocalObjectReference, objNamespace string) []reconcile.Request {
	if ref.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: objNamespace, Name: ref.Name},
	}}
}
