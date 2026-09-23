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

package scheduler

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// VM affinity/anti-affinity is evaluated at HOST granularity: a term "matches a
// host" when some already-placed VM ON that host matches the term's selector.
// This is the natural reading for ADR-0007 P1's flat Host model, where the only
// topology domain is the host.
//
// Deliberately NOT interpreted in P1 (documented deferrals, not silent no-ops):
// VMAffinityTerm.TopologyKey (every term is treated as host-level co-location),
// Namespaces, and NamespaceSelector — the caller supplies the already-placed VM
// set already scoped to the namespaces it wants considered.

// hostRunsMatchingVM reports whether any VM already placed on hostID matches the
// term. It returns an error only when the term's label selector is malformed.
func (ec *evalContext) hostRunsMatchingVM(hostID string, term v1beta1.VMAffinityTerm) (bool, error) {
	for _, p := range ec.placedByHost[hostID] {
		ok, err := termMatchesVM(term, p.Labels)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// termMatchesVM reports whether a VM with vmLabels satisfies the term. A term
// combines its metav1 LabelSelector (when set) AND its MatchExpressions (when
// present); an empty term (neither set) matches every VM, mirroring Kubernetes
// empty-selector semantics.
func termMatchesVM(term v1beta1.VMAffinityTerm, vmLabels map[string]string) (bool, error) {
	if term.LabelSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			return false, fmt.Errorf("parse VM affinity label selector: %w", err)
		}
		if !sel.Matches(labels.Set(vmLabels)) {
			return false, nil
		}
	}
	for _, req := range term.MatchExpressions {
		if !evalSelectorRequirement(req, vmLabels) {
			return false, nil
		}
	}
	return true, nil
}

// evalSelectorRequirement evaluates one VMSelectorRequirement (the project's own
// In/NotIn/Exists/DoesNotExist vocabulary) against a VM's labels. An unknown
// operator matches nothing (fail-closed).
func evalSelectorRequirement(req v1beta1.VMSelectorRequirement, vmLabels map[string]string) bool {
	val, present := vmLabels[req.Key]
	switch req.Operator {
	case v1beta1.VMSelectorOpIn:
		return present && containsString(req.Values, val)
	case v1beta1.VMSelectorOpNotIn:
		// NotIn is satisfied when the key is absent or its value is not listed.
		return !present || !containsString(req.Values, val)
	case v1beta1.VMSelectorOpExists:
		return present
	case v1beta1.VMSelectorOpDoesNotExist:
		return !present
	default:
		return false
	}
}
