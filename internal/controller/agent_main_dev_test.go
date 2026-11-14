//go:build virtrigaud_dev

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
	"os"
	"testing"

	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/agenttarget"
)

// TestMain runs the test binary as a per-host agent process when the agent
// tests (virtualmachine_operation_agent_test.go) re-execute it as one.
func TestMain(m *testing.M) {
	if agenttarget.RunChildIfRequested() {
		return
	}
	os.Exit(m.Run())
}
