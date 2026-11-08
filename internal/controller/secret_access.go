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
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The manager's Secret access (docs/manager-secret-access.md) falls in two
// groups:
//
//   - Provider namespaces: the Provider's TLS Secret, the clustered host
//     credential Secrets, the host-inventory Secret (<provider>-hosts) and the
//     operation-intent Secrets (vrtg-op-*). These are read through the
//     informer cache (the TLS Secret is read on every Provider resolve) and,
//     for the last two, written.
//   - Workload namespaces: the cloud-init user-data and metadata Secrets of a
//     VirtualMachine and the S3 credentials Secret of a VMMigration. These
//     are read live (APIReader), get only, so a namespace that holds only
//     workloads needs no list, watch or write on Secrets.
//
// By default the Secret informer covers every namespace, which needs a
// cluster-wide list/watch grant. SecretCacheNamespacesFlagName restricts it to
// the Provider namespaces, so the manager can run with per-namespace Roles
// and no cluster-wide Secret grant (Helm rbac.secrets.scope=namespaced).

// SecretCacheNamespacesFlagName is the manager flag that restricts the Secret
// informer cache to a comma-separated list of namespaces.
const SecretCacheNamespacesFlagName = "secret-cache-namespaces"

// ParseSecretCacheNamespaces parses the value of SecretCacheNamespacesFlagName:
// comma-separated namespace names, sorted and without duplicates. An empty
// value returns nil (the Secret informer covers every namespace). A name that
// is not a valid namespace name is an error.
func ParseSecretCacheNamespaces(value string) ([]string, error) {
	var out []string
	for _, ns := range strings.Split(value, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("--%s: %q is not a namespace name: %s", SecretCacheNamespacesFlagName, ns, strings.Join(errs, "; "))
		}
		out = append(out, ns)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// SecretCacheByObject returns the cache options that restrict the Secret
// informer to namespaces, or nil (every namespace) when namespaces is empty.
// A cached read of a Secret outside namespaces fails rather than falling back
// to a cluster-wide list.
func SecretCacheByObject(namespaces []string) map[client.Object]cache.ByObject {
	if len(namespaces) == 0 {
		return nil
	}
	nsConfig := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		nsConfig[ns] = cache.Config{}
	}
	return map[client.Object]cache.ByObject{&corev1.Secret{}: {Namespaces: nsConfig}}
}
