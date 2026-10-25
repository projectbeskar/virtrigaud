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

package proxmox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestParseClassSizing reads both encodings of a clone's class: the v1beta1
// VMClassSpec the VMClone controller sends (`cpu`, `memory` quantity) and a
// contracts.VMClass ("CPU", "MemoryMiB").
func TestParseClassSizing(t *testing.T) {
	cases := []struct {
		name   string
		json   string
		cpus   int
		memMiB int64
	}{
		{name: "VMClassSpec", json: `{"cpu":4,"memory":"8Gi"}`, cpus: 4, memMiB: 8192},
		{name: "VMClassSpec decimal quantity", json: `{"cpu":2,"memory":"2048Mi"}`, cpus: 2, memMiB: 2048},
		{name: "contracts.VMClass", json: `{"CPU":4,"MemoryMiB":4096}`, cpus: 4, memMiB: 4096},
		{name: "cpu only", json: `{"cpu":3}`, cpus: 3, memMiB: 0},
		{name: "empty", json: ``},
		{name: "unparseable", json: `{"cpu":`},
		{name: "bad quantity", json: `{"cpu":2,"memory":"lots"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cpus, mem := parseClassSizing(tc.json)
			assert.Equal(t, tc.cpus, cpus)
			assert.Equal(t, tc.memMiB, mem)
		})
	}
}

// TestClone_AppliesVMClassSpecMemory: a VMClone's class override, as the
// controller marshals it (a v1beta1 VMClassSpec), sizes the clone's CPU AND
// memory. Before the fix only the CPU was applied: the memory quantity under
// "memory" was never read.
func TestClone_AppliesVMClassSpecMemory(t *testing.T) {
	p, _ := newStampTestProvider(t)
	ctx := context.Background()
	src, err := p.Create(ctx, createReq(otherOwner(), ""))
	require.NoError(t, err)

	spec := v1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("6Gi")}
	classJSON, err := json.Marshal(spec)
	require.NoError(t, err)

	target := contracts.ObjectIdentity{UID: testOwnerUID, Namespace: testNamespace, Name: "sized-clone"}
	resp, err := p.Clone(ctx, &providerv1.CloneRequest{
		SourceVmId: src.Id, TargetName: target.Name, TargetVm: ownerProto(target),
		ClassJson: string(classJSON),
	})
	require.NoError(t, err)

	cores, memMiB := vmConfigSize(t, p, resp.TargetVmId)
	assert.Equal(t, 4, cores)
	assert.Equal(t, 6144, memMiB, "the VMClassSpec memory quantity must size the clone")
}
