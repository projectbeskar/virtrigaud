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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// ssarAnswer answers one SelfSubjectAccessReview in a fake client.
type ssarAnswer func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error)

// ssarClient returns a client whose SelfSubjectAccessReview creates are
// answered by answer, and the list of reviews it was asked, as
// "namespace:verb:resource[:name]".
func ssarClient(t *testing.T, answer ssarAnswer) (client.Client, *[]string) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, authorizationv1.AddToScheme(s))
	var asked []string
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			r, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			require.True(t, ok, "only SelfSubjectAccessReviews are created")
			require.NotNil(t, r.Spec.ResourceAttributes)
			a := *r.Spec.ResourceAttributes
			key := a.Namespace + ":" + a.Verb + ":" + a.Resource
			if a.Name != "" {
				key += ":" + a.Name
			}
			asked = append(asked, key)
			st, err := answer(a)
			r.Status = st
			return err
		},
	}).Build()
	return c, &asked
}

// denyAll denies every review.
func denyAll(authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
	return authorizationv1.SubjectAccessReviewStatus{}, nil
}

func TestCheckHostEnrolmentSecretAccess(t *testing.T) {
	ctx := context.Background()
	key := &types.NamespacedName{Namespace: "cert-manager", Name: "host-agent-ca"}
	withKey := &HostEnrolmentConfig{Namespace: "host-agent-pki", CAKeySecret: key}
	withoutKey := &HostEnrolmentConfig{Namespace: "host-agent-pki"}
	clusterWide := []string{":get:secrets", ":list:secrets", ":watch:secrets"}
	keyReviews := []string{"cert-manager:get:secrets:host-agent-ca", "cert-manager:list:secrets", "cert-manager:watch:secrets"}

	t.Run("enrolment disabled: no review", func(t *testing.T) {
		c, asked := ssarClient(t, denyAll)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, nil))
		assert.Empty(t, *asked)
	})

	t.Run("everything denied passes", func(t *testing.T) {
		c, asked := ssarClient(t, denyAll)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withoutKey))
		assert.Equal(t, clusterWide, *asked, "Secret reads in all namespaces are reviewed with namespace \"\"")

		c, asked = ssarClient(t, denyAll)
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withKey))
		assert.Equal(t, append(append([]string{}, clusterWide...), keyReviews...), *asked)
	})

	t.Run("enrolment-namespace Secret access is not a refusal", func(t *testing.T) {
		// The pins Secrets: the enrolment Role grants the manager full
		// Secret access in the enrolment namespace, which is intended.
		c, _ := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
			return authorizationv1.SubjectAccessReviewStatus{Allowed: a.Namespace == "host-agent-pki"}, nil
		})
		require.NoError(t, CheckHostEnrolmentSecretAccess(ctx, c, withKey))
	})

	for name, tc := range map[string]struct {
		cfg   *HostEnrolmentConfig
		allow func(a authorizationv1.ResourceAttributes) bool
		want  string
	}{
		"cluster-wide get": {withoutKey, func(a authorizationv1.ResourceAttributes) bool {
			return a.Namespace == "" && a.Verb == "get"
		}, "get Secrets in all namespaces"},
		"cluster-wide list": {withKey, func(a authorizationv1.ResourceAttributes) bool {
			return a.Namespace == "" && a.Verb == "list"
		}, "list Secrets in all namespaces"},
		"cluster-wide watch": {withoutKey, func(a authorizationv1.ResourceAttributes) bool {
			return a.Namespace == "" && a.Verb == "watch"
		}, "watch Secrets in all namespaces"},
		"the exact sub-CA key Secret": {withKey, func(a authorizationv1.ResourceAttributes) bool {
			return a.Namespace == key.Namespace && a.Name == key.Name && a.Verb == "get"
		}, "get the sub-CA key Secret cert-manager/host-agent-ca"},
		"list in the sub-CA key namespace": {withKey, func(a authorizationv1.ResourceAttributes) bool {
			return a.Namespace == key.Namespace && a.Name == "" && a.Verb == "list"
		}, "list Secrets in the sub-CA key namespace cert-manager"},
		"everything (the default cluster grant)": {withKey, func(authorizationv1.ResourceAttributes) bool { return true },
			"get Secrets in all namespaces, list Secrets in all namespaces, watch Secrets in all namespaces, get the sub-CA key Secret"},
	} {
		t.Run("refuses: "+name, func(t *testing.T) {
			c, asked := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
				return authorizationv1.SubjectAccessReviewStatus{Allowed: tc.allow(a)}, nil
			})
			err := CheckHostEnrolmentSecretAccess(ctx, c, tc.cfg)
			require.ErrorIs(t, err, ErrHostEnrolmentSecretAccess)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "rbac.secrets.scope=namespaced", "the refusal says how to fix it")
			want := len(clusterWide)
			if tc.cfg.CAKeySecret != nil {
				want += len(keyReviews)
			}
			assert.Len(t, *asked, want, "every review is asked, so the refusal lists every allowed access")
		})
	}

	t.Run("a review error refuses", func(t *testing.T) {
		boom := errors.New("connection refused")
		for _, failOn := range append(append([]string{}, clusterWide...), keyReviews...) {
			t.Run(failOn, func(t *testing.T) {
				c, _ := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
					k := a.Namespace + ":" + a.Verb + ":" + a.Resource
					if a.Name != "" {
						k += ":" + a.Name
					}
					if k == failOn {
						return authorizationv1.SubjectAccessReviewStatus{}, boom
					}
					return authorizationv1.SubjectAccessReviewStatus{}, nil
				})
				err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
				require.ErrorIs(t, err, boom)
				assert.Contains(t, err.Error(), "check whether the manager may")
			})
		}
	})

	t.Run("an evaluation error refuses", func(t *testing.T) {
		c, _ := ssarClient(t, func(a authorizationv1.ResourceAttributes) (authorizationv1.SubjectAccessReviewStatus, error) {
			if a.Namespace == key.Namespace && a.Name == key.Name {
				return authorizationv1.SubjectAccessReviewStatus{EvaluationError: "webhook authorizer unavailable"}, nil
			}
			return authorizationv1.SubjectAccessReviewStatus{}, nil
		})
		err := CheckHostEnrolmentSecretAccess(ctx, c, withKey)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not evaluate it: webhook authorizer unavailable")
		assert.Contains(t, err.Error(), "get the sub-CA key Secret cert-manager/host-agent-ca")
	})
}

