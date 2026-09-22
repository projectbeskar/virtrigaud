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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// hostInventoryVolumeName is the Pod volume name for the clustered
// host-inventory Secret. Referenced by both the Volume (buildPodVolumes) and the
// VolumeMount (buildProviderContainer) — the two must match.
const hostInventoryVolumeName = "host-inventory"

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
// Provider, matching Host.spec.providerRef by name and resolved namespace. The
// ref namespace defaults to the Host's own namespace when unset — the same
// resolution countConnectedVMs uses for VirtualMachine.spec.providerRef.
func hostBelongsToProvider(host *infravirtrigaudiov1beta1.Host, provider *infravirtrigaudiov1beta1.Provider) bool {
	if host.Spec.ProviderRef.Name != provider.Name {
		return false
	}
	ns := host.Spec.ProviderRef.Namespace
	if ns == "" {
		ns = host.Namespace
	}
	return ns == provider.Namespace
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
// owner-referenced Secret to be garbage-collected on Provider deletion; it
// carries only host metadata (never credentials), so it is harmless meanwhile.
//
// SECURITY: this renders host METADATA only (id/endpoint/labels). The
// credentials sub-document is left empty (hostsecret.Credentials); credential
// resolution is a separate, security-reviewed PR. Host connection material is
// never read or logged here — the log line records the host COUNT only.
func (r *ProviderReconciler) reconcileHostInventorySecret(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) error {
	if !isClusterTopology(provider) {
		return nil
	}
	logger := log.FromContext(ctx)

	// Gather the Host CRs this Provider fronts. Listing cluster-wide (then
	// filtering by resolved providerRef namespace) supports an explicit
	// cross-namespace Host.spec.providerRef, mirroring countConnectedVMs.
	hostList := &infravirtrigaudiov1beta1.HostList{}
	if err := r.List(ctx, hostList); err != nil {
		return fmt.Errorf("list Hosts for provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}

	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	for i := range hostList.Items {
		h := &hostList.Items[i]
		if !hostBelongsToProvider(h, provider) {
			continue
		}
		inv.Hosts = append(inv.Hosts, hostsecret.Host{
			ID:       h.Name,
			Endpoint: h.Spec.Endpoint,
			Labels:   h.Spec.Labels,
			// Credentials intentionally left empty in this PR (see doc comment).
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
	return nil
}

// providersForHost maps a Host event to a reconcile request for the Provider it
// names in spec.providerRef, so adding/removing/editing a Host re-renders that
// Provider's host-inventory Secret promptly (ADR-0007 D3) instead of waiting for
// the next resync.
func (r *ProviderReconciler) providersForHost(_ context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infravirtrigaudiov1beta1.Host)
	if !ok {
		return nil
	}
	return providerRequestForRef(host.Spec.ProviderRef, host.Namespace)
}

// providersForHostPool maps a HostPool event to a reconcile request for the
// Provider it names in spec.providerRef. The pool's fields do not enter the
// rendered Secret in this structural PR (only Host id/endpoint/labels do), so
// the re-render is idempotent today; the watch is wired so pool policy can enter
// the inventory additively in a later ADR-0007 slice without a plumbing change.
func (r *ProviderReconciler) providersForHostPool(_ context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*infravirtrigaudiov1beta1.HostPool)
	if !ok {
		return nil
	}
	return providerRequestForRef(pool.Spec.ProviderRef, pool.Namespace)
}

// providerRequestForRef builds the single reconcile request addressed by an
// ObjectRef to a Provider. The ref namespace defaults to the referring object's
// namespace when unset (matching hostBelongsToProvider / countConnectedVMs).
func providerRequestForRef(ref infravirtrigaudiov1beta1.ObjectRef, objNamespace string) []reconcile.Request {
	if ref.Name == "" {
		return nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = objNamespace
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: ref.Name},
	}}
}
