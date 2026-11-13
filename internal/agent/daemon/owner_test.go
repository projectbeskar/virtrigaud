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

	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// TestStoreOwnerMismatch: each check on its own. With an active enrolment its
// host decides alone; with none, the store marker's host and the newest held
// enrolment record's host each decide; a store that names no host is adopted.
func TestStoreOwnerMismatch(t *testing.T) {
	active := func(host string) *opstore.ActiveEnrolment { return &opstore.ActiveEnrolment{HostID: host} }
	for _, tc := range []struct {
		name                      string
		active                    *opstore.ActiveEnrolment
		marker, latest, certsHost string
		want                      bool
	}{
		{"active enrolment of another host", active("a"), "", "", "b", true},
		{"active enrolment of this host", active("a"), "", "", "a", false},
		{"active enrolment decides alone: a stale marker is not consulted", active("a"), "b", "b", "a", false},
		{"no active: the marker names another host", nil, "a", "", "b", true},
		{"no active: the newest record names another host", nil, "", "a", "b", true},
		{"no active: marker and record name this host", nil, "a", "a", "a", false},
		{"no active: marker is this host's, the record another's", nil, "b", "a", "b", true},
		{"no active: the record is this host's, the marker another's", nil, "a", "b", "b", true},
		{"no active: the store names no host (adoptable by any host)", nil, "", "", "b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemon.StoreOwnerMismatch(tc.active, tc.marker, tc.latest, tc.certsHost); got != tc.want {
				t.Fatalf("mismatch = %v, want %v", got, tc.want)
			}
		})
	}
}
