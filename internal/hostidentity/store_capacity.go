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

package hostidentity

import (
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// StoreCapacityVersion is the highest version of StoreCapacity this build
// implements: what a manager sends as GetHostInfoRequest.store_capacity_version
// and the highest an agent answers.
const StoreCapacityVersion uint32 = 1

// Store levels as fractions of the threshold: WARNING from
// StoreWarningNum/StoreWarningDen of it. The per-host agent's compaction
// treats the same share as near its threshold.
const (
	StoreWarningNum = 3
	StoreWarningDen = 4
)

// StoreLevelOf is the level of a store whose live pages use live bytes
// against a threshold of threshold bytes, by the rule of the contract's
// StoreLevel: FULL when live > threshold (the agent's admission refuses new
// operations exactly then); otherwise WARNING when
// live >= threshold / 4 * 3, in integer arithmetic; otherwise OK. A threshold
// of 0 has no level (UNSPECIFIED).
func StoreLevelOf(live, threshold uint64) providerv1.StoreLevel {
	switch {
	case threshold == 0:
		return providerv1.StoreLevel_STORE_LEVEL_UNSPECIFIED
	case live > threshold:
		return providerv1.StoreLevel_STORE_LEVEL_FULL
	case live >= threshold/StoreWarningDen*StoreWarningNum:
		return providerv1.StoreLevel_STORE_LEVEL_WARNING
	}
	return providerv1.StoreLevel_STORE_LEVEL_OK
}

// VerifyStoreCapacity checks the store capacity c of a HostAnswer against the
// store_capacity_version the manager sent (requested): none is allowed when
// it sent 0; a capacity names a threshold other than 0 and the level
// StoreLevelOf gives for its sizes. A nil c is valid: an agent older than
// the field never sets it, and the manager treats that as no report.
func VerifyStoreCapacity(c *providerv1.StoreCapacity, requested uint32) error {
	if c == nil {
		return nil
	}
	switch {
	case requested == 0:
		return refuse("a store capacity in the answer to a request that asked for none")
	case c.GetThresholdBytes() == 0:
		return refuse("a store capacity with a threshold of 0")
	case c.GetLevel() != StoreLevelOf(c.GetLiveBytes(), c.GetThresholdBytes()):
		return refuse("the store capacity level %s does not match its sizes", c.GetLevel())
	}
	return nil
}
