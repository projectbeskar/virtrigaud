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
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// What a Clone request carries of spec.customization (CustomizeJSON).
//
// The providers read the customization through cloneidentity.ParseCustomization
// (internal/providers/cloneidentity): libvirt (clone_seed.go) and vSphere
// (clone_identity.go) apply hostname, domain and — libvirt only —
// userData.cloudInit.inline; they read userData.cloudInit.secretRef and
// userData.ignition only for whether they are set, and every other top-level
// key only for whether it is set, to report it as not applied. Proxmox and mock
// do not read it.
//
// So the request carries an ALLOW-LIST of spec.customization, built field by
// field (providerCustomization), never the object as written: the applied
// fields as they are, the non-secret fields providers only report, and of the
// fields that may hold a secret only their non-secret parts, so a provider
// still sees (and reports) that sysprep, certificates or guest commands were
// asked for. Left out — nothing reads them, and they may hold a credential:
// sysprep.productKey, sysprep.adminPassword.value, sysprep.joinDomain.password.value,
// sysprep.customCommands, certificates[].data, guestCommands[].command and
// .arguments, and userData.ignition.inline. A field added to the API is not
// sent until it is added here (TestProviderCustomization_EveryFieldClassified
// fails until it is classified).

// customizationNotSentReason is the reason of the Warning event of a VMClone
// whose spec.customization holds a secret value that is not acted on.
const customizationNotSentReason = "CustomizationSecretNotSent"

// customizationNotSentMessageFmt is that event's message: the field paths
// (never their values).
const customizationNotSentMessageFmt = "%s: accepted but not acted on in this release and not sent to the provider; " +
	"it is stored in plaintext in the VMClone — do not put a real password, product key or certificate key here"

// providerCustomization returns the part of c a Clone request carries (see the
// file comment). It never returns a value that shares memory with c.
func providerCustomization(c *infrav1beta1.VMCustomization) *infrav1beta1.VMCustomization {
	if c == nil {
		return nil
	}
	out := &infrav1beta1.VMCustomization{
		Hostname: c.Hostname,
		Domain:   c.Domain,
		TimeZone: c.TimeZone,
		Tags:     append([]string(nil), c.Tags...),
	}
	for i := range c.Networks {
		out.Networks = append(out.Networks, *c.Networks[i].DeepCopy())
	}
	if c.UserData != nil {
		out.UserData = &infrav1beta1.UserData{}
		if ci := c.UserData.CloudInit; ci != nil {
			out.UserData.CloudInit = &infrav1beta1.CloudInit{Inline: ci.Inline, SecretRef: ci.SecretRef.DeepCopy()}
		}
		if ign := c.UserData.Ignition; ign != nil {
			out.UserData.Ignition = &infrav1beta1.Ignition{SecretRef: ign.SecretRef.DeepCopy()}
		}
	}
	if s := c.Sysprep; s != nil {
		out.Sysprep = &infrav1beta1.SysprepCustomization{
			Enabled:       s.Enabled,
			Organization:  s.Organization,
			Owner:         s.Owner,
			AdminPassword: passwordReference(s.AdminPassword),
		}
		if j := s.JoinDomain; j != nil {
			out.Sysprep.JoinDomain = &infrav1beta1.DomainJoinSpec{
				Domain:             j.Domain,
				Username:           j.Username,
				OrganizationalUnit: j.OrganizationalUnit,
				Password:           *passwordReference(&j.Password),
			}
		}
	}
	for _, cert := range c.Certificates {
		out.Certificates = append(out.Certificates, infrav1beta1.CertificateSpec{
			Name:      cert.Name,
			SecretRef: cert.SecretRef.DeepCopy(),
			SecretKey: cert.SecretKey,
			Store:     cert.Store,
		})
	}
	for _, cmd := range c.GuestCommands {
		gc := infrav1beta1.GuestCommand{
			WorkingDirectory: cmd.WorkingDirectory,
			RunAsUser:        cmd.RunAsUser,
			Stage:            cmd.Stage,
		}
		if cmd.Timeout != nil {
			t := *cmd.Timeout
			gc.Timeout = &t
		}
		out.GuestCommands = append(out.GuestCommands, gc)
	}
	return out
}

// passwordReference returns the Secret reference of p without its plaintext
// value (nil for nil).
func passwordReference(p *infrav1beta1.PasswordSpec) *infrav1beta1.PasswordSpec {
	if p == nil {
		return nil
	}
	return &infrav1beta1.PasswordSpec{SecretRef: p.SecretRef.DeepCopy(), SecretKey: p.SecretKey}
}

// customizeJSON returns the CustomizeJSON of a Clone request for clone: the
// allow-listed customization (providerCustomization), or "" when the clone
// has none or it cannot be marshalled. When spec.customization holds a secret
// value that is not acted on (VMCustomization.UnusedSecretFields), it records a
// Warning event naming the fields — never their values.
func (r *VMCloneReconciler) customizeJSON(_ context.Context, clone *infrav1beta1.VMClone) string {
	if clone.Spec.Customization == nil {
		return ""
	}
	if fields := clone.Spec.Customization.UnusedSecretFields(); len(fields) > 0 && r.Recorder != nil {
		r.Recorder.Event(clone, corev1.EventTypeWarning, customizationNotSentReason,
			fmt.Sprintf(customizationNotSentMessageFmt, strings.Join(fields, ", ")))
	}
	data, err := json.Marshal(providerCustomization(clone.Spec.Customization))
	if err != nil {
		return ""
	}
	return string(data)
}
