//go:build !virtrigaud_dev

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
)

// A release build refuses every host inventory flag, the insecure one
// included, and runs the default policy without any.
func TestParseHostInventoryPolicy_ReleaseBuild(t *testing.T) {
	for name, flags := range map[string]HostInventoryFlags{
		"insecure":   {InsecureSkipAllowList: true},
		"allow-list": {AllowListFile: "/x", AllowListSHA256: "00"},
		"max age":    {MaxAge: "10m"},
		"features":   {RequiredFeatures: "kvm"},
		"origins":    {AcceptedOrigins: "read-only"},
		"tiers":      {AcceptedTiers: "pidfd-store"},
		"signers":    {SignersFile: "/s"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHostInventoryPolicy(flags)
			require.ErrorIs(t, err, devgate.ErrNotCompiled)
		})
	}
	p, err := ParseHostInventoryPolicy(HostInventoryFlags{})
	require.NoError(t, err)
	assert.Nil(t, p.AllowList)
	assert.False(t, p.SkipAllowList)
	assert.Equal(t, DefaultHostInventoryMaxAge, p.MaxAge)
}
