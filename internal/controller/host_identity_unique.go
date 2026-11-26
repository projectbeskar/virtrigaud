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

	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// reasonEnrolIdentityInUse is the AgentEnrolled condition reason, the
// denial reason of an enrolment request and the event reason of a Host whose
// spec.identity another Host that is not revoked also carries: such a Host
// is not enrolled (development builds only). The identity names one
// machine, and the manager's host ledger follows a machine across Host
// objects only while no two live Hosts claim it.
const reasonEnrolIdentityInUse = "IdentityInUse"

// hostsWithIdentity counts the Hosts other than host (by UID), in every
// namespace, that carry host's spec.identity and are neither revoked nor
// deleting. It reads through reader (the API reader for a decision).
func hostsWithIdentity(ctx context.Context, reader client.Reader, host *infravirtrigaudiov1beta1.Host) (int, error) {
	if host.Spec.Identity == "" {
		return 0, nil
	}
	var list infravirtrigaudiov1beta1.HostList
	if err := reader.List(ctx, &list); err != nil {
		return 0, fmt.Errorf("list Hosts to check that the identity of Host %s/%s is unique: %w", host.Namespace, host.Name, err)
	}
	n := 0
	for i := range list.Items {
		o := &list.Items[i]
		if o.UID == host.UID || o.Spec.Identity != host.Spec.Identity || o.Spec.Revoked || !o.DeletionTimestamp.IsZero() {
			continue
		}
		n++
	}
	return n, nil
}