func TestParseHostEnrolmentConfig_CAKeySecret(t *testing.T) {
	ca := hostidentitytest.NewCA(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, ca.PEM, 0o600))
	base := HostEnrolmentFlags{
		Namespace: "host-agent-pki", Issuer: "ClusterIssuer/host-agent-ca", AdminGroup: "enrolment-admins",
		TrustDomain: "cluster.test", CABundleFile: caFile,
	}
	installID := bytes.Repeat([]byte{1}, 16)

	if !devgate.Compiled {
		// The flag is not registered in a release build; the field alone
		// still counts as an enrolment flag and is refused.
		_, err := ParseHostEnrolmentConfig(HostEnrolmentFlags{CAKeySecret: "cert-manager/host-agent-ca"}, installID)
		require.ErrorIs(t, err, devgate.ErrNotCompiled)
		return
	}

	cfg, err := ParseHostEnrolmentConfig(base, installID)
	require.NoError(t, err)
	assert.Nil(t, cfg.CAKeySecret, "optional")

	f := base
	f.CAKeySecret = "cert-manager/host-agent.ca-key"
	cfg, err = ParseHostEnrolmentConfig(f, installID)
	require.NoError(t, err)
	assert.Equal(t, &types.NamespacedName{Namespace: "cert-manager", Name: "host-agent.ca-key"}, cfg.CAKeySecret)

	_, err = ParseHostEnrolmentConfig(HostEnrolmentFlags{CAKeySecret: "cert-manager/host-agent-ca"}, installID)
	require.Error(t, err, "the flag alone does not enable enrolment")
	assert.Contains(t, err.Error(), "--"+DevHostEnrolmentNamespaceFlagName)

	for value, want := range map[string]string{
		"host-agent-ca":                 "must be <namespace>/<name>",
		"cert-manager/a/b":              "must be <namespace>/<name>",
		"/host-agent-ca":                `namespace ""`,
		"cert-manager/":                 `the Secret name ""`,
		"Cert_Manager/host-agent-ca":    `namespace "Cert_Manager"`,
		"cert-manager/Host_Agent_CA":    `the Secret name "Host_Agent_CA"`,
		"host-agent-pki/host-agent-ca":  "must not be in the enrolment namespace host-agent-pki",
		"cert-manager.io/host-agent-ca": `namespace "cert-manager.io"`,
	} {
		t.Run(value, func(t *testing.T) {
			f := base
			f.CAKeySecret = value
			_, err := ParseHostEnrolmentConfig(f, installID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--"+DevHostEnrolmentCAKeySecretFlagName)
			assert.Contains(t, err.Error(), want)
		})
	}
}
