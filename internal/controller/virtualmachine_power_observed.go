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
	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// A provider reports a VM's power state as On or Off, and one that can tell
// the difference (libvirt, review R1) as Suspended — paused or suspended to
// RAM: still active on its host, resuming at the size it has — or Unknown.
// "Off" therefore always means powered off, which the clustered shrink
// deferral relies on (poweredOff): a suspended guest with a wake timer must
// never have a shrink recorded as applied while it still holds its old size.

// observedPowerState is what status.powerState records for a power state a
// provider reported: the reported value when the API knows it, Unknown
// otherwise (so an unexpected value never makes the whole status write fail
// validation), and nothing for an empty report.
func observedPowerState(reported string) infravirtrigaudiov1beta1.ObservedPowerState {
	switch s := infravirtrigaudiov1beta1.ObservedPowerState(reported); s {
	case "", infravirtrigaudiov1beta1.ObservedPowerStateOn, infravirtrigaudiov1beta1.ObservedPowerStateOff,
		infravirtrigaudiov1beta1.ObservedPowerState(infravirtrigaudiov1beta1.PowerStateOffGraceful),
		infravirtrigaudiov1beta1.ObservedPowerStateSuspended,
		infravirtrigaudiov1beta1.ObservedPowerStateUnknown:
		return s
	}
	return infravirtrigaudiov1beta1.ObservedPowerStateUnknown
}

// powerStateUnmanaged reports whether reported is a power state the manager
// does not act on: Suspended or Unknown. Its On/Off operations cannot bring
// such a VM to its desired state safely — starting an active domain fails,
// and powering off a VM in an unknown state is a guess — and a reconfigure of
// an active domain that is not running is refused by the provider anyway. The
// VM is left as it is until it runs or is powered off — except a Suspended VM
// whose spec asks for Off, which reconcileVM powers off (review H4).
func powerStateUnmanaged(reported string) bool {
	switch observedPowerState(reported) {
	case infravirtrigaudiov1beta1.ObservedPowerStateSuspended, infravirtrigaudiov1beta1.ObservedPowerStateUnknown:
		return true
	}
	return false
}

// adoptedDesiredPowerState is the spec.powerState an adopted VM starts with:
// Off for a VM that is powered off, On otherwise. A suspended (or unknown) VM
// is adopted as On — never Off, which would power it off — and is left alone
// while it stays suspended (powerStateUnmanaged).
func adoptedDesiredPowerState(reported string) infravirtrigaudiov1beta1.PowerState {
	if reported == string(infravirtrigaudiov1beta1.PowerStateOff) {
		return infravirtrigaudiov1beta1.PowerStateOff
	}
	return infravirtrigaudiov1beta1.PowerStateOn
}
