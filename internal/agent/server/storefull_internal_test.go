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

package server

import (
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// TestStoreFullRefusalIsMachineReadable: a refusal because the store is
// full is RESOURCE_EXHAUSTED with the STORE_FULL reason in the agent's
// ErrorInfo domain, and its message names the re-key as the remedy.
func TestStoreFullRefusalIsMachineReadable(t *testing.T) {
	err := admissionRefusal(opstore.RefusalStoreFull)
	st := status.Convert(err)
	if st.Code() != codes.ResourceExhausted || !strings.Contains(st.Message(), "re-keying the Host") {
		t.Fatalf("%v", err)
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}
	if info == nil || info.GetReason() != ReasonStoreFull || info.GetDomain() != ErrorInfoDomain {
		t.Fatalf("ErrorInfo = %+v", info)
	}
}
