/*
Copyright 2025.

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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// VMImageReconciler reconciles a VMImage object
type VMImageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// VMImageReconciler is intentionally a watch-only no-op backstop: it holds an
// informer cache on VMImage via For() but performs no writes.
//
// Image preparation (issue #154) is driven entirely by the VirtualMachine
// controller, which is the only actor holding the (image, provider) pair. That
// controller is the SINGLE WRITER of the prepare-related VMImage status fields
// (ProviderStatus, PrepareTaskRef, Phase, Ready, AvailableOn) and writes them
// under retry.RetryOnConflict (see EnsureImageOnProvider). Keeping this
// reconciler a no-op deliberately avoids a second writer and the two-writer
// status race that bit issue #189 — there is no provider resolver wired here, so
// it could not poll a prepare task to completion anyway. The triggering VM polls
// its own prepare to completion.
//
// RBAC for vmimages/status get;update;patch is therefore granted on the
// VirtualMachine reconciler (the writer), not here; this reconciler needs only
// the get;list;watch its cache requires. The apiGroup is infra.virtrigaud.io.
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages,verbs=get;list;watch

// Reconcile is a no-op backstop. The VirtualMachine controller is the sole
// driver of VMImage preparation and status (issue #154); this reconciler exists
// only to hold the VMImage informer cache. It records reconcile metrics for
// consistency with the other reconcilers and returns without requeueing.
func (r *VMImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VMImage")
	defer func() {
		outcome := metrics.OutcomeSuccess
		switch {
		case retErr != nil:
			outcome = metrics.OutcomeError
		case result.Requeue || result.RequeueAfter > 0:
			outcome = metrics.OutcomeRequeue
		}
		timer.Finish(outcome)
	}()

	_ = logf.FromContext(ctx)

	// No-op: the VirtualMachine controller is the sole driver of VMImage
	// preparation and status (issue #154). See the type doc above.
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VMImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.VMImage{}).
		Named("vmimage").
		Complete(r)
}
