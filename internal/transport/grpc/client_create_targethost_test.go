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

package grpc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestConvertCreateRequest_ThreadsTargetHostID pins the manager -> proto mapping
// for the ADR-0007 P1 clustered create binding: contracts.CreateRequest.TargetHostID
// must be carried on the wire as CreateRequest.target_host_id, as an EXPLICIT
// field separate from placement_json (which stays for external-orchestrator
// hints). It is passed through verbatim; empty stays empty so single-host
// providers never receive it.
func TestConvertCreateRequest_ThreadsTargetHostID(t *testing.T) {
	c := &Client{}

	t.Run("set", func(t *testing.T) {
		got, err := c.convertCreateRequest(contracts.CreateRequest{
			Name:         "vm1",
			TargetHostID: "host-b",
			Placement:    &contracts.Placement{Cluster: "ignored-by-target"},
		})
		require.NoError(t, err)
		require.Equal(t, "host-b", got.TargetHostId, "TargetHostID must map to CreateRequest.target_host_id")
		// The explicit binding must not leak into placement_json.
		require.NotContains(t, got.PlacementJson, "host-b")
	})

	t.Run("empty stays empty", func(t *testing.T) {
		got, err := c.convertCreateRequest(contracts.CreateRequest{Name: "vm2"})
		require.NoError(t, err)
		require.Empty(t, got.TargetHostId, "an unset TargetHostID must not populate target_host_id")
	})
}
