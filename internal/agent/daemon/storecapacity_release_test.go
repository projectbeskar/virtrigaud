//go:build !virtrigaud_dev

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

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestReleaseBuildReportsNoStoreCapacity: a release build's signed host
// answer never carries the store capacity, even to a request that asks for
// it, so its answers stay as they were.
func TestReleaseBuildReportsNoStoreCapacity(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	info, err := providerv1.NewProviderClient(r.conn()).GetHostInfo(callCtx(t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{7}, 16), StoreCapacityVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetHostAnswer() == nil {
		t.Fatal("no signed host answer")
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(info.GetHostAnswer().GetBody(), a); err != nil {
		t.Fatal(err)
	}
	if a.GetStoreCapacity() != nil {
		t.Fatalf("a release build's answer carries a store capacity: %v", a.GetStoreCapacity())
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}
