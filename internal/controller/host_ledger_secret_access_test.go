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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/types"
)

// With host ledger keys, enrolment refuses while the manager may get, list
// or watch the key Secret or any Secret of its namespace through the API,
// whether by a review or by a rule limited to named objects.
func TestCheckHostEnrolmentSecretAccess_LedgerKey(t *testing.T) {
	ctx := context.Background()
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", LedgerKeySecret: &types.NamespacedName{Namespace: "virtrigaud-system",
		Name: "host-ledger-key"}}
	c, asked := ssarClient(t, denyAll)
	require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, cfg))
	for _, v := range []string{"get", "list", "watch"} {
		assert.Contains(t, *asked, "virtrigaud-system:"+v+":/secrets:host-ledger-key")
		assert.Contains(t, *asked, "virtrigaud-system:"+v+":/secrets")
	}
	for _, key := range []string{"virtrigaud-system:get:/secrets:host-ledger-key", "virtrigaud-system:list:/secrets"} {
		c, _ := ssarClient(t, allowOnly(key))
		err := CheckHostEnrolmentSecretAccess(ctx, c, cfg)
		require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess, key)
		assert.Contains(t, err.Error(), "host ledger key")
	}
	named, _ := rulesClient(t, denyAll, authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{{
		APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}, ResourceNames: []string{"host-ledger-key"}}}})
	err := CheckHostEnrolmentSecretAccess(ctx, named, cfg)
	require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess, "a rule limited to the key Secret's name refuses too")
	assert.Contains(t, err.Error(), "in namespace virtrigaud-system")
}
