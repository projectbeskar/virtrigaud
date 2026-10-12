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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The default sizeLimit of a provider pod's writable /tmp emptyDir, per
// provider type. They are sized for what each provider stages in its own /tmp:
//   - vSphere stages whole disks and images there: an OVA download (capped by
//     VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB, default 256 GiB) and, for a
//     migration import or a converted/multi-extent export, about twice the
//     disk at once. 512Gi covers both up to that cap; a larger disk needs a
//     larger limit (the annotation or --provider-tmp-size-limit).
//   - libvirt and Proxmox stream disk data to and from the hypervisor host
//     over SSH; their pod /tmp only holds SSH material, known_hosts files
//     and control sockets (KB).
//
// A sizeLimit bounds the pod's /tmp; it does not reserve node storage. A node
// with less free ephemeral storage than the limit can still fill up first.
const (
	vsphereProviderTmpSizeLimit = "512Gi"
	smallProviderTmpSizeLimit   = "1Gi"
)

// eventReasonTmpSizeLimitClamped is the Warning recorded on a Provider whose
// /tmp size-limit annotation exceeds the manager's ceiling (or asks for none)
// and was clamped to the ceiling.
const eventReasonTmpSizeLimitClamped = "TmpSizeLimitClamped"

// defaultProviderTmpSizeLimit is the per-type default /tmp sizeLimit of a
// provider pod (see vsphereProviderTmpSizeLimit).
func defaultProviderTmpSizeLimit(providerType infravirtrigaudiov1beta1.ProviderType) *resource.Quantity {
	value := smallProviderTmpSizeLimit
	if providerType == infravirtrigaudiov1beta1.ProviderTypeVSphere {
		value = vsphereProviderTmpSizeLimit
	}
	limit := resource.MustParse(value)
	return &limit
}

// ParseProviderTmpSizeLimitMax parses the --provider-tmp-size-limit-max
// ceiling: empty means no ceiling (nil); otherwise a positive quantity. "none"
// is refused: a ceiling of no limit is no ceiling, so leave the flag empty.
func ParseProviderTmpSizeLimitMax(value string) (*resource.Quantity, error) {
	if value == "" {
		return nil, nil
	}
	if value == infravirtrigaudiov1beta1.ProviderTmpSizeLimitNone {
		return nil, fmt.Errorf("invalid /tmp size-limit ceiling %q: leave it empty for no ceiling", value)
	}
	return infravirtrigaudiov1beta1.ParseProviderTmpSizeLimit(value)
}

// providerTmpSizeLimit returns the sizeLimit of the provider pod's /tmp
// emptyDir, nil meaning no limit.
//
//  1. The base is the manager-wide TmpSizeLimit when set, else the per-type
//     default.
//  2. A valid ProviderTmpSizeLimitAnnotation replaces the base, within the
//     ceiling (TmpSizeLimitMax): with a ceiling set, an annotation above it,
//     or "none", is clamped to it and a TmpSizeLimitClamped Warning event is
//     recorded once per annotation value.
//  3. Without an annotation the base is capped at the ceiling.
//
// A value that does not parse (the webhook and the manager's startup refuse
// one, but the webhook may be off) is ignored and logged.
func (r *ProviderReconciler) providerTmpSizeLimit(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) *resource.Quantity {
	logger := log.FromContext(ctx)

	base := defaultProviderTmpSizeLimit(provider.Spec.Type)
	if r.TmpSizeLimit != "" {
		limit, err := infravirtrigaudiov1beta1.ParseProviderTmpSizeLimit(r.TmpSizeLimit)
		if err == nil {
			base = limit
		} else {
			logger.Info("Ignoring an invalid manager-wide /tmp size limit", "error", err.Error())
		}
	}
	ceiling, err := ParseProviderTmpSizeLimitMax(r.TmpSizeLimitMax)
	if err != nil {
		logger.Info("Ignoring an invalid /tmp size-limit ceiling", "error", err.Error())
		ceiling = nil
	}

	key := client.ObjectKeyFromObject(provider).String()
	value, annotated := provider.Annotations[infravirtrigaudiov1beta1.ProviderTmpSizeLimitAnnotation]
	if annotated {
		limit, err := infravirtrigaudiov1beta1.ParseProviderTmpSizeLimit(value)
		switch {
		case err != nil:
			logger.Info("Ignoring an invalid /tmp size-limit annotation",
				"provider", provider.Name, "annotation", infravirtrigaudiov1beta1.ProviderTmpSizeLimitAnnotation, "error", err.Error())
		case ceiling == nil:
			r.tmpLimitWarned.Delete(key)
			return limit
		case limit == nil || limit.Cmp(*ceiling) > 0:
			r.warnTmpLimitClamped(provider, key, value, ceiling)
			capped := ceiling.DeepCopy()
			return &capped
		default:
			r.tmpLimitWarned.Delete(key)
			return limit
		}
	}
	r.tmpLimitWarned.Delete(key)
	if ceiling != nil && (base == nil || base.Cmp(*ceiling) > 0) {
		capped := ceiling.DeepCopy()
		return &capped
	}
	return base
}

// warnTmpLimitClamped records the TmpSizeLimitClamped Warning on provider,
// once per annotation value.
func (r *ProviderReconciler) warnTmpLimitClamped(provider *infravirtrigaudiov1beta1.Provider, key, value string, ceiling *resource.Quantity) {
	if prev, ok := r.tmpLimitWarned.Load(key); ok && prev == value {
		return
	}
	r.tmpLimitWarned.Store(key, value)
	r.recordProviderEvent(provider, corev1.EventTypeWarning, eventReasonTmpSizeLimitClamped,
		fmt.Sprintf("%s=%q exceeds the manager's /tmp size-limit ceiling (--provider-tmp-size-limit-max=%s); "+
			"the provider pods' /tmp is limited to %s. Only a cluster administrator can raise the ceiling.",
			infravirtrigaudiov1beta1.ProviderTmpSizeLimitAnnotation, value, ceiling.String(), ceiling.String()))
}
