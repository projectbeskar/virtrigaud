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

package contracts

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
)

// HostUnavailableHostKey is the google.rpc.ErrorInfo metadata key a clustered
// provider sets on a HostUnavailableReason status when the host it could not
// reach is the very host the call was routed to (the request's
// target_host_id): its value is that host's id, which the manager already
// sent. It is absent when the host that could not be reached may be another
// host of the Provider (the cluster-wide disk guard's answer), and from an
// older provider. The manager names the host in a VirtualMachine's status
// only when the value is the VM's own host, so no other host is ever named.
// It is never an endpoint, a URI or an address.
const HostUnavailableHostKey = "host"

// UnreachableHost returns the host a HostUnavailableReason answer in err's
// chain says could not be reached (HostUnavailableHostKey), or "" when err
// carries no such answer or the answer does not name the host.
func UnreachableHost(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok &&
			info.GetReason() == HostUnavailableReason && info.GetDomain() == HostUnavailableErrorDomain {
			return info.GetMetadata()[HostUnavailableHostKey]
		}
	}
	return ""
}
