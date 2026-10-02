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
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// providerUnreachableDetail is what consumer-readable status and events say
// in place of the text of a provider RPC that failed in the transport — the
// manager could not reach the provider at all.
const providerUnreachableDetail = "the provider could not be reached (the cause is in the manager log)"

// providerResolveFailedDetail is what consumer-readable status and events say
// when the manager could not resolve a usable client for a Provider (the
// remote resolver failed: runtime not Running or without an endpoint, TLS or
// gRPC client setup or Validate failing). The resolver's own error can name
// the TLS Secret, the provider Service's address and the runtime phase
// (ADR-0007 A6, threat 5), so it goes to the manager log only.
const providerResolveFailedDetail = "the manager cannot reach the provider (its client could not be set up; the cause is in the manager log)"

// transportFailureMarkers are fragments of the gRPC client's own connection
// errors (grpc-go transport and resolver), which carry the provider Service's
// address and are never an answer from the provider.
var transportFailureMarkers = []string{
	"connection error",
	"resolver error",
	"produced zero addresses",
	"dial tcp",
	"transport:",
}

// isTransportFailure reports whether err is a gRPC Unavailable failure of the
// transport itself (see transportFailureMarkers) rather than an answer from
// the provider.
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		return false
	}
	msg := st.Message()
	for _, marker := range transportFailureMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// rpcFailureDetail renders the error of a provider RPC for consumer-readable
// status and events (VMClone, VMSnapshot, VMMigration): the provider's answer
// made safe by sanitizeProviderDetail, or providerUnreachableDetail for a
// transport failure, whose text names the provider Service's address. The
// caller logs err in full.
func rpcFailureDetail(err error) string {
	if isTransportFailure(err) {
		return providerUnreachableDetail
	}
	return sanitizeProviderDetail(err)
}
