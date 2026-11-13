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

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// floorAgent is an agent with a settable clock floor.
type floorAgent struct {
	floor time.Time
	state clockfloor.StateKind
}

func (a *floorAgent) HostView(context.Context, uint64) (opstore.HostView, error) {
	return opstore.HostView{}, nil
}
func (a *floorAgent) TrustState() (providerv1.AgentState, string) {
	return providerv1.AgentState_AGENT_STATE_READY, ""
}
func (a *floorAgent) ClockFloor() time.Time                 { return a.floor }
func (a *floorAgent) ClockFloorState() clockfloor.StateKind { return a.state }
func (a *floorAgent) UnattestedCause() string               { return "" }

// TestGrantClockFailsClosed: grants are judged at the later of the host
// clock and a healthy floor, read once per call; an unhealthy floor gives the
// gate's cause and a clock the verifier refuses as untrusted, which the
// operation path answers UNATTESTED (never definite), while the certificate
// check falls back to the host clock.
func TestGrantClockFailsClosed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := &floorAgent{floor: now.Add(time.Hour), state: clockfloor.StateOK}
	s := &Server{opts: Options{Agent: a, Now: func() time.Time { return now }, Log: logr.Discard()}}
	clock, cause := s.grantClock()
	if cause != "" || !clock.Now().Equal(a.floor) {
		t.Fatalf("healthy floor ahead: %v %q, want the floor", clock.Now(), cause)
	}
	// The call's clock keeps its one reading even if the floor changes.
	a.state = clockfloor.StateClockFloorAhead
	if !clock.Now().Equal(a.floor) {
		t.Fatal("the clock of a call read the floor's state again")
	}
	for st, want := range map[clockfloor.StateKind]string{clockfloor.StateClockFloorAhead: CauseClockFloorAhead, "": CauseClockFloorUnavailable} {
		a.state = st
		clock, cause := s.grantClock()
		if cause != want || !clock.Now().IsZero() {
			t.Fatalf("floor %q: %v %q, want the zero time and %q", st, clock.Now(), cause, want)
		}
		if got := s.verifyNow(); !got.Equal(now) {
			t.Fatalf("floor %q: certificates judged at %v, want the host clock", st, got)
		}
	}
	err := s.verifyRefusal("m", "actor", &grantverify.Refusal{Reason: grantverify.ReasonGrantUnauthenticated, Rule: grantverify.RuleUntrustedClock})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), CauseClockFloorUnavailable) {
		t.Fatalf("an untrusted-clock refusal: %v, want UNATTESTED", err)
	}
}
