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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

const (
	// errReasonGetVMSet is the metrics.RecordError reason for a failed VMSet
	// Get in the reconcile entry path.
	errReasonGetVMSet = "get-vmset"

	// vmSetConditionReady is the Ready condition type reported on a VMSet.
	vmSetConditionReady = "Ready"
	// vmSetReasonNotImplemented is the Ready=False reason set by the
	// not-yet-active VMSet stub controller (issue #179).
	vmSetReasonNotImplemented = "ControllerNotImplemented"
	// vmSetNotImplementedMessage is the human-readable message reported by the
	// VMSet stub controller.
	vmSetNotImplementedMessage = "VMSet has no active controller in this release (#179)"
)

// VMSetReconciler is a not-yet-active stub for the VMSet resource. It reports a
// Ready=False / ControllerNotImplemented condition so operators get a clear
// signal that VMSet has no functional controller in this release, and does
// nothing else (issue #179).
type VMSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsets/status,verbs=get;update;patch

// Reconcile sets the not-implemented condition on the VMSet and returns.
//
// Named return values (`result`, `retErr`) are required by the deferred
// outcome-inference block — do not change the signature without updating it.
func (r *VMSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VMSet")
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

	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmset-%s/%s", req.Namespace, req.Name))
	logger := logging.FromContext(ctx)

	vmSet := &infrav1beta1.VMSet{}
	if err := r.Get(ctx, req.NamespacedName, vmSet); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VMSet")
		metrics.RecordError(errReasonGetVMSet, metrics.ComponentManager)
		return ctrl.Result{}, err
	}

	// Idempotent: only write status when the condition is not already set as
	// expected, to avoid a reconcile loop on our own status updates.
	existing := k8s.GetCondition(vmSet.Status.Conditions, vmSetConditionReady)
	if existing != nil &&
		existing.Status == metav1.ConditionFalse &&
		existing.Reason == vmSetReasonNotImplemented &&
		vmSet.Status.ObservedGeneration == vmSet.Generation {
		return ctrl.Result{}, nil
	}

	logger.Info("VMSet has no active controller; reporting ControllerNotImplemented")
	vmSet.Status.ObservedGeneration = vmSet.Generation
	k8s.SetCondition(&vmSet.Status.Conditions, vmSetConditionReady,
		metav1.ConditionFalse, vmSetReasonNotImplemented, vmSetNotImplementedMessage)
	if err := r.Status().Update(ctx, vmSet); err != nil {
		logger.Error(err, "Failed to update VMSet status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VMSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.VMSet{}).
		Complete(r)
}
