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

package hostidentity_test

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	levelOK      = providerv1.StoreLevel_STORE_LEVEL_OK
	levelWarning = providerv1.StoreLevel_STORE_LEVEL_WARNING
	levelFull    = providerv1.StoreLevel_STORE_LEVEL_FULL
)

func TestStoreLevelOf(t *testing.T) {
	for _, tc := range []struct {
		live, threshold uint64
		want            providerv1.StoreLevel
	}{
		{0, 0, providerv1.StoreLevel_STORE_LEVEL_UNSPECIFIED},
		{5, 0, providerv1.StoreLevel_STORE_LEVEL_UNSPECIFIED},
		{0, 1000, levelOK},
		{749, 1000, levelOK},
		{750, 1000, levelWarning},
		{1000, 1000, levelWarning}, // at the threshold admission still accepts
		{1001, 1000, levelFull},
		// threshold / 4 * 3 in integer arithmetic: 1003 / 4 * 3 = 750.
		{749, 1003, levelOK},
		{750, 1003, levelWarning},
		{(256 << 20) / 4 * 3, 256 << 20, levelWarning},
		{(256<<20)/4*3 - 1, 256 << 20, levelOK},
		{256<<20 + 1, 256 << 20, levelFull},
	} {
		if got := hostidentity.StoreLevelOf(tc.live, tc.threshold); got != tc.want {
			t.Errorf("StoreLevelOf(%d, %d) = %s, want %s", tc.live, tc.threshold, got, tc.want)
		}
	}
}

// storeAnswer is a host answer carrying c, and an expectation that asked for
// a store capacity.
func (f *answerFixture) storeAnswer(c *providerv1.StoreCapacity) (*providerv1.HostAnswer, hostidentity.AnswerExpectation) {
	exp := f.exp(hostidentity.BodyTypeHostAnswer)
	exp.StoreCapacityVersion = hostidentity.StoreCapacityVersion
	return &providerv1.HostAnswer{Context: f.ctx(), AgentApiVersion: hostidentity.StoreCapacityAgentAPIVersion, GrantVersions: []uint32{1},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY, StoreCapacity: c}, exp
}

func TestVerifyAnswer_StoreCapacity(t *testing.T) {
	f := newAnswerFixture(t)
	for _, c := range []*providerv1.StoreCapacity{
		{RequestedVersion: 1, Version: 1, LiveBytes: 10, ThresholdBytes: 1000, Level: levelOK},
		{RequestedVersion: 1, Version: 1, LiveBytes: 800, ThresholdBytes: 1000, Level: levelWarning},
		{RequestedVersion: 1, Version: 1, LiveBytes: 1001, ThresholdBytes: 1000, Level: levelFull},
		{RequestedVersion: 1, Version: 1, Level: providerv1.StoreLevel_STORE_LEVEL_NOT_MEASURED},
	} {
		ha, exp := f.storeAnswer(c)
		v, err := hostidentity.VerifyAnswer(f.sign(t, ha), exp)
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if !proto.Equal(v.Host.GetStoreCapacity(), c) {
			t.Errorf("verified capacity = %v, want %v", v.Host.GetStoreCapacity(), c)
		}
	}
	// An older agent answers without the field: verified, no capacity.
	ha, exp := f.storeAnswer(nil)
	ha.AgentApiVersion = hostidentity.StoreCapacityAgentAPIVersion - 1
	v, err := hostidentity.VerifyAnswer(f.sign(t, ha), exp)
	if err != nil || v.Host.GetStoreCapacity() != nil {
		t.Fatalf("an answer without a capacity: %v, %v", v.Host.GetStoreCapacity(), err)
	}
}

