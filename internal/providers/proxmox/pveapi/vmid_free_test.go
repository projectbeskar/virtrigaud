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

package pveapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVMIDFree pins the one VMID probe (GET /cluster/nextid?vmid=): 200 is
// free, 400 "already exists" is in use, and anything else is an error —
// never read as free or in use.
func TestVMIDFree(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantFree bool
		wantErr  bool
	}{
		{"free", http.StatusOK, `{"data":"700"}`, true, false},
		{"in use", http.StatusBadRequest, `{"errors":{"vmid":"VM 700 already exists"}}`, false, false},
		{"another 400", http.StatusBadRequest, `{"errors":{"vmid":"value must be a valid id"}}`, false, true},
		{"server error", http.StatusInternalServerError, `{"data":null}`, false, true},
		{"forbidden", http.StatusForbidden, `{"data":null}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api2/json/cluster/nextid" || r.URL.Query().Get("vmid") != "700" {
					t.Errorf("unexpected request %s", r.URL.String())
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, err := NewClient(&Config{Endpoint: srv.URL, TokenID: "user@pve!t", TokenSecret: "s"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			free, err := c.VMIDFree(context.Background(), 700)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if free != tc.wantFree {
				t.Fatalf("free = %v, want %v", free, tc.wantFree)
			}
		})
	}
}
