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

// VMNetworkAttachmentReconciler reconciles a VMNetworkAttachment object
type VMNetworkAttachmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// VMNetworkAttachmentReconciler is a watch-only stub: its Reconcile is a
// no-op and it holds an informer cache on VMNetworkAttachment via For().
// Least-privilege (issue #152) therefore grants only the get;list;watch the
// cache requires — VMNetworkAttachment is read-only across the whole manager
// (no controller creates or mutates it). The apiGroup is infra.virtrigaud.io;
// the previous doubled value generated a phantom rule for a non-existent
// group.
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmnetworkattachments,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the VMNetworkAttachment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *VMNetworkAttachmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VMNetworkAttachment")
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

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VMNetworkAttachmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.VMNetworkAttachment{}).
		Named("vmnetworkattachment").
		Complete(r)
}
