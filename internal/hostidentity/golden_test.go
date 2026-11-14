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
	"bytes"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// goldenBodyFile holds the body bytes the agent's encoder produced for
// goldenAnswer, in hexadecimal. The manager refuses a body that is not its
// own deterministic re-encoding, so the agent and the manager must encode
// alike: this vector fails when either side's protobuf encoder changes its
// output. Regenerate it only deliberately (VIRTRIGAUD_UPDATE_GOLDEN=1), and
// build the agent and the manager with the same protobuf-go minor version.
const goldenBodyFile = "testdata/operation_answer_body.hex"

// goldenNonce is the fixed nonce of the golden answer.
var goldenNonce = bytes.Repeat([]byte{0x5a}, 16)

// goldenAnswer is a fixed OperationAnswer with every kind of field set,
// naming the trusttest enrolment.
func goldenAnswer(rec hostidentity.Record) *providerv1.OperationAnswer {
	return &providerv1.OperationAnswer{
		Context: &providerv1.AnswerContext{
			Nonce: goldenNonce, ProviderUid: trusttest.ProviderUID, HostId: trusttest.HostName, InstallId: rec.InstallID,
			EnrolmentId: rec.EnrolmentID, ChainPosition: position(41), AgentState: providerv1.AgentState_AGENT_STATE_READY,
			AgentTime: timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)), ContinuityHead: bytes.Repeat([]byte{0xcd}, 32),
			CheckpointSeq: 7, CheckpointHead: bytes.Repeat([]byte{0xee}, 32),
		},
		OpId: bytes.Repeat([]byte{1}, 16), OpEnrolmentId: rec.EnrolmentID,
		State: providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, Tombstoned: true,
		Kind: int32(providerv1.OperationKind_OPERATION_KIND_RECONFIGURE), Attempt: 3, VmUid: "vm-uid-golden",
		BindingId: bytes.Repeat([]byte{2}, 16), IntentDigest: bytes.Repeat([]byte{3}, 32), OutcomeReason: "applied_live",
	}
}

// leafOf is the DER host certificate of p.
func leafOf(t *testing.T, p *trusttest.PKI) []byte {
	t.Helper()
	block, _ := pem.Decode(p.HostCertPEM)
	if block == nil {
		t.Fatal("no host certificate")
	}
	return block.Bytes
}

// TestGoldenAnswerEncoding: the agent's encoder still produces the checked-in
// body bytes, and the manager accepts exactly those bytes signed by the host.
func TestGoldenAnswerEncoding(t *testing.T) {
	p := trusttest.New(t)
	blob, err := answer.Sign(p.HostKey, leafOf(t, p), goldenAnswer(p.Record))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("VIRTRIGAUD_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenBodyFile, []byte(hex.EncodeToString(blob.GetBody())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Clean(goldenBodyFile))
	if err != nil {
		t.Fatal(err)
	}
	golden, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob.GetBody(), golden) {
		t.Fatalf("the agent's encoding of the golden answer changed:\n got %x\nwant %x", blob.GetBody(), golden)
	}
	if again, err := (proto.MarshalOptions{Deterministic: true}).Marshal(goldenAnswer(p.Record)); err != nil || !bytes.Equal(again, golden) {
		t.Fatal("the deterministic encoding the manager checks against differs from the golden bytes")
	}
	pin, err := hostidentity.SPKIPin(&p.HostKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	signed := signRaw(t, p.HostKey, leafOf(t, p), hostidentity.BodyTypeOperationAnswer, golden)
	exp := hostidentity.AnswerExpectation{
		BodyType: hostidentity.BodyTypeOperationAnswer, Anchors: p.HostCAPool(), HostURI: p.HostURI, HostName: trusttest.HostName,
		HostID: trusttest.HostName, Pins: []string{pin}, ProviderUID: trusttest.ProviderUID, InstallID: p.Record.InstallID,
		EnrolmentID: p.Record.EnrolmentID, Nonce: goldenNonce, Now: time.Now(),
	}
	if _, err := hostidentity.VerifyAnswer(signed, exp); err != nil {
		t.Fatalf("the manager refuses the golden body: %v", err)
	}
}
