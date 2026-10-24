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

package v1beta1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// unattachedDisksWarningFmt is the admission warning of a VirtualMachine whose
// spec.disks its Provider does not attach: the Provider's name and type.
const unattachedDisksWarningFmt = "spec.disks: Provider %q is of type %s, which does not attach additional disks; " +
	"these disks will not be created or attached (the VirtualMachine is accepted and runs without them, and its " +
	"DisksApplied condition says so)"

// unattachedDisksWarnings returns the admission warning for vm when its
// spec.disks is not empty and its Provider is of a type that does not attach
// additional disks (ProviderIgnoresAdditionalDisks). It never fails admission:
// no Reader, a Provider that cannot be read (missing, not yet created, an API
// error) or a Provider in another namespace (whose details a consumer may not
// be granted; the manager's DisksApplied condition covers it once the grant is
// checked) give no warning.
func (v *VirtualMachineDiskSizeValidator) unattachedDisksWarnings(ctx context.Context, vm *VirtualMachine) admission.Warnings {
	if v.Reader == nil || len(vm.Spec.Disks) == 0 || vm.Spec.ProviderRef.Name == "" {
		return nil
	}
	ns := vm.Namespace
	if ns == "" {
		if req, err := admission.RequestFromContext(ctx); err == nil {
			ns = req.Namespace
		}
	}
	if ns == "" || (vm.Spec.ProviderRef.Namespace != "" && vm.Spec.ProviderRef.Namespace != ns) {
		return nil
	}
	provider := &Provider{}
	if err := v.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: vm.Spec.ProviderRef.Name}, provider); err != nil {
		return nil
	}
	if !ProviderIgnoresAdditionalDisks(provider.Spec.Type) {
		return nil
	}
	return admission.Warnings{fmt.Sprintf(unattachedDisksWarningFmt, provider.Name, provider.Spec.Type)}
}
