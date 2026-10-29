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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestReconcileVM_StatusIPsAreNormalised pins V9: whatever a provider answers
// (a libvirt guest agent reports every address of every guest interface),
// status.ips lists only addresses a user can reach the VM on, in the
// provider's order, without duplicates.
func TestReconcileVM_StatusIPsAreNormalised(t *testing.T) {
	tests := []struct {
		name     string
		answered []string
		seeded   []string
		want     []string
	}{
		{
			name:     "loopback and link-local are dropped",
			answered: []string{"127.0.0.1", "::1", "192.0.2.31", "fe80::5054:ff:fe88:d0c1"},
			want:     []string{"192.0.2.31"},
		},
		{
			name:     "private and global addresses of both families keep the provider's order",
			answered: []string{"2001:db8::7", "10.0.0.4", "fd00::4", "192.0.2.4"},
			want:     []string{"2001:db8::7", "10.0.0.4", "fd00::4", "192.0.2.4"},
		},
		{
			name:     "duplicates, unspecified, multicast and garbage are dropped",
			answered: []string{"192.0.2.9", "0.0.0.0", "::", "224.0.0.251", "ff02::fb", "169.254.3.3", "N/A", "", "192.0.2.9"},
			want:     []string{"192.0.2.9"},
		},
		{
			name:     "a previously written dirty list is rewritten clean",
			answered: []string{"192.0.2.31"},
			seeded:   []string{"127.0.0.1", "::1", "192.0.2.31", "fe80::1"},
			want:     []string{"192.0.2.31"},
		},
		{
			name:     "nothing reachable leaves status.ips empty",
			answered: []string{"127.0.0.1", "::1", "fe80::5054:ff:fe88:d0c1"},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := boundClusterVM("app")
			vm.Status.IPs = tt.seeded
			prov := &routingProvider{describeResp: contracts.DescribeResponse{
				Exists: true, PowerState: "Off", IPs: tt.answered,
			}}
			r := clusteredFixture(t, prov, vm)

			_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, getVM(t, r, "app").Status.IPs)
		})
	}
}

// TestGetRequeueInterval_UnreachableAddressesAreNotAnIP pins that the
// "waiting for an IP" poll keys off the normalised list: a powered-on VM that
// only has a loopback or link-local address is still waiting for an IP.
func TestGetRequeueInterval_UnreachableAddressesAreNotAnIP(t *testing.T) {
	r := &VirtualMachineReconciler{}
	poweredOn := string(contracts.PowerStateOn)

	waiting := contracts.DescribeResponse{
		Exists: true, PowerState: poweredOn,
		IPs: contracts.RoutableIPs([]string{"127.0.0.1", "fe80::1"}),
	}
	assert.Empty(t, waiting.IPs)
	assert.Equal(t, 10*time.Second, r.getRequeueInterval(nil, waiting))

	reachable := contracts.DescribeResponse{
		Exists: true, PowerState: poweredOn,
		IPs: contracts.RoutableIPs([]string{"127.0.0.1", "192.0.2.5"}),
	}
	assert.Equal(t, []string{"192.0.2.5"}, reachable.IPs)
	assert.Equal(t, 2*time.Minute, r.getRequeueInterval(nil, reachable))
}
