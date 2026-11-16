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

package daemon_test

import (
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
)

// TestStartUpProgressStatus: a development build names the start-up step
// in the service status while the store opens and recovers; a release build
// sends nothing new.
func TestStartUpProgressStatus(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"STATUS=opening the operation store", "STATUS=establishing continuity"} {
		if got := h.notes.has(s); got != config.DevBuild {
			t.Errorf("%q sent = %v in a build with DevBuild = %v: %v", s, got, config.DevBuild, h.notes.snapshot())
		}
	}
}
