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

package grantverify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestReasons_EqualTheContractValues: grantverify cannot import contracts, so
// its reason strings are copies; they must stay equal to the contract's.
func TestReasons_EqualTheContractValues(t *testing.T) {
	assert.Equal(t, contracts.GrantUnauthenticatedReason, grantverify.ReasonGrantUnauthenticated)
	assert.Equal(t, contracts.GrantMismatchReason, grantverify.ReasonGrantMismatch)
	assert.Equal(t, contracts.IdentityRequiredReason, grantverify.ReasonIdentityRequired)
}
