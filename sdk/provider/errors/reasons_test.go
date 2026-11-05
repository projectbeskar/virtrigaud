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

package errors

import (
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestWithReason checks that the status carries the code, message and an
// ErrorInfo of the reason in VirtRigaud's domain, with its metadata.
func TestWithReason(t *testing.T) {
	err := WithReason(codes.NotFound, "no such task", ReasonUnknownToAgent, map[string]string{"host": "h1"})
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound || st.Message() != "no such task" {
		t.Fatalf("status = %v", err)
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}
	if info == nil {
		t.Fatal("no ErrorInfo detail")
	}
	if info.GetReason() != ReasonUnknownToAgent || info.GetDomain() != ErrorInfoDomain || info.GetMetadata()["host"] != "h1" {
		t.Fatalf("ErrorInfo = %v", info)
	}
}
