//go:build virtrigaud_dev

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

package daemon_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// storeCapacity asks the running agent for a signed host answer with its
// store capacity and returns the capacity (nil when the answer has none).
func (r *running) storeCapacity() *providerv1.StoreCapacity {
	r.h.t.Helper()
	info, err := providerv1.NewProviderClient(r.conn()).GetHostInfo(callCtx(r.h.t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{7}, 16), StoreCapacityVersion: hostidentity.StoreCapacityVersion})
	if err != nil {
		r.h.t.Fatalf("GetHostInfo: %v", err)
	}
	if info.GetHostAnswer() == nil {
		r.h.t.Fatal("no signed host answer")
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(info.GetHostAnswer().GetBody(), a); err != nil {
		r.h.t.Fatal(err)
	}
	return a.GetStoreCapacity()
}

// TestAgentReportsStoreCapacity: a development build's signed host answer
// carries the store's real live size and threshold, at OK with the default
// threshold, WARNING from three quarters of a small threshold, and FULL above
// it (that admission refuses exactly then is TestCapacity_MatchesAdmission).
func TestAgentReportsStoreCapacity(t *testing.T) {
	h, _ := newFakeHost(t)
	r, _ := h.run()
	c := r.storeCapacity()
	if c == nil {
		t.Fatal("a development build's answer carries no store capacity")
	}
	if c.GetThresholdBytes() != opstore.DefaultMaxStoreBytes || c.GetLiveBytes() == 0 ||
		c.GetLevel() != providerv1.StoreLevel_STORE_LEVEL_OK {
		t.Fatalf("default threshold: %v", c)
	}
	live := c.GetLiveBytes()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	// A threshold the live size sits between three quarters of and at:
	// WARNING.
	h.maxStoreBytes = int64(live + live/8)
	r, _ = h.run()
	c = r.storeCapacity()
	if c.GetThresholdBytes() != uint64(h.maxStoreBytes) ||
		c.GetLevel() != hostidentity.StoreLevelOf(c.GetLiveBytes(), c.GetThresholdBytes()) {
		t.Fatalf("small threshold: %v", c)
	}
	if c.GetLevel() != providerv1.StoreLevel_STORE_LEVEL_WARNING {
		t.Fatalf("live %d against threshold %d (previous live %d): level %s, want WARNING",
			c.GetLiveBytes(), c.GetThresholdBytes(), live, c.GetLevel())
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	// Any real store is above a one-byte threshold: FULL.
	h.maxStoreBytes = 1
	r, _ = h.run()
	c = r.storeCapacity()
	if c.GetThresholdBytes() != 1 || c.GetLevel() != providerv1.StoreLevel_STORE_LEVEL_FULL {
		t.Fatalf("one-byte threshold: %v", c)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}
