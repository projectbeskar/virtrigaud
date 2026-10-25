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

package v1beta1

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The VMClone customization webhook never refuses anything: it returns one
// admission warning naming the fields of spec.customization that may hold a
// secret value this release does not act on (VMCustomization.UnusedSecretFields)
// on a create, and on an update that changes spec.customization (not on the
// manager's own finalizer and metadata writes, nor once the VMClone is being
// deleted). Such a value is stored in plaintext in the VMClone, and the
// manager does not send it to the provider. Because it only warns, its failurePolicy is Ignore
// (a manager that cannot be reached delays a VMClone write by at most the
// timeout, never refuses it). The manager serves it whenever webhooks are on;
// the chart renders its configuration with the other validating webhooks.

// +kubebuilder:webhook:path=/validate-infra-virtrigaud-io-v1beta1-vmclone,mutating=false,failurePolicy=ignore,timeoutSeconds=5,sideEffects=None,groups=infra.virtrigaud.io,resources=vmclones,verbs=create;update,versions=v1beta1,name=vvmclone-customization.kb.io,admissionReviewVersions=v1

// unusedSecretFieldsWarningFmt is the admission warning of the fields of
// spec.customization that may hold a secret value this release does not act
// on: their paths (customizationWarnings).
const unusedSecretFieldsWarningFmt = "%s: accepted but not acted on in this release; stored in plaintext in " +
	"the object — do not put a real password, key or other credential here"

// maxWarnedPaths is the most field paths the warning names; the rest are
// counted ("+N more"), so the warning stays well within the API server's
// warning size budget.
const maxWarnedPaths = 8

// VMCloneCustomizationValidator warns, never refuses, when a VMClone's
// spec.customization holds a secret value this release does not act on.
// +kubebuilder:object:generate=false
type VMCloneCustomizationValidator struct{}

var _ webhook.CustomValidator = &VMCloneCustomizationValidator{}

// SetupVMCloneWebhookWithManager registers the VMClone customization
// validating webhook. Callers register it whenever webhook serving is
// configured (see cmd/manager/main.go).
func SetupVMCloneWebhookWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&VMClone{}).
		WithValidator(&VMCloneCustomizationValidator{}).
		Complete(); err != nil {
		return fmt.Errorf("register the VMClone customization webhook: %w", err)
	}
	return nil
}

// ValidateCreate admits the VMClone, with a warning naming the unused secret
// fields of its spec.customization.
func (v *VMCloneCustomizationValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	clone, ok := obj.(*VMClone)
	if !ok {
		return nil, fmt.Errorf("expected a VMClone object but got %T", obj)
	}
	return customizationWarnings(clone), nil
}

// ValidateUpdate admits the update, with a warning naming the unused secret
// fields of the new object's spec.customization when the update changes
// spec.customization. An update that leaves it as it was (the manager's
// finalizer and metadata writes) and an update of a VMClone being deleted get
// no warning.
func (v *VMCloneCustomizationValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	clone, ok := newObj.(*VMClone)
	if !ok {
		return nil, fmt.Errorf("expected a VMClone object but got %T", newObj)
	}
	old, ok := oldObj.(*VMClone)
	if !ok {
		return nil, fmt.Errorf("expected a VMClone object but got %T", oldObj)
	}
	if clone.DeletionTimestamp != nil || equality.Semantic.DeepEqual(old.Spec.Customization, clone.Spec.Customization) {
		return nil, nil
	}
	return customizationWarnings(clone), nil
}

// ValidateDelete always admits.
func (v *VMCloneCustomizationValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// customizationWarnings returns one warning naming the unused secret fields of
// clone's spec.customization — at most maxWarnedPaths of them, the rest
// counted — or none when it has none.
func customizationWarnings(clone *VMClone) admission.Warnings {
	fields := clone.Spec.Customization.UnusedSecretFields()
	if len(fields) == 0 {
		return nil
	}
	named := strings.Join(fields[:min(len(fields), maxWarnedPaths)], ", ")
	if extra := len(fields) - maxWarnedPaths; extra > 0 {
		named += fmt.Sprintf(" (+%d more)", extra)
	}
	return admission.Warnings{fmt.Sprintf(unusedSecretFieldsWarningFmt, named)}
}

// UnusedSecretFields returns the paths (from "spec.customization") of the
// fields of c, when set, that hold — or commonly hold — a secret value this
// release does not act on: sysprep.productKey, sysprep.adminPassword.value,
// sysprep.joinDomain.password.value, sysprep.customCommands,
// certificates[i].data, guestCommands[i].command and .arguments, and
// userData.ignition.inline. Such a value is stored in plaintext in the object;
// the manager never sends it to a provider. It returns paths only, never
// values; nil for a nil c.
func (c *VMCustomization) UnusedSecretFields() []string {
	if c == nil {
		return nil
	}
	const prefix = "spec.customization."
	var out []string
	if s := c.Sysprep; s != nil {
		if s.ProductKey != "" {
			out = append(out, prefix+"sysprep.productKey")
		}
		if s.AdminPassword != nil && s.AdminPassword.Value != "" {
			out = append(out, prefix+"sysprep.adminPassword.value")
		}
		if s.JoinDomain != nil && s.JoinDomain.Password.Value != "" {
			out = append(out, prefix+"sysprep.joinDomain.password.value")
		}
		if len(s.CustomCommands) > 0 {
			out = append(out, prefix+"sysprep.customCommands")
		}
	}
	for i, cert := range c.Certificates {
		if cert.Data != "" {
			out = append(out, fmt.Sprintf("%scertificates[%d].data", prefix, i))
		}
	}
	for i, cmd := range c.GuestCommands {
		if cmd.Command != "" {
			out = append(out, fmt.Sprintf("%sguestCommands[%d].command", prefix, i))
		}
		if len(cmd.Arguments) > 0 {
			out = append(out, fmt.Sprintf("%sguestCommands[%d].arguments", prefix, i))
		}
	}
	if c.UserData != nil && c.UserData.Ignition != nil && c.UserData.Ignition.Inline != "" {
		out = append(out, prefix+"userData.ignition.inline")
	}
	return out
}
