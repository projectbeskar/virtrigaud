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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

const cloudInitBoundary = "VIRTRIGAUD_CLOUD_INIT_BOUNDARY"

// extractCloudInitFromSecret extracts cloud-init user data from a Secret.
// Accepted keys: userdata, user-data, cloud-init, cloud-config.
func extractCloudInitFromSecret(s *corev1.Secret) (string, error) {
	acceptedKeys := []string{"userdata", "user-data", "cloud-init", "cloud-config"}
	for _, key := range acceptedKeys {
		if val, ok := s.Data[key]; ok {
			return string(val), nil
		}
	}
	return "", fmt.Errorf("secret %q contains no recognised cloud-init key; accepted keys: %v", s.Name, acceptedKeys)
}

// extractMetaDataFromSecret extracts cloud-init metadata from a Secret.
// Accepted keys: metadata, meta-data, meta_data.
func extractMetaDataFromSecret(s *corev1.Secret) (string, error) {
	acceptedKeys := []string{"metadata", "meta-data", "meta_data"}
	for _, key := range acceptedKeys {
		if val, ok := s.Data[key]; ok {
			return string(val), nil
		}
	}
	return "", fmt.Errorf("secret %q contains no recognised metadata key; accepted keys: %v", s.Name, acceptedKeys)
}

// mergeCloudConfigParts combines multiple cloud-config documents into a
// MIME multipart/mixed document so cloud-init processes them all.
func mergeCloudConfigParts(parts []string) string {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + cloudInitBoundary + "\"\n")
	b.WriteString("MIME-Version: 1.0\n")
	for _, part := range parts {
		b.WriteString("\n--" + cloudInitBoundary + "\n")
		b.WriteString("Content-Type: text/cloud-config; charset=\"utf-8\"\n")
		b.WriteString("\n")
		b.WriteString(part)
		b.WriteString("\n")
	}
	b.WriteString("\n--" + cloudInitBoundary + "--\n")
	return b.String()
}

// resolveCloudInitUserData resolves cloud-init user data from inline content,
// a Secret reference, or both (merged as MIME multipart when both are set).
func (r *VirtualMachineReconciler) resolveCloudInitUserData(ctx context.Context, namespace string, ci *infravirtrigaudiov1beta1.CloudInit) (string, error) {
	var parts []string

	if ci.Inline != "" {
		parts = append(parts, ci.Inline)
	}

	if ci.SecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: ci.SecretRef.Name, Namespace: namespace}, secret); err != nil {
			return "", fmt.Errorf("fetching cloud-init secret %q: %w", ci.SecretRef.Name, err)
		}
		data, err := extractCloudInitFromSecret(secret)
		if err != nil {
			return "", err
		}
		parts = append(parts, data)
	}

	switch len(parts) {
	case 0:
		return "", nil
	case 1:
		return parts[0], nil
	default:
		return mergeCloudConfigParts(parts), nil
	}
}

// resolveCloudInitMetaData resolves cloud-init metadata from inline content,
// a Secret reference, or both (concatenated with a newline when both are set).
func (r *VirtualMachineReconciler) resolveCloudInitMetaData(ctx context.Context, namespace string, meta *infravirtrigaudiov1beta1.CloudInitMetaData) (string, error) {
	var parts []string

	if meta.Inline != "" {
		parts = append(parts, meta.Inline)
	}

	if meta.SecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: meta.SecretRef.Name, Namespace: namespace}, secret); err != nil {
			return "", fmt.Errorf("fetching metadata secret %q: %w", meta.SecretRef.Name, err)
		}
		data, err := extractMetaDataFromSecret(secret)
		if err != nil {
			return "", err
		}
		parts = append(parts, data)
	}

	return strings.Join(parts, "\n"), nil
}