func TestVerifyAnswer_StoreCapacityRules(t *testing.T) {
	f := newAnswerFixture(t)
	for name, tc := range map[string]struct {
		c    *providerv1.StoreCapacity
		sent uint32
		want string
	}{
		"not asked for":     {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1, ThresholdBytes: 1000, Level: levelOK}, 0, "asked for none"},
		"threshold 0":       {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1, Level: levelOK}, 1, "threshold of 0"},
		"level unspecified": {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1, ThresholdBytes: 1000}, 1, "does not match"},
		"OK when near":      {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 750, ThresholdBytes: 1000, Level: levelOK}, 1, "does not match"},
		"OK when full":      {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1001, ThresholdBytes: 1000, Level: levelOK}, 1, "does not match"},
		"WARNING when full": {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1001, ThresholdBytes: 1000, Level: levelWarning}, 1, "does not match"},
		"FULL at threshold": {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1000, ThresholdBytes: 1000, Level: levelFull}, 1, "does not match"},
		"undefined level":   {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1, ThresholdBytes: 1000, Level: 9}, 1, "undefined value"},
		"version above":     {nil, hostidentity.StoreCapacityVersion + 1, "above"},
		"echo 0":            {&providerv1.StoreCapacity{Version: 1, LiveBytes: 1, ThresholdBytes: 1000, Level: levelOK}, 1, "echoes version 0"},
		"echo above":        {&providerv1.StoreCapacity{RequestedVersion: 2, Version: 1, LiveBytes: 1, ThresholdBytes: 1000, Level: levelOK}, 1, "echoes version 2"},
		"not measured, sizes": {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1, Level: providerv1.StoreLevel_STORE_LEVEL_NOT_MEASURED}, 1,
			"carries sizes"},
		"missing from a current agent": {nil, 1, "answered without it"},
		"version 0":                    {&providerv1.StoreCapacity{RequestedVersion: 1, LiveBytes: 1, ThresholdBytes: 1000, Level: levelOK}, 1, "uses version 0"},
		"version above the request":    {&providerv1.StoreCapacity{RequestedVersion: 1, Version: 2, LiveBytes: 1, ThresholdBytes: 1000, Level: levelOK}, 1, "uses version 2"},
	} {
		t.Run(name, func(t *testing.T) {
			ha, exp := f.storeAnswer(tc.c)
			exp.StoreCapacityVersion = tc.sent
			_, err := hostidentity.VerifyAnswer(f.sign(t, ha), exp)
			if !errors.Is(err, hostidentity.ErrAnswer) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("VerifyAnswer = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// TestVerifyAnswer_StoreCapacitySigned: the capacity is part of the signed
// body; altering, adding or stripping it after signing fails verification.
func TestVerifyAnswer_StoreCapacitySigned(t *testing.T) {
	f := newAnswerFixture(t)
	signedCap := &providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1001, ThresholdBytes: 1000, Level: levelFull}
	ha, exp := f.storeAnswer(signedCap)
	blob := f.sign(t, ha)
	if _, err := hostidentity.VerifyAnswer(blob, exp); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*providerv1.HostAnswer){
		"lowered to OK": func(a *providerv1.HostAnswer) {
			a.StoreCapacity = &providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 10, ThresholdBytes: 1000, Level: levelOK}
		},
		"threshold raised": func(a *providerv1.HostAnswer) {
			a.StoreCapacity = &providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 1001, ThresholdBytes: 2000, Level: levelOK}
		},
		"stripped": func(a *providerv1.HostAnswer) { a.StoreCapacity = nil },
		"version changed": func(a *providerv1.HostAnswer) {
			a.StoreCapacity = proto.CloneOf(a.StoreCapacity)
			a.StoreCapacity.Version = 0
		},
		"echo changed": func(a *providerv1.HostAnswer) {
			a.StoreCapacity = proto.CloneOf(a.StoreCapacity)
			a.StoreCapacity.RequestedVersion = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := &providerv1.HostAnswer{}
			if err := proto.Unmarshal(blob.GetBody(), body); err != nil {
				t.Fatal(err)
			}
			edit(body)
			raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			tampered := proto.CloneOf(blob)
			tampered.Body = raw
			_, err = hostidentity.VerifyAnswer(tampered, exp)
			if !errors.Is(err, hostidentity.ErrAnswer) || !strings.Contains(err.Error(), "signature does not verify") {
				t.Fatalf("VerifyAnswer of a tampered capacity = %v, want a signature failure", err)
			}
		})
	}
	// Added to an answer signed without one.
	plain, exp2 := f.storeAnswer(nil)
	pblob := f.sign(t, plain)
	added := proto.CloneOf(plain)
	added.StoreCapacity = &providerv1.StoreCapacity{RequestedVersion: 1, Version: 1, LiveBytes: 10, ThresholdBytes: 1000, Level: levelOK}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(added)
	if err != nil {
		t.Fatal(err)
	}
	tampered := proto.CloneOf(pblob)
	tampered.Body = raw
	if _, err := hostidentity.VerifyAnswer(tampered, exp2); !errors.Is(err, hostidentity.ErrAnswer) {
		t.Fatalf("VerifyAnswer of an added capacity = %v, want a refusal", err)
	}
}

// TestVerifyAnswer_StoreCapacityRequestStripped: a relay that strips
// store_capacity_version from the request makes a current agent answer
// without a capacity (as it was asked for none); the manager, which asked,
// refuses that answer instead of reading it as "not reported". A relay that
// raises it makes the agent's signed echo differ, which is refused too.
func TestVerifyAnswer_StoreCapacityRequestStripped(t *testing.T) {
	f := newAnswerFixture(t)
	ha, exp := f.storeAnswer(nil) // what an agent asked for none signs
	_, err := hostidentity.VerifyAnswer(f.sign(t, ha), exp)
	if !errors.Is(err, hostidentity.ErrAnswer) || !strings.Contains(err.Error(), "altered in transit") {
		t.Fatalf("VerifyAnswer of an answer to a stripped request = %v, want a refusal", err)
	}
	exp.StoreCapacityVersion = 0 // the manager asked for none: nothing to miss
	if _, err := hostidentity.VerifyAnswer(f.sign(t, ha), exp); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyStoreCapacity_Versions: a manager that asks for a version above
// the agent's gets the agent's own version with the request echoed (forward
// compatibility); a version above the request, above what the verifier
// implements, or 0 is refused.
func TestVerifyStoreCapacity_Versions(t *testing.T) {
	c := func(requested, version uint32) *providerv1.StoreCapacity {
		return &providerv1.StoreCapacity{RequestedVersion: requested, Version: version, LiveBytes: 1, ThresholdBytes: 1000, Level: levelOK}
	}
	if err := hostidentity.VerifyStoreCapacity(c(3, 1), 3, hostidentity.StoreCapacityAgentAPIVersion); err != nil {
		t.Fatalf("a version 1 answer to a version 3 request: %v", err)
	}
	for name, tc := range map[string]struct{ requested, version, sent uint32 }{
		"above the supported version": {3, hostidentity.StoreCapacityVersion + 1, 3},
		"above the request":           {1, 2, 1},
		"zero":                        {3, 0, 3},
		"echo differs":                {2, 1, 3},
	} {
		if err := hostidentity.VerifyStoreCapacity(c(tc.requested, tc.version), tc.sent, hostidentity.StoreCapacityAgentAPIVersion); !errors.Is(err, hostidentity.ErrAnswer) {
			t.Errorf("%s: VerifyStoreCapacity = %v, want a refusal", name, err)
		}
	}
}
