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

package capabilities

import (
	"context"
	"testing"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestHonestReconfigure_CapabilityAndResponse pins the SDK side of the honest
// Reconfigure result: the capability reaches GetCapabilitiesResponse, and
// HonestReconfigureResponse marks every answer (honest_result) with
// restart_required and an optional task.
func TestHonestReconfigure_CapabilityAndResponse(t *testing.T) {
	caps, err := NewBuilder().Core().HonestReconfigure().Build().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !caps.GetSupportsHonestReconfigure() {
		t.Error("HonestReconfigure() must advertise supports_honest_reconfigure")
	}
	plain, err := NewBuilder().Core().Build().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.GetSupportsHonestReconfigure() {
		t.Error("not advertised unless asked for")
	}

	resp := HonestReconfigureResponse("", true)
	if !resp.GetHonestResult() || !resp.GetRestartRequired() || resp.GetTask() != nil {
		t.Errorf("synchronous restart-required answer: %+v", resp)
	}
	resp = HonestReconfigureResponse("task-1", false)
	if !resp.GetHonestResult() || resp.GetRestartRequired() || resp.GetTask().GetId() != "task-1" {
		t.Errorf("asynchronous answer: %+v", resp)
	}
}
