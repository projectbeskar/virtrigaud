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

package controller

// The whole-protocol model test of the host ledger: a seeded, deterministic
// schedule of interleaved events across at least two manager epochs, with
// the protocol's safety and liveness invariants checked after every step.
//
// What runs for real:
//
//   - the agent's operation store (opstore.Store with the ledger gate) on an
//     in-memory anchor file system and a memory-backed store directory,
//     with an in-process executor whose effects are the ground truth:
//     admission, the ledger gate in Execute, attestation (first and
//     re-attestation, its durable record), ledger marks, retire-if-unknown
//     with its trace scan, recovery, re-enrolment and its freeze;
//   - the manager's host ledger library (HostLedger) over a controller-runtime
//     fake client: initialise (first contact, archive of an earlier store),
//     rebind to a replaced Host, begin session, raise, adopt, its tags and
//     keys, the per-process guard, the write counter and the ledger index,
//     every hold and where it is kept;
//   - the ledger evidence a ledger is started from, signed by a stand-in host
//     key over the store's real facts (position, origin, attestation record)
//     and checked by the real answer verifier;
//   - the manager's only grant signing points for the ledger (sendAttest,
//     sendLedgerMark, sendRetire) and its retirement decision (decideRetire,
//     with receiptLedgered and ledgerContinuous), and coverRetirement.
//
// What is modelled:
//
//   - the wire: a grant the manager signed reaches the agent's store as the
//     order it names (signing, the grant verifier and the transport are
//     tested on their own); the agent's grant check is reduced to the
//     enrolment it names. Messages can be delivered at once, delayed (put on
//     a queue and delivered or dropped later, in any order) or lost;
//   - the agent server's challenge registry (at most four outstanding per
//     purpose, in memory) and its refusal of operations while the store
//     awaits attestation, is freezing or is lost;
//   - the attestation driver and the VirtualMachine controller's attempt
//     markers and intents (the receipt, the intent's ledger stamp, the
//     re-send and expect_known rule, polling), which follow the real
//     driver's steps but call the real library above;
//   - etcd: a snapshot and restore of every object of the fake client (the
//     ledger Secrets and Hosts), the pins state (enrolment, store state,
//     ledger index, holds) and the attempt markers, together;
//   - a manager downgrade window (an older build that signs nothing at
//     grant version 2 and never touches the ledger), leader changes with two
//     writers, and manager restarts (a new process: no guard, no own record,
//     no holds in memory).
//
// The invariants, checked after every step (effects are counted in the
// executor, which no rollback undoes):
//
//  1. an attempt the manager ever closed Retired never has an effect. A
//     violation is exempted only when the run's record explains it: the
//     op was accepted and ran, an agent store+anchor rollback to a snapshot
//     below its acceptance then dropped its record, and an etcd restore
//     after it ran and before the retirement lowered the store's ledger below its
//     acceptance (counted and reported, never failed); every other
//     violation fails;
//  2. no operation starts before the ledger covers it: when a step runs,
//     the agent is attested, its ledger mark covers the operation's
//     acceptance, and the mark is a position an attestation or ledger mark
//     was signed with (each checked under invariant 4 when signed);
//  3. at most one effect per op_id. A repeated effect is exempted only when
//     an agent store+anchor rollback to a snapshot taken before the earlier
//     effect happened between the two (counted and reported);
//  4. nothing is signed for an unattested or unbound host: at every
//     attestation, ledger mark, retirement and operation grant, the store's
//     current ledger object, read from the API server then, exists, is not
//     archived, is bound to the current Host object and holds the grant's
//     session (pending for an attestation, Attested and current for the
//     others); an attestation or mark names a position at or below the
//     object's position (the same head at the same sequence number), and a
//     retirement is for the object's store and generation;
//  5. no permanent wedge: once the adversary stops, every run reaches a
//     quiescent state in which every attempt is closed or the host is held
//     for a documented reason (the agent's store lost, or a documented hold
//     met by the last drive of the final round); a run that injected none
//     of the faults that can hold a host for good must end with the ledger
//     Attested in the agent's session and every attempt closed.
//
// Seeds: VIRTRIGAUD_PROTOCOL_MODEL_SEEDS (default 1000, 200 with -short); one
// seed: VIRTRIGAUD_PROTOCOL_MODEL_SEED. A failure prints the seed, the
// shortest failing prefix of its schedule and that prefix's trace.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	pmSeedsEnv     = "VIRTRIGAUD_PROTOCOL_MODEL_SEEDS"
	pmSeedEnv      = "VIRTRIGAUD_PROTOCOL_MODEL_SEED"
	pmWorkersEnv   = "VIRTRIGAUD_PROTOCOL_MODEL_WORKERS"
	pmDefaultSeeds = 1000
	pmShortSeeds   = 200
	// pmSteps is the number of random steps of a run before the adversary
	// stops.
	pmSteps = 90
	// pmQuiesceRounds bounds the rounds of the quiescent phase.
	pmQuiesceRounds = 40
	// pmMaxAttempts bounds the attempts a run prepares.
	pmMaxAttempts = 12
	pmHostID      = "h"
	pmNamespace   = "ns"
	pmEnrolNS     = "virtrigaud-enrolment"
	pmIdentity    = "serial-h"
	pmProviderUID = "0b7a1c3e-1111-4222-8333-944455556666"
	pmVersion     = "v0.5.0"
	pmStepName    = "effect"
)

// pmNoStartCheck switches invariant 2 off (VIRTRIGAUD_PROTOCOL_MODEL_NO_START_CHECK=1), to show that the
// other invariants alone notice a missing ledger gate too.
var pmNoStartCheck = os.Getenv("VIRTRIGAUD_PROTOCOL_MODEL_NO_START_CHECK") == "1"

// pmListFlagged logs the first seeds of each flagged class
// (VIRTRIGAUD_PROTOCOL_MODEL_LIST_FLAGGED=1), for triage.
var pmListFlagged = os.Getenv("VIRTRIGAUD_PROTOCOL_MODEL_LIST_FLAGGED") == "1"

// pmInstall is the installation id of the model's manager.
var pmInstall = bytes.Repeat([]byte{0x42}, 16)

// pmFault is a fault a run injects; the faults in pmLenient may hold the
// host for good (for a documented reason), the others never may.
type pmFault int

const (
	faultAgentRollback pmFault = iota + 1 // the agent's store and anchor rolled back together
	faultStoreOnlyRollback
	faultEtcdRestore          // with a manager restart
	faultEtcdRestoreNoRestart // the running manager sees the restore
	faultPartialRestore       // the ledger object alone restored
	faultRestoreAcross        // an etcd restore across an enrolment or store change
	faultLedgerDeleted
)

func (f pmFault) String() string {
	return [...]string{"", "agent-rollback", "store-only-rollback", "etcd-restore", "etcd-restore-no-restart",
		"partial-restore", "restore-across-enrolment", "ledger-deleted"}[f]
}

// pmLenient are the faults after which a host may stay held for good.
var pmLenient = map[pmFault]bool{faultAgentRollback: true, faultStoreOnlyRollback: true, faultEtcdRestoreNoRestart: true,
	faultPartialRestore: true, faultRestoreAcross: true, faultLedgerDeleted: true}

// pmDocumentedHolds are the manager hold reasons a lenient run may end in.
var pmDocumentedHolds = map[string]bool{LedgerReasonRegressed: true, LedgerReasonTampered: true, LedgerReasonMissing: true,
	LedgerReasonAhead: true, LedgerReasonFork: true, LedgerReasonStoreMismatch: true, LedgerReasonAgentQuarantined: true,
	LedgerReasonUnconfirmed: true}

// ---------------------------------------------------------------------------
// The agent side.

// pmExec is the executor: every kind has one once step, whose effect is the
// ground truth the invariants read.
type pmExec struct{ w *pmWorld }

func (e *pmExec) Plan(providerv1.OperationKind) ([]executor.Step, error) {
	return []executor.Step{{Name: pmStepName, Once: true}}, nil
}

func (e *pmExec) Run(_ context.Context, op executor.Operation, _ executor.Step) error {
	e.w.onStart(op.OpID)
	k := hex.EncodeToString(op.OpID)
	e.w.effects[k]++
	e.w.effectSteps[k] = append(e.w.effectSteps[k], e.w.stepNo)
	e.w.present[k] = true
	return nil
}

func (e *pmExec) Inspect(_ context.Context, op executor.Operation, _ executor.Step) (executor.Evidence, error) {
	if e.w.present[hex.EncodeToString(op.OpID)] {
		return executor.EvidencePresent, nil
	}
	return executor.EvidenceAbsent, nil
}

func (e *pmExec) Goal(_ context.Context, op executor.Operation, _ executor.Step) (bool, error) {
	return e.w.present[hex.EncodeToString(op.OpID)], nil
}

func (e *pmExec) Traces(_ context.Context, op executor.Operation) (executor.Trace, error) {
	p := e.w.present[hex.EncodeToString(op.OpID)]
	return executor.Trace{Op: p, Placement: p}, nil
}

// pmAgent is the agent process and its store.
type pmAgent struct {
	dir   string
	mem   *journaltest.MemFS
	s     *opstore.Store
	up    bool
	start uint64 // the active enrolment's start epoch
	// challenges are the outstanding challenge nonces, by purpose, oldest
	// first (in memory: a restart forgets them).
	challenges map[providerv1.ChallengePurpose][][16]byte
	// snaps are saved copies of the store and its anchor file system.
	snaps []pmAgentSnap
}

type pmAgentSnap struct {
	db   []byte
	mem  map[string][]byte
	seq  uint64
	step int
}

// ---------------------------------------------------------------------------
// The manager side.

// pmAttempt is one attempt marker with its intent (etcd state).
type pmAttempt struct {
	n       int
	opID    []byte
	vm      string
	binding []byte
	enrol   []byte
	mint    uint64
	salt    []byte
	request []byte
	// store and gen are the ledger stamp of the intent (MAC-covered).
	store [16]byte
	gen   uint64
	phase infravirtrigaudiov1beta1.OperationAttemptPhase
	// receipt is the marker's receipt; raised reports whether the ledger
	// was raised over it.
	receipt *infravirtrigaudiov1beta1.OperationAttemptReceipt
	recPos  journal.Position
	raised  bool
	outcome string // closed: Succeeded, Retired, NotPerformed, OutcomeUnknown
	reason  string
}

func (a *pmAttempt) clone() *pmAttempt {
	c := *a
	if a.receipt != nil {
		r := *a.receipt
		c.receipt = &r
	}
	return &c
}

func (a *pmAttempt) inFlight() bool { return a.outcome == "" }

func (a *pmAttempt) key() string { return hex.EncodeToString(a.opID) }

// pmMgr is one manager process.
type pmMgr struct {
	epoch int
	l     *HostLedger
	ha    *HostAnswers
	alive bool
	// ready is the driver's readiness for grant version 2: the session,
	// store and generation of the ledger the last drive found the agent
	// READY in.
	ready        bool
	readySession [16]byte
	readyStore   [16]byte
	readyGen     uint64
	readyHost    types.UID
	lastReattest time.Time
	// lastHold is the reason of the hold the last drive met, and
	// persistent whether it was a persistent one.
	lastHold   string
	persistent bool
	// probeHold is the hold the last drive of a quiescent round met.
	probeHold string
}

// pmMsg is a message on the wire, delivered later or lost.
type pmMsg struct {
	kind   string // attest, mark, retire, op
	attest contracts.AttestOrder
	mark   contracts.LedgerMarkOrder
	retire contracts.RetireOrder
	op     opstore.Request
}

// pmAuthority is the HostAuthorityCaller the real signing points deliver
// through: it checks invariant 4 and hands the order to the wire.
type pmAuthority struct{ w *pmWorld }

func (a *pmAuthority) Challenge(context.Context, contracts.ChallengeQuery) (contracts.AuthorityRead, error) {
	return contracts.AuthorityRead{}, errors.New("not used")
}

func (a *pmAuthority) Attest(_ context.Context, o contracts.AttestOrder) (contracts.AuthorityRead, error) {
	a.w.signedAttest(o)
	return contracts.AuthorityRead{}, nil
}

func (a *pmAuthority) LedgerMark(_ context.Context, o contracts.LedgerMarkOrder) (contracts.AuthorityRead, error) {
	a.w.signedMark(o)
	return contracts.AuthorityRead{}, nil
}

func (a *pmAuthority) RetireIfUnknown(_ context.Context, o contracts.RetireOrder) (contracts.AuthorityRead, error) {
	a.w.signedRetire(o)
	return contracts.AuthorityRead{}, nil
}

// pmClient is the API server: the fake client, swapped by a restore.
type pmClient struct {
	client.Client
}

// pmEtcd is an etcd snapshot.
type pmEtcd struct {
	objs     []client.Object
	stores   map[string]HostStoreState
	trusts   map[string]HostTrust
	attempts []*pmAttempt
	hostUID  types.UID
	enrol    []byte
	store    [16]byte
}

// ---------------------------------------------------------------------------
// The world.

type pmWorld struct {
	seed  uint64
	rnd   *mrand.Rand
	ctx   context.Context
	steps int // random steps before the adversary stops
	trace []string
	now   time.Time

	scheme   *k8sruntime.Scheme
	cli      *pmClient
	trust    *staticTrust
	keys     *HostLedgerKeys
	cfg      *HostEnrolmentConfig
	provider *infravirtrigaudiov1beta1.Provider
	hostUID  types.UID
	hostN    int
	auth     *pmAuthority

	agent  *pmAgent
	enrol  []byte // the host's current enrolment (manager and agent agree once re-enrolled)
	enrolN byte
	// cert is the enrolment of the agent's certificate: w.enrol is the
	// manager's (an etcd restore can roll it back; the certificate stays).
	cert     []byte
	attempts []*pmAttempt
	mgrs     []*pmMgr
	epochs   int
	down     bool // a downgrade window: an older manager build runs
	queue    []pmMsg
	etcd     []pmEtcd
	retired  map[string]bool // every attempt the manager ever closed Retired (ground truth)
	// lastAnswer is when a verified answer of each Host object last arrived.
	lastAnswer map[string]time.Time
	faults     map[pmFault]bool
	enabled    map[pmFault]bool

	// Ground truth of the agent's world.
	effects map[string]int
	present map[string]bool
	accSeq  map[string]uint64
	// signedPos are the ledger positions an attestation or ledger mark was
	// signed with (each checked against the durable ledger object then).
	signedPos map[string]bool
	// retireStore and retireGen are the ledger stamp of the attempt a
	// retirement is being signed for.
	retireStore [16]byte
	retireGen   uint64

	// The causal record the safety exemptions are decided on: the step of
	// every accept (with its position), effect and retirement of each op,
	// and of every agent store+anchor rollback and etcd restore.
	stepNo      int
	acceptEv    map[string][]pmAcceptEv
	effectSteps map[string][]int
	retireStep  map[string]int
	// tombSeq is the position the ledger was raised to over a retirement
	// (at or above its tombstone's commit).
	tombSeq   map[string]uint64
	rollbacks []pmRollbackEv
	restores  []pmRestoreEv

	// reply is the reply of the last message delivered at once.
	reply   any
	stats   map[string]int
	flagged []string
}

type pmFailure struct{ msg string }

func (f *pmFailure) Error() string { return f.msg }

func (w *pmWorld) fail(format string, args ...any) {
	panic(&pmFailure{msg: fmt.Sprintf(format, args...)})
}

func (w *pmWorld) log(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf(format, args...))
}

func (w *pmWorld) stat(name string) { w.stats[name]++ }

func (w *pmWorld) id16() [16]byte {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], w.rnd.Uint64())
	binary.BigEndian.PutUint64(b[8:], w.rnd.Uint64())
	return b
}

func (w *pmWorld) chance(n int) bool { return w.rnd.IntN(n) == 0 }

func pmPos(p contracts.LedgerPosition) journal.Position {
	var out journal.Position
	copy(out.StoreID[:], p.StoreID)
	out.Seq = p.Seq
	copy(out.Head[:], p.Head)
	return out
}

func pmPosKey(store [16]byte, seq uint64, head [32]byte) string {
	return fmt.Sprintf("%x/%d/%x", store[:4], seq, head[:6])
}

// pmAcceptEv is one accept of an op: the step and the accepted position.
type pmAcceptEv struct {
	step int
	seq  uint64
}

// pmRollbackEv is a rollback of the agent's store with its anchor: its
// step, and the position and step of the snapshot it restored.
type pmRollbackEv struct {
	step, snapStep int
	snapSeq        uint64
}

// pmRestoreEv is an etcd restore: its step, and the position of the agent
// store's ledger object right after it (0: none).
type pmRestoreEv struct {
	step      int
	store     [16]byte
	ledgerSeq uint64
}

// durable reads the current ledger object of store from the API server
// (nil when there is none).
func (w *pmWorld) durable(store [16]byte) *HostLedgerRecord {
	sec := &corev1.Secret{}
	if err := w.cli.Get(w.ctx, types.NamespacedName{Namespace: pmEnrolNS, Name: hostLedgerSecretName(store)}, sec); err != nil {
		return nil
	}
	rec, err := decodeHostLedger(sec.Data[hostLedgerDataKey])
	if err != nil {
		w.fail("a ledger object does not decode: %v", err)
	}
	return rec
}

// checkDurable is invariant 4: what is signed agrees with the current
// durable ledger object of store, which exists, is not archived, is bound
// to the Host object uid and satisfies ok.
func (w *pmWorld) checkDurable(what string, store [16]byte, uid types.UID, ok func(r *HostLedgerRecord) string) {
	r := w.durable(store)
	switch {
	case r == nil:
		w.fail("invariant 4 violated: %s signed with no ledger object of its store", what)
	case r.Archived:
		w.fail("invariant 4 violated: %s signed from an archived ledger", what)
	case r.HostUID != string(uid):
		w.fail("invariant 4 violated: %s signed for a ledger not bound to the current Host object", what)
	}
	if why := ok(r); why != "" {
		w.fail("invariant 4 violated: %s signed against the durable ledger: %s", what, why)
	}
}

// notAbove reports why position pos is not at or below the ledger's
// position (the same head at the same sequence number), or "".
func notAbove(r *HostLedgerRecord, pos journal.Position) string {
	switch {
	case pos.StoreID != r.StoreID:
		return "another store"
	case pos.Seq > r.Position.Seq:
		return fmt.Sprintf("position %d above the ledger's %d", pos.Seq, r.Position.Seq)
	case pos.Seq == r.Position.Seq && pos.Head != r.Position.Head:
		return "another head at the ledger's position"
	}
	return ""
}

// onStart is invariant 2, at every step run.
func (w *pmWorld) onStart(opID []byte) {
	k := hex.EncodeToString(opID)
	w.stat("op-started")
	if pmNoStartCheck {
		return
	}
	acc := w.accSeq[k]
	a, ok := w.agent.s.Attestation()
	switch {
	case !ok:
		w.fail("invariant 2 violated: op %s started while the agent was not attested", k[:8])
	case acc == 0 || acc > a.Mark.Seq:
		w.fail("invariant 2 violated: op %s started before the ledger covered it: accepted at %d, mark %d", k[:8], acc, a.Mark.Seq)
	case !w.signedPos[pmPosKey(a.Mark.StoreID, a.Mark.Seq, a.Mark.Head)]:
		w.fail("invariant 2 violated: op %s started under a ledger mark (%d) no attestation or ledger mark was signed with", k[:8], a.Mark.Seq)
	}
}

func (w *pmWorld) signedAttest(o contracts.AttestOrder) {
	nonce := [16]byte(o.ChallengeNonce)
	pos := pmPos(o.Ledger)
	w.checkDurable("an attestation", pos.StoreID, w.hostUID, func(r *HostLedgerRecord) string {
		if !r.HasPendingSession(nonce) {
			return "its session is not pending"
		}
		return notAbove(r, pos)
	})
	w.signedPos[pmPosKey(pos.StoreID, pos.Seq, pos.Head)] = true
	w.stat("signed-attest")
	w.send(pmMsg{kind: "attest", attest: o})
}

func (w *pmWorld) signedMark(o contracts.LedgerMarkOrder) {
	session := [16]byte(o.AttestationID)
	pos := pmPos(o.Ledger)
	w.checkDurable("a ledger mark", pos.StoreID, w.hostUID, func(r *HostLedgerRecord) string {
		if r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested || r.AttestationID != session {
			return "not Attested in its session"
		}
		return notAbove(r, pos)
	})
	w.signedPos[pmPosKey(pos.StoreID, pos.Seq, pos.Head)] = true
	w.stat("signed-mark")
	w.send(pmMsg{kind: "mark", mark: o})
}

func (w *pmWorld) signedRetire(o contracts.RetireOrder) {
	session := [16]byte(o.AttestationID)
	w.checkDurable("a retirement", w.retireStore, w.hostUID, func(r *HostLedgerRecord) string {
		switch {
		case r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested || r.AttestationID != session:
			return "not Attested in its session"
		case r.StoreID != w.retireStore || r.Generation != w.retireGen:
			return "not the store and generation the attempt was prepared under"
		}
		return ""
	})
	w.stat("signed-retire")
	w.send(pmMsg{kind: "retire", retire: o})
}

// send puts m on the wire: delivered at once (its reply in w.reply), or
// queued for a later delivery (no reply).
func (w *pmWorld) send(m pmMsg) {
	w.reply = nil
	if w.quiet() || !w.chance(5) {
		w.reply = w.deliver(m)
		return
	}
	w.queue = append(w.queue, m)
	w.log("  queued %s", m.kind)
}

// quiet reports the quiescent phase (no adversary).
func (w *pmWorld) quiet() bool { return w.steps < 0 }

// ---------------------------------------------------------------------------
// Setup.

var (
	pmHostsMu sync.Mutex
	pmHosts   = map[byte]*testHost{}
	// pmBase is the model clock's start: after every stand-in host
	// certificate's start of validity (pmPrepareHosts).
	pmBase time.Time
)

// pmMaxEnrolments bounds the enrolments of a run (one stand-in host
// certificate each).
const pmMaxEnrolments = 12

// pmPrepareHosts creates the stand-in host signers before any run, so that
// the model clock starts after their certificates' validity begins.
func pmPrepareHosts() {
	p := &infravirtrigaudiov1beta1.Provider{}
	p.Namespace, p.Name, p.UID = pmNamespace, "ch", pmProviderUID
	for n := byte(1); n <= pmMaxEnrolments; n++ {
		pmHost(p, n)
	}
	if pmBase.IsZero() {
		pmBase = time.Now().UTC().Truncate(time.Second)
	}
}

// pmT adapts the test host helpers outside a test.
type pmT struct{}

func (pmT) Helper()                           {}
func (pmT) Fatalf(format string, args ...any) { panic(&pmFailure{msg: fmt.Sprintf(format, args...)}) }
func (pmT) Errorf(format string, args ...any) { panic(&pmFailure{msg: fmt.Sprintf(format, args...)}) }
func (pmT) FailNow()                          { panic(&pmFailure{msg: "FailNow"}) }

func pmEnrolment(n byte) []byte { return bytes.Repeat([]byte{0xe0 + n}, 16) }

// pmHost returns the stand-in host signer of enrolment n (cached: the
// certificate depends only on the enrolment).
func pmHost(provider *infravirtrigaudiov1beta1.Provider, n byte) *testHost {
	pmHostsMu.Lock()
	defer pmHostsMu.Unlock()
	if h := pmHosts[n]; h != nil {
		return h
	}
	h := newTestHost(pmT{}, provider, pmHostID, pmEnrolment(n), operation.Epoch{Counter: 1, Holder: bytes.Repeat([]byte{1}, 16)})
	pmHosts[n] = h
	return h
}

func newPMWorld(seed uint64, steps int, dir string) *pmWorld {
	w := &pmWorld{seed: seed, rnd: mrand.New(mrand.NewPCG(seed, 0x9e3779b97f4a7c15)), ctx: context.Background(), steps: steps,
		now: pmBase, retired: map[string]bool{}, lastAnswer: map[string]time.Time{},
		faults: map[pmFault]bool{}, enabled: map[pmFault]bool{}, effects: map[string]int{}, present: map[string]bool{},
		accSeq: map[string]uint64{}, signedPos: map[string]bool{}, stats: map[string]int{}, acceptEv: map[string][]pmAcceptEv{},
		effectSteps: map[string][]int{}, retireStep: map[string]int{}, tombSeq: map[string]uint64{}}
	// Each run enables a few faults; about a third enable none, so that
	// strict liveness is checked on many runs.
	if !w.chance(3) {
		all := []pmFault{faultAgentRollback, faultStoreOnlyRollback, faultEtcdRestore, faultEtcdRestoreNoRestart, faultPartialRestore,
			faultLedgerDeleted}
		for _, f := range all {
			if w.chance(3) {
				w.enabled[f] = true
			}
		}
	}
	w.scheme = k8sruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(w.scheme)
	_ = infravirtrigaudiov1beta1.AddToScheme(w.scheme)
	w.provider = &infravirtrigaudiov1beta1.Provider{}
	w.provider.Namespace, w.provider.Name, w.provider.UID = pmNamespace, "ch", pmProviderUID
	w.enrolN = 1
	w.enrol = pmEnrolment(w.enrolN)
	w.cert = w.enrol
	w.hostN = 1
	w.hostUID = types.UID(fmt.Sprintf("00000000-0000-4000-8000-%012d", w.hostN))
	host := pmHostObject(w.hostUID)
	w.cli = &pmClient{Client: fake.NewClientBuilder().WithScheme(w.scheme).WithObjects(host).Build()}
	w.trust = &staticTrust{trusts: map[string]HostTrust{}, stores: map[string]HostStoreState{}}
	w.setTrust()
	keys, err := NewHostLedgerKeys(bytes.Repeat([]byte{0x11}, 32), nil, bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		w.fail("keys: %v", err)
	}
	w.keys = keys
	w.cfg = &HostEnrolmentConfig{Namespace: pmEnrolNS, InstallID: pmInstall}
	w.auth = &pmAuthority{w: w}
	w.agent = &pmAgent{dir: dir}
	w.agentFresh()
	w.newMgr()
	return w
}

func pmHostObject(uid types.UID) *infravirtrigaudiov1beta1.Host {
	return &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: pmHostID, Namespace: pmNamespace, UID: uid},
		Spec: infravirtrigaudiov1beta1.HostSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "ch"},
			PoolRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"}, Endpoint: "grpc://h:7443", Identity: pmIdentity},
	}
}

func (w *pmWorld) setTrust() {
	w.trust.trusts[pmHostID] = HostTrust{Enrolment: operation.Enrolment{EnrolmentID: bytes.Clone(w.enrol),
		Epoch: operation.Epoch{Counter: 1, Holder: pmInstall}}, InstallID: pmInstall, HostName: pmHostID, HostUID: string(w.hostUID)}
}

func (w *pmWorld) hostTrust() HostTrust { return w.trust.trusts[pmHostID] }

// newMgr starts a manager process (a new epoch).
func (w *pmWorld) newMgr() *pmMgr {
	w.epochs++
	l := &HostLedger{Client: w.cli, APIReader: w.cli, Trust: w.trust, Keys: w.keys, Config: w.cfg,
		Now: func() time.Time { return w.now }, ManagerVersion: func() string { return pmVersion }}
	l.lastAnswer = func(_, _, hostUID string) time.Time { return w.lastAnswer[hostUID] }
	m := &pmMgr{epoch: w.epochs, l: l, ha: &HostAnswers{Ledger: l}, alive: true}
	w.mgrs = append(w.mgrs, m)
	w.log("manager epoch %d starts", m.epoch)
	return m
}

func (w *pmWorld) alive() []*pmMgr {
	var out []*pmMgr
	for _, m := range w.mgrs {
		if m.alive {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Agent operations.

func (w *pmWorld) agentOptions() opstore.Options {
	return opstore.Options{Journal: journal.Options{Dir: w.agent.dir, FS: w.agent.mem, OpenTimeout: 200 * time.Millisecond},
		Executor: &pmExec{w: w}, LedgerGate: true}
}

// agentFresh creates a new store, enrolled under w.cert.
func (w *pmWorld) agentFresh() {
	a := w.agent
	if a.s != nil {
		_ = a.s.Close()
	}
	_ = os.RemoveAll(a.dir)
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		w.fail("mkdir: %v", err)
	}
	a.mem = journaltest.NewMemFS()
	a.snaps = nil
	s, err := opstore.Open(w.ctx, w.agentOptions())
	if err != nil {
		w.fail("open a fresh store: %v", err)
	}
	a.s = s
	if err := s.EstablishContinuity(w.ctx); err != nil {
		w.fail("continuity: %v", err)
	}
	if _, err := s.Recover(w.ctx); err != nil {
		w.fail("recover: %v", err)
	}
	a.start++
	if err := s.Enrol(w.ctx, opstore.Enrolment{HostID: pmHostID, ID: bytes.Clone(w.cert), ProviderUID: pmProviderUID,
		InstallID: pmInstall, StartEpoch: a.start}); err != nil {
		w.fail("enrol: %v", err)
	}
	a.up = true
	a.challenges = map[providerv1.ChallengePurpose][][16]byte{}
}

// agentOpen opens the store after a restart: it awaits attestation.
func (w *pmWorld) agentOpen() {
	a := w.agent
	s, err := opstore.Open(w.ctx, w.agentOptions())
	if err != nil {
		w.fail("reopen the store: %v", err)
	}
	a.s, a.up = s, true
	a.challenges = map[providerv1.ChallengePurpose][][16]byte{}
	w.refreshAccepted()
}

func (w *pmWorld) agentRestart() {
	a := w.agent
	_ = a.s.Close()
	a.mem.Crash()
	w.agentOpen()
	w.log("agent restart")
}

// refreshAccepted re-reads the acceptance positions of every attempt's
// operation from the store (after a restart or rollback).
func (w *pmWorld) refreshAccepted() {
	for _, at := range w.attempts {
		v, ok, err := w.agent.s.Lookup(w.ctx, opstore.OpRef{EnrolmentID: at.enrol, OpID: at.opID})
		if err == nil && ok {
			w.accSeq[at.key()] = v.AcceptedSeq
		}
	}
}

func (w *pmWorld) agentSnapshot() {
	a := w.agent
	if !a.up {
		return
	}
	db, err := os.ReadFile(filepath.Join(a.dir, journal.StoreFile))
	if err != nil {
		return
	}
	a.snaps = append(a.snaps, pmAgentSnap{db: db, mem: a.mem.Snapshot(), seq: a.s.Position().Seq, step: w.stepNo})
	if len(a.snaps) > 3 {
		a.snaps = a.snaps[1:]
	}
	w.log("agent snapshot at %d", a.s.Position().Seq)
}

// agentRollback restores a saved store; with anchor, its anchor file
// system too (undetectable by the host alone).
func (w *pmWorld) agentRollback(withAnchor bool) {
	a := w.agent
	if len(a.snaps) == 0 {
		return
	}
	snap := a.snaps[w.rnd.IntN(len(a.snaps))]
	_ = a.s.Close()
	a.mem.Crash()
	if err := os.WriteFile(filepath.Join(a.dir, journal.StoreFile), snap.db, 0o600); err != nil {
		w.fail("restore the store: %v", err)
	}
	if withAnchor {
		a.mem.Restore(snap.mem)
		w.faults[faultAgentRollback] = true
		w.rollbacks = append(w.rollbacks, pmRollbackEv{step: w.stepNo, snapStep: snap.step, snapSeq: snap.seq})
	} else {
		w.faults[faultStoreOnlyRollback] = true
	}
	w.agentOpen()
	w.log("agent rollback to %d (anchor too: %v)", snap.seq, withAnchor)
}

// agentState is the agent's ledger state as its answers report it.
type pmAgentState struct {
	lost     bool
	attested bool
	session  [16]byte
	mark     journal.Position
	ready    bool
	view     opstore.HostView
}

func (w *pmWorld) agentState() (pmAgentState, bool) {
	a := w.agent
	if !a.up {
		return pmAgentState{}, false
	}
	var st pmAgentState
	js, _ := a.s.Journal().State()
	st.lost = js == journal.StateLost
	v, err := a.s.HostView(w.ctx, 0)
	if err != nil {
		return pmAgentState{}, false
	}
	st.view = v
	if att, ok := a.s.Attestation(); ok && !st.lost {
		st.attested, st.session, st.mark = true, [16]byte(att.Session), att.Mark
	}
	st.ready = !st.lost && st.attested && js == journal.StateReady && a.s.Recovered() && !a.s.Freezing()
	return st, true
}

// agentActiveEnrolment is the agent's active enrolment, nil when none.
func (w *pmWorld) agentActiveEnrolment(st pmAgentState) []byte {
	if st.view.Enrolment == nil {
		return nil
	}
	return st.view.Enrolment.ID
}

// challenge is the agent server's Challenge: a nonce of purpose, or none.
func (w *pmWorld) challenge(purpose providerv1.ChallengePurpose) ([16]byte, bool) {
	st, ok := w.agentState()
	if !ok || st.lost || (purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST) == st.attested {
		return [16]byte{}, false
	}
	n := w.id16()
	q := append(w.agent.challenges[purpose], n)
	if len(q) > 4 {
		q = q[len(q)-4:]
	}
	w.agent.challenges[purpose] = q
	return n, true
}

func (w *pmWorld) consumeChallenge(n [16]byte) (providerv1.ChallengePurpose, bool) {
	for p, q := range w.agent.challenges {
		for i, c := range q {
			if c == n {
				w.agent.challenges[p] = append(q[:i:i], q[i+1:]...)
				return p, true
			}
		}
	}
	return 0, false
}

// pmAttestReply is the agent's answer to an attestation.
type pmAttestReply struct {
	applied bool
	session [16]byte
}

// deliver hands m to the agent and returns its reply (nil: none).
func (w *pmWorld) deliver(m pmMsg) any {
	a := w.agent
	if !a.up {
		w.log("  %s lost: agent down", m.kind)
		return nil
	}
	// The agent's grant check is reduced to the enrolment of its
	// certificate (w.cert) that a grant must name.
	st, _ := w.agentState()
	switch m.kind {
	case "attest":
		o := m.attest
		if !bytes.Equal(o.EnrolmentID, w.cert) {
			w.log("  attest refused: enrolment")
			return nil
		}
		purpose, ok := w.consumeChallenge([16]byte(o.ChallengeNonce))
		if !ok {
			w.log("  attest refused: challenge unknown")
			return nil
		}
		r := opstore.AttestRequest{Session: bytes.Clone(o.ChallengeNonce), Ledger: pmPos(o.Ledger),
			Reattest: purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST}
		if o.Checkpoint != nil {
			cp := pmPos(*o.Checkpoint)
			r.Checkpoint = &cp
		}
		res, err := a.s.Attest(w.ctx, r)
		if errors.Is(err, opstore.ErrContinuityPending) {
			if err := a.s.EstablishContinuity(w.ctx); err != nil {
				w.fail("continuity before the first attestation: %v", err)
			}
			res, err = a.s.Attest(w.ctx, r)
		}
		if err != nil {
			w.log("  attest refused: %v", err)
			return nil
		}
		if res.Quarantined != "" {
			w.log("  attest quarantined the store: %s", res.Quarantined)
			w.stat("agent-quarantined-" + string(res.Quarantined))
			return nil
		}
		if res.First {
			if js, _ := a.s.Journal().State(); js == journal.StateUnattested {
				if err := a.s.EstablishContinuity(w.ctx); err != nil {
					w.fail("continuity after the first attestation: %v", err)
				}
			}
			if !a.s.Recovered() {
				if _, err := a.s.Recover(w.ctx); err != nil {
					w.fail("recover after the first attestation: %v", err)
				}
			}
			// The enrolment sync of the start-up: a store whose active
			// enrolment is older than the agent's certificate (a store
			// rolled back across a re-enrolment) re-enrols to it.
			if v, err := a.s.HostView(w.ctx, 0); err == nil && (v.Enrolment == nil || !bytes.Equal(v.Enrolment.ID, w.cert)) &&
				v.LastStartEpoch < a.start {
				if err := a.s.Reenrol(w.ctx, opstore.Enrolment{HostID: pmHostID, ID: bytes.Clone(w.cert), ProviderUID: pmProviderUID,
					InstallID: pmInstall, StartEpoch: a.start}); err != nil {
					w.log("  enrolment sync failed: %v", err)
				}
			}
			w.refreshAccepted()
		}
		w.log("  attest applied at %d (re-attest %v)", r.Ledger.Seq, r.Reattest)
		w.stat("attest-applied")
		if r.Reattest {
			w.stat("reattest-applied")
		}
		return pmAttestReply{applied: true, session: [16]byte(o.ChallengeNonce)}
	case "mark":
		o := m.mark
		if !bytes.Equal(o.EnrolmentID, w.cert) {
			return nil
		}
		res, err := a.s.LedgerMark(w.ctx, opstore.LedgerMarkRequest{Session: bytes.Clone(o.AttestationID), Ledger: pmPos(o.Ledger)})
		if err != nil {
			w.log("  mark refused: %v", err)
			return nil
		}
		if res.Outcome == opstore.LedgerMarkQuarantined {
			w.log("  mark quarantined the store: %s", res.Quarantined)
			w.stat("agent-quarantined-" + string(res.Quarantined))
			return nil
		}
		w.log("  mark %d outcome %d", o.Ledger.Seq, res.Outcome)
		return nil
	case "retire":
		o := m.retire
		id := o.Identity
		res, err := a.s.RetireIfUnknown(w.ctx, opstore.RetireRequest{Session: bytes.Clone(o.AttestationID), Op: opstore.Request{
			ProviderUID: id.ProviderUID, EnrolmentID: id.EnrolmentID, OpID: id.OpID, VMUID: id.VMUID, BindingID: id.BindingID,
			Kind: o.Kind, Attempt: id.Attempt, IntentDigest: o.IntentDigest,
			MintEpoch: opstore.Epoch{Counter: id.MintEpoch.Counter, Holder: id.MintEpoch.Holder}}})
		if err != nil {
			w.log("  retire refused: %v", err)
			return nil
		}
		w.log("  retire outcome %d", res.Outcome)
		return res
	case "op":
		return w.deliverOp(m.op, st)
	}
	return nil
}

// pmOpReply is the agent's reply to an operation delivery.
type pmOpReply struct {
	d       opstore.Decision
	refused bool
}

// deliverOp is the agent server's operation path: refused while the store
// is lost, awaits attestation or is freezing; otherwise admission, and the
// runner's first Execute.
func (w *pmWorld) deliverOp(r opstore.Request, st pmAgentState) any {
	a := w.agent
	if st.lost || !st.attested || a.s.Freezing() {
		w.log("  op %x refused: agent not attested or lost", r.OpID[:4])
		return pmOpReply{refused: true}
	}
	d, err := a.s.Admit(w.ctx, r)
	if err != nil {
		w.log("  op %x admit error: %v", r.OpID[:4], err)
		return nil
	}
	ref := opstore.OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}
	if d.Outcome == opstore.OutcomeAccepted || d.Outcome == opstore.OutcomeKnown {
		if d.Op.AcceptedSeq != 0 {
			w.accSeq[hex.EncodeToString(r.OpID)] = d.Op.AcceptedSeq
		}
	}
	if d.Outcome == opstore.OutcomeAccepted {
		k := hex.EncodeToString(r.OpID)
		w.acceptEv[k] = append(w.acceptEv[k], pmAcceptEv{step: w.stepNo, seq: d.Op.AcceptedSeq})
	}
	if d.Outcome == opstore.OutcomeKnown && d.Op.State == opstore.StateAccepted {
		a.s.DepositRequest(ref, r.RequestBytes)
	}
	w.log("  op %x outcome %d refusal %v state %v", r.OpID[:4], d.Outcome, d.Refusal, d.Op.State)
	if d.Outcome != opstore.OutcomeRefused && d.Op.State == opstore.StateAccepted {
		w.execute(ref, r.RequestBytes)
	}
	return pmOpReply{d: d}
}

func (w *pmWorld) execute(ref opstore.OpRef, payload []byte) {
	_, err := w.agent.s.Execute(w.ctx, ref, payload)
	switch {
	case err == nil:
		w.stat("execute-ran")
	case errors.Is(err, opstore.ErrAwaitingLedger):
		w.stat("execute-parked")
	case errors.Is(err, opstore.ErrAwaitingRequest):
		w.stat("execute-awaiting-request")
	default:
		msg := err.Error()
		if i := strings.IndexAny(msg, ":0123456789"); i > 0 {
			msg = msg[:i]
		}
		w.stat("execute-other-" + strings.ReplaceAll(strings.TrimSpace(msg), " ", "_"))
	}
}

// agentTick runs the agent's runners: every accepted operation is tried.
func (w *pmWorld) agentTick() {
	if !w.agent.up {
		return
	}
	for _, at := range w.attempts {
		ref := opstore.OpRef{EnrolmentID: at.enrol, OpID: at.opID}
		v, ok, err := w.agent.s.Lookup(w.ctx, ref)
		if err != nil || !ok || v.State != opstore.StateAccepted || v.Tombstoned {
			continue
		}
		w.execute(ref, nil)
	}
}

// ---------------------------------------------------------------------------
// The attestation driver (modelled steps, real library).

func (w *pmWorld) holdOf(m *pmMgr, err error) {
	var h *HostLedgerHoldError
	if errors.As(err, &h) {
		m.lastHold, m.persistent = h.Reason, h.Persistent
		w.stat("hold-" + h.Reason)
	}
}

// evidence signs, with the stand-in host key of the current enrolment, a
// host answer carrying the store's real facts, verifies it and returns its
// ledger evidence.
func (w *pmWorld) evidence(v opstore.HostView) hostidentity.LedgerEvidence {
	h := pmHost(w.provider, w.enrol[0]-0xe0)
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	pos := append(append(append([]byte(nil), v.Position.StoreID[:]...), binary.BigEndian.AppendUint64(nil, v.Position.Seq)...),
		v.Position.Head[:]...)
	c := &providerv1.AnswerContext{Nonce: nonce, ProviderUid: pmProviderUID, HostId: pmHostID, InstallId: h.trust.InstallID,
		EnrolmentId: bytes.Clone(w.enrol), ChainPosition: pos, AgentState: providerv1.AgentState_AGENT_STATE_UNATTESTED,
		AgentTime: timestamppb.New(w.now), LedgerVersionEcho: hostidentity.LedgerVersion}
	if v.CheckpointSeq > 0 {
		c.CheckpointSeq, c.CheckpointHead = v.CheckpointSeq, bytes.Clone(v.CheckpointHead[:])
	}
	l := &providerv1.HostLedgerState{RequestedVersion: hostidentity.LedgerVersion, Version: hostidentity.StoreAttestedLedgerVersion,
		State:                  providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION,
		StoreOriginEnrolmentId: bytes.Clone(v.OriginEnrolmentID), StoreEnrolmentCount: v.EnrolmentActivations}
	if a := v.Attested; a.EverAttested {
		l.StoreEverAttested, l.StoreFirstAttestedSeq, l.StoreAttestedRecordSeq = true, a.FirstSeq, a.RecordSeq
		l.StoreFirstAttestedEnrolmentId = bytes.Clone(a.FirstEnrolmentID)
	}
	body := &providerv1.HostAnswer{Context: c, AgentApiVersion: 6, GrantVersions: []uint32{2},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY, Ledger: l}
	exp := h.trust.expectation(w.provider, pmHostID, hostidentity.BodyTypeHostAnswer, nonce, w.now)
	exp.LedgerVersion = hostidentity.LedgerVersion
	va, err := hostidentity.VerifyAnswer(blobOf(h.signRaw(pmT{}, body)), exp)
	if err != nil {
		w.fail("the stand-in host answer does not verify: %v", err)
	}
	ev, err := va.LedgerEvidence()
	if err != nil {
		w.fail("ledger evidence: %v", err)
	}
	return ev
}

// drive is one heartbeat of the attestation driver of manager m.
func (w *pmWorld) drive(m *pmMgr) {
	m.ready = false
	m.ha.updateLedger(w.provider, pmHostID, func(s *hostLedgerState) { s.ready = false })
	st, ok := w.agentState()
	if !ok {
		return
	}
	if !bytes.Equal(w.cert, w.enrol) || (st.attested && !bytes.Equal(w.agentActiveEnrolment(st), w.enrol)) {
		// The agent's answers do not verify under the host's current
		// enrolment (a re-enrolment in progress, or restored trust).
		w.stat("drive-enrolment-mismatch")
		return
	}
	w.lastAnswer[string(w.hostUID)] = w.now
	store := st.view.Position.StoreID
	rec, err := m.l.LoadBound(w.ctx, w.provider, pmHostID, store)
	switch {
	case errors.Is(err, ErrHostLedgerNotFound):
		ev := w.evidence(st.view)
		m.l.Observe(w.provider, pmHostID, ev)
		var res HostLedgerInitResult
		res, err = m.l.Initialise(w.ctx, w.provider, pmHostID, HostLedgerInit{StoreID: store, Evidence: ev})
		rec = res.Record
		if err == nil {
			w.log("  m%d initialised the ledger (%s), archived %d", m.epoch, res.Source, len(res.Archived))
			w.stat("init-" + string(res.Source))
			if len(res.Archived) > 0 {
				w.stat("archived-earlier-store")
			}
		}
	case errors.Is(err, ErrHostLedgerNotBound):
		rec, err = m.l.Rebind(w.ctx, w.provider, pmHostID, store)
		if err == nil {
			w.log("  m%d rebound the ledger", m.epoch)
			w.stat("rebind")
		}
	}
	if err != nil || rec == nil {
		w.holdOf(m, err)
		w.log("  m%d drive: ledger not usable: %v", m.epoch, err)
		return
	}
	switch {
	case st.lost:
		m.ha.holdQuarantined(w.ctx, w.provider, pmHostID, store)
		m.lastHold, m.persistent = LedgerReasonAgentQuarantined, true // a lost store is lost for good
		return
	case !st.attested:
		w.attest(m, rec, providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST)
		return
	}
	if rec.HasPendingSession(st.session) {
		if rec, err = m.l.Adopt(w.ctx, w.provider, pmHostID, store, st.session, LedgerEvidenceHostAnswer); err != nil {
			w.holdOf(m, err)
			return
		}
		w.log("  m%d adopted the agent's session from a host answer", m.epoch)
		w.stat("adopt-host-answer")
	}
	if rec.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested && rec.AttestationID == st.session {
		if m.l.heartbeatDue(w.provider, pmHostID) {
			got, err := m.l.Raise(w.ctx, w.provider, pmHostID, store, []HostLedgerRaise{{Session: st.session,
				Position:   HostLedgerPosition{Seq: st.view.Position.Seq, Head: st.view.Position.Head},
				Checkpoint: HostLedgerPosition{Seq: st.view.CheckpointSeq, Head: st.view.CheckpointHead}, Heartbeat: true}})
			if err != nil {
				w.holdOf(m, err)
				return
			}
			rec = got
		}
		if st.mark.Seq < rec.Position.Seq {
			// The driver's own step: the heartbeat token, then (without an
			// own write of the ledger as it is) the ledger written again,
			// then the mark signed from that write.
			if own := m.l.ownRecord(store); own == nil || own.WriteCounter != rec.WriteCounter {
				w.stat("reassert")
			}
			if err := m.ha.markBehind(w.ctx, w.provider, w.auth, pmHostID, w.hostTrust(), rec); err != nil {
				w.holdOf(m, err)
				w.stat("mark-behind-not-sent")
			}
		}
		w.markReady(m, store, st)
		return
	}
	if w.now.Sub(m.lastReattest) >= reattestInterval {
		m.lastReattest = w.now
		w.attest(m, rec, providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST)
	}
}

// markReady is the driver's readiness check: a fresh bound read, Attested in
// the agent's session, the agent READY, no hold.
func (w *pmWorld) markReady(m *pmMgr, store [16]byte, st pmAgentState) {
	if !st.ready {
		return
	}
	rec, err := m.l.LoadBound(w.ctx, w.provider, pmHostID, store)
	if err != nil || rec.Archived || rec.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested ||
		rec.AttestationID != st.session || m.l.HeldReason(w.provider, pmHostID) != "" {
		return
	}
	m.ready, m.readySession, m.readyStore, m.readyGen, m.readyHost = true, rec.AttestationID, rec.StoreID, rec.Generation, w.hostUID
	// The driver's readiness, as the real markReady records it.
	m.ha.updateLedger(w.provider, pmHostID, func(s *hostLedgerState) {
		s.ready, s.session, s.store, s.generation = true, rec.AttestationID, rec.StoreID, rec.Generation
	})
	m.lastHold, m.persistent = "", false
}

// attest is the driver's attestation: challenge, begin, sign from the write.
func (w *pmWorld) attest(m *pmMgr, rec *HostLedgerRecord, purpose providerv1.ChallengePurpose) {
	st, _ := w.agentState()
	if st.attested && rec.HasPendingSession(st.session) {
		if _, err := m.l.Adopt(w.ctx, w.provider, pmHostID, rec.StoreID, st.session, LedgerEvidenceChallengeAnswer); err != nil {
			w.holdOf(m, err)
		}
		w.stat("adopt-challenge-answer")
		return
	}
	nonce, ok := w.challenge(purpose)
	if !ok {
		return
	}
	lp := LedgerSessionAttest
	if purpose == providerv1.ChallengePurpose_CHALLENGE_PURPOSE_REATTEST &&
		rec.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
		lp = LedgerSessionReattest
	}
	begun, err := m.l.BeginSession(w.ctx, w.provider, pmHostID, rec.StoreID, nonce, lp,
		HostLedgerPosition{Seq: st.view.Position.Seq, Head: st.view.Position.Head})
	if err != nil {
		w.holdOf(m, err)
		w.log("  m%d begin session refused: %v", m.epoch, err)
		return
	}
	w.log("  m%d begun session (purpose %d) at %d", m.epoch, purpose, begun.Position.Seq)
	w.reply = nil
	if _, err := m.ha.sendAttest(w.ctx, w.provider, w.auth, pmHostID, w.hostTrust(), begun, nonce); err != nil {
		w.log("  m%d sendAttest refused: %v", m.epoch, err)
		return
	}
	if r, ok := w.reply.(pmAttestReply); ok && r.applied && m.alive {
		if _, err := m.l.Adopt(w.ctx, w.provider, pmHostID, rec.StoreID, r.session, LedgerEvidenceAuthorityAnswer); err != nil {
			w.holdOf(m, err)
			return
		}
		w.stat("adopt-authority-answer")
	}
}

// mark signs a ledger mark from rec through the real signing point.
func (w *pmWorld) mark(m *pmMgr, rec *HostLedgerRecord) {
	if _, err := m.ha.sendLedgerMark(w.ctx, w.provider, w.auth, pmHostID, w.hostTrust(), rec); err != nil {
		w.stat("mark-not-signed")
	}
}

// ---------------------------------------------------------------------------
// The VirtualMachine controller's attempts (modelled markers, real rules).

func (w *pmWorld) prepared(at *pmAttempt) contracts.PreparedOperation {
	t := w.hostTrust()
	return contracts.PreparedOperation{Kind: contracts.OperationKindCreate,
		Identity: contracts.OperationAttemptIdentity{ProviderUID: pmProviderUID, HostID: pmHostID, EnrolmentID: bytes.Clone(at.enrol),
			Epoch: contracts.OperationEpoch{Counter: t.Enrolment.Epoch.Counter, Holder: t.Enrolment.Epoch.Holder}, VMUID: at.vm,
			BindingID: bytes.Clone(at.binding), OpID: bytes.Clone(at.opID), Attempt: 1,
			MintEpoch: contracts.OperationEpoch{Counter: at.mint, Holder: pmInstall}},
		RequestBytes: at.request, DigestSalt: at.salt, LedgerStoreID: bytes.Clone(at.store[:]), LedgerGeneration: at.gen}
}

func (w *pmWorld) request(at *pmAttempt, expectKnown bool) opstore.Request {
	return opstore.Request{ProviderUID: pmProviderUID, EnrolmentID: bytes.Clone(at.enrol), OpID: bytes.Clone(at.opID), VMUID: at.vm,
		BindingID: bytes.Clone(at.binding), Kind: providerv1.OperationKind_OPERATION_KIND_CREATE, Attempt: 1,
		IntentDigest: operation.IntentDigest(at.salt, at.request), MintEpoch: opstore.Epoch{Counter: at.mint, Holder: pmInstall},
		Actor: "provider/model", ExpectKnown: expectKnown, RequestBytes: bytes.Clone(at.request)}
}

// prepare prepares a new attempt under the ledger m is ready in.
func (w *pmWorld) prepare(m *pmMgr) {
	if !m.ready || len(w.attempts) >= pmMaxAttempts {
		return
	}
	n := len(w.attempts) + 1
	op, bind, salt := w.id16(), w.id16(), w.id16()
	at := &pmAttempt{n: n, opID: op[:], vm: fmt.Sprintf("vm-%d", n), binding: bind[:], enrol: bytes.Clone(w.enrol),
		mint: w.agent.start, salt: append(salt[:], salt[:]...), request: []byte(fmt.Sprintf("create vm-%d", n)),
		store: m.readyStore, gen: m.readyGen, phase: infravirtrigaudiov1beta1.OperationAttemptPhasePrepared}
	w.attempts = append(w.attempts, at)
	w.log("  m%d prepared attempt %d (op %x)", m.epoch, n, at.opID[:4])
}

// readyFor reports whether m may sign an operation grant now, and checks
// invariant 4 for it against the durable ledger object of the store m is
// ready in: bound to the current Host object, not archived, Attested in
// the ready session.
func (w *pmWorld) readyFor(m *pmMgr) bool {
	if !m.ready || w.down {
		return false
	}
	// signedSend's own check: the ledger is read again just before the
	// grant (the real ledgerStillReady).
	if err := m.ha.ledgerStillReady(w.ctx, w.provider, pmHostID); err != nil {
		m.ready = false
		w.stat("send-refused-ledger-changed")
		return false
	}
	w.checkDurable("an operation grant", m.readyStore, w.hostUID, func(r *HostLedgerRecord) string {
		if r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested || r.AttestationID != m.readySession {
			return "not Attested in the ready session"
		}
		return ""
	})
	return true
}

// sendAttempt signs and sends (or re-sends) at at grant version 2.
func (w *pmWorld) sendAttempt(m *pmMgr, at *pmAttempt) {
	if !at.inFlight() || !w.readyFor(m) {
		return
	}
	expectKnown := false
	if at.receipt != nil {
		rec, err := m.l.LoadBound(w.ctx, w.provider, pmHostID, at.store)
		expectKnown = err != nil || !ledgerContinuous(w.prepared(at), rec) || receiptLedgered(rec, at.receipt)
	}
	if at.phase == infravirtrigaudiov1beta1.OperationAttemptPhasePrepared {
		at.phase = infravirtrigaudiov1beta1.OperationAttemptPhaseSent
	}
	w.stat("signed-op")
	w.log("  m%d sends attempt %d (expect_known %v)", m.epoch, at.n, expectKnown)
	w.send(pmMsg{kind: "op", op: w.request(at, expectKnown)})
	r, ok := w.reply.(pmOpReply)
	if !ok || !m.alive {
		return
	}
	if r.refused {
		return
	}
	if r.d.Refusal == opstore.RefusalExpectKnownUnknown {
		// The marker holds a receipt that counts as ledgered and the store
		// has no record of the operation (a store rolled back below it, or
		// a ledger restored below it): it may have run, so it is held as
		// outcome unknown, never retired nor run as new.
		w.stat("expect-known-refused")
		w.close(at, "OutcomeUnknown", "expect_known_unknown")
		return
	}
	switch {
	case r.d.Outcome == opstore.OutcomeRefused && r.d.Refusal.DefiniteNotPerformed():
		w.close(at, "NotPerformed", r.d.Refusal.String())
	case r.d.Acceptance != nil:
		w.receipt(m, at, r.d.Acceptance.Session, r.d.Acceptance.Position, true)
	}
}

// receipt is receipt verification against the ledger (store and adopted
// session) and the marker write; raiseNow raises the ledger and the mark at
// once (otherwise a later step does).
func (w *pmWorld) receipt(m *pmMgr, at *pmAttempt, session []byte, pos journal.Position, raiseNow bool) {
	// A receipt verifies only under the host's trust of the attempt's
	// enrolment, and counts only for the host's current store (the store of
	// its last verified answer).
	if at.receipt != nil || !at.inFlight() || !bytes.Equal(at.enrol, w.enrol) || pos.StoreID != w.agent.s.Position().StoreID {
		return
	}
	rec, err := m.l.LoadBound(w.ctx, w.provider, pmHostID, pos.StoreID)
	if err != nil || rec.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested ||
		rec.AttestationID != [16]byte(session) || rec.StoreID != at.store {
		w.stat("receipt-not-recorded")
		return
	}
	at.receipt = &infravirtrigaudiov1beta1.OperationAttemptReceipt{StoreID: hex.EncodeToString(pos.StoreID[:]), Seq: int64(pos.Seq),
		Head: hex.EncodeToString(pos.Head[:]), AttestationID: hex.EncodeToString(session)}
	at.recPos = pos
	at.phase = infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted
	w.stat("receipt-recorded")
	if raiseNow && !w.chance(4) {
		w.raiseReceipts(m)
	}
}

// raiseReceipts raises the ledger over every recorded, unraised receipt of
// the ledger's current session, and the mark after it.
func (w *pmWorld) raiseReceipts(m *pmMgr) {
	byStore := map[[16]byte][]HostLedgerRaise{}
	var stores [][16]byte
	for _, at := range w.attempts {
		// A raise a reconcile could not take at once is taken later only
		// for the host's current store and enrolment (the real manager's
		// heartbeat raise covers it from the agent's answer).
		if at.receipt == nil || at.raised || !bytes.Equal(at.enrol, w.enrol) || at.recPos.StoreID != w.agent.s.Position().StoreID {
			continue
		}
		if _, ok := byStore[at.recPos.StoreID]; !ok {
			stores = append(stores, at.recPos.StoreID)
		}
		session, _ := hex.DecodeString(at.receipt.AttestationID)
		byStore[at.recPos.StoreID] = append(byStore[at.recPos.StoreID], HostLedgerRaise{Session: [16]byte(session),
			Position: HostLedgerPosition{Seq: at.recPos.Seq, Head: at.recPos.Head}})
	}
	for _, store := range stores {
		got, err := m.l.Raise(w.ctx, w.provider, pmHostID, store, byStore[store])
		if err != nil {
			w.holdOf(m, err)
			continue
		}
		for _, at := range w.attempts {
			if at.receipt != nil && !at.raised && at.recPos.StoreID == store {
				at.raised = true
			}
		}
		w.mark(m, got)
	}
}

func (w *pmWorld) close(at *pmAttempt, outcome, reason string) {
	at.outcome, at.reason = outcome, reason
	at.phase = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal
	if outcome == "Retired" {
		w.retired[at.key()] = true
		if _, ok := w.retireStep[at.key()]; !ok {
			w.retireStep[at.key()] = w.stepNo
		}
	}
	w.stat("closed-" + outcome + "-" + reason)
	w.log("  attempt %d closed %s (%s)", at.n, outcome, reason)
}

// verifiedOpAnswer is the verified answer the manager acts on for an
// operation answer view v of the agent in state st.
func (w *pmWorld) verifiedOpAnswer(at *pmAttempt, v opstore.OpAnswerView, st pmAgentState) hostidentity.VerifiedAnswer {
	state := providerv1.AgentState_AGENT_STATE_UNATTESTED
	if st.ready {
		state = providerv1.AgentState_AGENT_STATE_READY
	}
	opState := providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD
	if v.Found {
		opState = providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED
	}
	c := &providerv1.AnswerContext{AgentState: state, EnrolmentId: bytes.Clone(w.agentActiveEnrolment(st))}
	if st.attested {
		c.AttestationId = bytes.Clone(st.session[:])
	}
	return hostidentity.VerifiedAnswer{Context: c,
		Operation: &providerv1.OperationAnswer{OpId: bytes.Clone(at.opID), OpEnrolmentId: bytes.Clone(at.enrol), State: opState},
		Position:  hostidentity.ChainPosition{StoreID: v.Host.Position.StoreID, Seq: v.Host.Position.Seq, Head: v.Host.Position.Head}}
}

// poll asks the agent about at and acts on the answer.
func (w *pmWorld) poll(m *pmMgr, at *pmAttempt) {
	if !at.inFlight() || w.down {
		return
	}
	// An attempt prepared under a store a new store archived is lost with
	// it (storeLost).
	if rec, err := m.l.Load(w.ctx, w.provider, pmHostID, at.store); err == nil && rec.Archived {
		w.close(at, "OutcomeUnknown", outcomeStoreLost)
		return
	}
	st, ok := w.agentState()
	if !ok || st.lost || !bytes.Equal(w.agentActiveEnrolment(st), w.enrol) || !bytes.Equal(w.cert, w.enrol) {
		return
	}
	v, err := w.agent.s.AnswerOp(w.ctx, opstore.OpRef{EnrolmentID: at.enrol, OpID: at.opID}, 0)
	if err != nil {
		return
	}
	rec, err := m.l.LoadBound(w.ctx, w.provider, pmHostID, v.Host.Position.StoreID)
	if err != nil {
		w.holdOf(m, err)
		return
	}
	adopted := st.ready && rec.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested && rec.AttestationID == st.session
	if !adopted {
		return // no answer outside the adopted session
	}
	frozen := !bytes.Equal(at.enrol, w.agentActiveEnrolment(st))
	switch {
	case v.Found && v.Op.State == opstore.StateCompleted:
		w.close(at, "Succeeded", "")
	case v.Found && v.Op.State == opstore.StateRetired && !frozen && v.Op.Reason == opstore.ReasonNeverReceived:
		// A signed RETIRED for the active enrolment: the tombstone of a
		// retirement whose answer was lost. Covered, then closed.
		if err := m.ha.coverRetirement(w.ctx, w.provider, pmHostID, st.session, w.verifiedOpAnswer(at, v, st).Position); err != nil {
			return
		}
		w.tombSeq[at.key()] = v.Host.Position.Seq
		w.close(at, "Retired", retiredNeverReceived)
	case v.Found && (v.Op.State == opstore.StateRetired || v.Op.State == opstore.StateOutcomeUnknown ||
		v.Op.State == opstore.StateFailedDirty || v.Op.State == opstore.StateFailedClean):
		w.close(at, "OutcomeUnknown", "agent-"+v.Op.State.String())
	case v.Found && v.Op.State == opstore.StateAccepted:
		if at.receipt == nil && len(v.AcceptedHead) == 32 {
			w.receipt(m, at, st.session[:], journal.Position{StoreID: v.Host.Position.StoreID, Seq: v.Op.AcceptedSeq,
				Head: [32]byte(v.AcceptedHead)}, true)
		}
		if v.AwaitingRequest {
			w.sendAttempt(m, at)
		}
	case !v.Found && v.EnrolmentHeld:
		w.retire(m, at, v, st, rec, frozen)
	case !v.Found:
		// UNKNOWN_TO_AGENT: the store holds no record of the attempt's
		// enrolment (discarded, or a store rolled back across a
		// re-enrolment) and cannot say whether the operation ran: held as
		// outcome unknown, never retired.
		w.close(at, "OutcomeUnknown", "unknown_to_agent")
	}
}

// retire is retire-if-unknown (active enrolment) or the frozen-predecessor
// closure, on the real decision.
func (w *pmWorld) retire(m *pmMgr, at *pmAttempt, v opstore.OpAnswerView, st pmAgentState, rec *HostLedgerRecord, frozen bool) {
	va := w.verifiedOpAnswer(at, v, st)
	verdict, why := decideRetire(retireEvidence{attempt: &infravirtrigaudiov1beta1.OperationAttempt{Phase: at.phase, Receipt: at.receipt},
		receiptBound: true, prepared: w.prepared(at), answer: va, ledger: rec, frozen: frozen})
	if verdict != retireAllowed {
		w.stat("retire-verdict-" + strings.ReplaceAll(why, " ", "_"))
		switch {
		case verdict == retireResend && !frozen:
			// The re-send rules apply (with expect_known when the receipt
			// counts as ledgered).
			w.sendAttempt(m, at)
		case verdict == retireResend && frozen:
			// A predecessor's attempt the closure does not close (its
			// receipt counts as ledgered, or it is not continuous with
			// the ledger) is held, as every predecessor attempt is.
			w.close(at, "OutcomeUnknown", "predecessor_held")
		}
		return
	}
	if frozen {
		w.close(at, "Retired", retiredUnknownToFrozenEnrolment)
		return
	}
	if at.receipt != nil {
		// A receipt that does not count as ledgered (a fenced session's
		// receipt above where its session was replaced).
		w.stat("retire-with-unledgered-receipt")
	}
	w.reply = nil
	w.retireStore, w.retireGen = at.store, at.gen
	if _, err := m.ha.sendRetire(w.ctx, w.auth, rec, w.prepared(at), st.session); err != nil {
		w.fail("sendRetire refused an attempt decideRetire allowed: %v", err)
	}
	res, ok := w.reply.(opstore.RetireResult)
	if !ok || !m.alive {
		return
	}
	if res.Outcome != opstore.RetireApplied && res.Outcome != opstore.RetireAlreadyApplied {
		return
	}
	st2, ok := w.agentState()
	if !ok || !st2.attested || st2.session != st.session {
		return
	}
	pos := hostidentity.ChainPosition{StoreID: st2.view.Position.StoreID, Seq: st2.view.Position.Seq, Head: st2.view.Position.Head}
	if err := m.ha.coverRetirement(w.ctx, w.provider, pmHostID, st.session, pos); err != nil {
		w.log("  retirement not covered: %v", err)
		return
	}
	w.tombSeq[at.key()] = pos.Seq
	w.close(at, "Retired", retiredNeverReceived)
}

// ---------------------------------------------------------------------------
// etcd.

func (w *pmWorld) etcdSnapshot() {
	var objs []client.Object
	secs := &corev1.SecretList{}
	_ = w.cli.List(w.ctx, secs)
	for i := range secs.Items {
		objs = append(objs, secs.Items[i].DeepCopy())
	}
	hosts := &infravirtrigaudiov1beta1.HostList{}
	_ = w.cli.List(w.ctx, hosts)
	for i := range hosts.Items {
		objs = append(objs, hosts.Items[i].DeepCopy())
	}
	snap := pmEtcd{objs: objs, stores: map[string]HostStoreState{}, trusts: map[string]HostTrust{}, hostUID: w.hostUID,
		enrol: bytes.Clone(w.enrol), store: w.agent.s.Position().StoreID}
	for k, v := range w.trust.stores {
		c := v
		c.LedgerIndex = v.LedgerIndex.clone()
		snap.stores[k] = c
	}
	for k, v := range w.trust.trusts {
		snap.trusts[k] = v
	}
	for _, at := range w.attempts {
		snap.attempts = append(snap.attempts, at.clone())
	}
	w.etcd = append(w.etcd, snap)
	if len(w.etcd) > 3 {
		w.etcd = w.etcd[1:]
	}
	w.log("etcd snapshot (%d objects, %d attempts)", len(objs), len(snap.attempts))
}

// etcdRestore restores a snapshot of everything; restart restarts the
// manager (a new epoch) as it would come back after the restore.
func (w *pmWorld) etcdRestore(restart bool) {
	if len(w.etcd) == 0 {
		return
	}
	snap := w.etcd[w.rnd.IntN(len(w.etcd))]
	objs := make([]client.Object, 0, len(snap.objs))
	hadLedger := false
	for _, o := range snap.objs {
		c, ok := o.DeepCopyObject().(client.Object)
		if !ok {
			w.fail("a snapshot object is not a client object")
		}
		objs = append(objs, c)
		if o.GetName() == hostLedgerSecretName(w.agent.s.Position().StoreID) {
			hadLedger = true
		}
	}
	if !hadLedger {
		// Restored to before the store's ledger existed: the store was
		// attested against a ledger the manager no longer holds (the
		// fresh-store procedure applies, a documented hold).
		w.faults[faultRestoreAcross] = true
	}
	w.cli.Client = fake.NewClientBuilder().WithScheme(w.scheme).WithObjects(objs...).Build()
	w.trust.stores = map[string]HostStoreState{}
	for k, v := range snap.stores {
		c := v
		c.LedgerIndex = v.LedgerIndex.clone()
		w.trust.stores[k] = c
	}
	w.trust.trusts = map[string]HostTrust{}
	for k, v := range snap.trusts {
		w.trust.trusts[k] = v
	}
	if !bytes.Equal(snap.enrol, w.enrol) || snap.store != w.agent.s.Position().StoreID {
		w.faults[faultRestoreAcross] = true
	}
	w.hostUID, w.enrol = snap.hostUID, bytes.Clone(snap.enrol)
	w.attempts = nil
	for _, at := range snap.attempts {
		w.attempts = append(w.attempts, at.clone())
	}
	store := w.agent.s.Position().StoreID
	ev := pmRestoreEv{step: w.stepNo, store: store}
	if r := w.durable(store); r != nil {
		ev.ledgerSeq = r.Position.Seq
	}
	w.restores = append(w.restores, ev)
	if restart {
		w.faults[faultEtcdRestore] = true
		for _, m := range w.alive() {
			m.alive = false
		}
		w.newMgr()
	} else {
		w.faults[faultEtcdRestoreNoRestart] = true
	}
	w.log("etcd restore (manager restart %v): %d attempts", restart, len(w.attempts))
}

// partialRestore puts an older copy of the current store's ledger object
// back, alone.
func (w *pmWorld) partialRestore() {
	store := w.agent.s.Position().StoreID
	for i := len(w.etcd) - 1; i >= 0; i-- {
		for _, o := range w.etcd[i].objs {
			sec, ok := o.(*corev1.Secret)
			if !ok || sec.Name != hostLedgerSecretName(store) {
				continue
			}
			cur := &corev1.Secret{}
			if err := w.cli.Get(w.ctx, types.NamespacedName{Namespace: pmEnrolNS, Name: sec.Name}, cur); err != nil {
				return
			}
			cur.Data = map[string][]byte{hostLedgerDataKey: bytes.Clone(sec.Data[hostLedgerDataKey])}
			if err := w.cli.Update(w.ctx, cur); err == nil {
				w.faults[faultPartialRestore] = true
				w.log("partial restore of the ledger object")
			}
			return
		}
	}
}

func (w *pmWorld) deleteLedger() {
	store := w.agent.s.Position().StoreID
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: pmEnrolNS, Name: hostLedgerSecretName(store)}}
	if err := w.cli.Delete(w.ctx, sec); err == nil {
		w.faults[faultLedgerDeleted] = true
		w.log("the ledger object was deleted")
	}
}

// ---------------------------------------------------------------------------
// Host lifecycle.

// reenrol re-enrols the host on the same store: the predecessor is frozen.
func (w *pmWorld) reenrol() {
	a := w.agent
	if !a.up || w.enrolN >= pmMaxEnrolments {
		return
	}
	if js, _ := a.s.Journal().State(); js != journal.StateReady || !a.s.Recovered() {
		return
	}
	// A host is re-enrolled once the manager knows its store (a store
	// re-enrolled before its first contact may never take a ledger by first
	// contact: the fresh-store procedure applies, a documented hold).
	sec := &corev1.Secret{}
	if err := w.cli.Get(w.ctx, types.NamespacedName{Namespace: pmEnrolNS, Name: hostLedgerSecretName(a.s.Position().StoreID)}, sec); err != nil {
		return
	}
	w.enrolN++
	w.enrol = pmEnrolment(w.enrolN)
	w.cert = w.enrol
	w.setTrust()
	a.start++
	if err := a.s.Reenrol(w.ctx, opstore.Enrolment{HostID: pmHostID, ID: bytes.Clone(w.enrol), ProviderUID: pmProviderUID,
		InstallID: pmInstall, StartEpoch: a.start}); err != nil {
		w.log("re-enrol failed: %v", err)
		return
	}
	w.stat("reenrol")
	w.log("re-enrolment on the same store (enrolment %d)", w.enrolN)
}

// reinstall wipes the agent: a new store under a new enrolment.
func (w *pmWorld) reinstall() {
	if w.enrolN >= pmMaxEnrolments {
		return
	}
	w.enrolN++
	w.enrol = pmEnrolment(w.enrolN)
	w.cert = w.enrol
	w.setTrust()
	w.agentFresh()
	w.stat("reinstall")
	w.log("agent reinstalled: new store under enrolment %d", w.enrolN)
}

// replaceHost replaces the Host object with a new one of the same name and
// identity (the old one deleted).
func (w *pmWorld) replaceHost() {
	old := &infravirtrigaudiov1beta1.Host{}
	if err := w.cli.Get(w.ctx, types.NamespacedName{Namespace: pmNamespace, Name: pmHostID}, old); err != nil {
		return
	}
	if err := w.cli.Delete(w.ctx, old); err != nil {
		return
	}
	w.hostN++
	w.hostUID = types.UID(fmt.Sprintf("00000000-0000-4000-8000-%012d", w.hostN))
	if err := w.cli.Create(w.ctx, pmHostObject(w.hostUID)); err != nil {
		w.fail("create the replacement Host: %v", err)
	}
	// The pins Secret of a new Host object starts afresh (no ledger index).
	delete(w.trust.stores, pmHostID)
	w.setTrust()
	w.stat("host-replaced")
	w.log("Host replaced (new object %d)", w.hostN)
}

// ---------------------------------------------------------------------------
// The schedule.

func (w *pmWorld) pickMgr() *pmMgr {
	al := w.alive()
	if len(al) == 0 || w.down {
		return nil
	}
	return al[w.rnd.IntN(len(al))]
}

func (w *pmWorld) pickAttempt() *pmAttempt {
	var fl []*pmAttempt
	for _, at := range w.attempts {
		if at.inFlight() {
			fl = append(fl, at)
		}
	}
	if len(fl) == 0 {
		return nil
	}
	return fl[w.rnd.IntN(len(fl))]
}

// step runs one random event.
func (w *pmWorld) step(i, forcedEpoch int) {
	if i == forcedEpoch {
		// Every run has at least two manager epochs.
		for _, m := range w.alive() {
			m.alive = false
		}
		w.down = false
		w.newMgr()
		return
	}
	r := w.rnd.IntN(130)
	m := w.pickMgr()
	switch {
	case r < 24:
		if m != nil {
			w.log("m%d drive", m.epoch)
			w.drive(m)
		}
	case r < 31:
		if m != nil {
			w.prepare(m)
		}
	case r < 43:
		if at := w.pickAttempt(); m != nil && at != nil {
			w.sendAttempt(m, at)
		}
	case r < 53:
		if at := w.pickAttempt(); m != nil && at != nil {
			w.log("m%d polls attempt %d", m.epoch, at.n)
			w.poll(m, at)
		}
	case r < 57:
		if m != nil {
			w.raiseReceipts(m)
		}
	case r < 66:
		if len(w.queue) > 0 {
			j := w.rnd.IntN(len(w.queue))
			msg := w.queue[j]
			w.queue = append(w.queue[:j:j], w.queue[j+1:]...)
			w.log("deliver delayed %s", msg.kind)
			w.deliver(msg)
		}
	case r < 68:
		if len(w.queue) > 0 {
			j := w.rnd.IntN(len(w.queue))
			w.queue = append(w.queue[:j:j], w.queue[j+1:]...)
			w.log("drop delayed message")
		}
	case r < 76:
		w.agentTick()
	case r < 79:
		if w.agent.up {
			w.agentRestart()
		}
	case r < 82:
		w.agentSnapshot()
	case r < 84:
		if w.enabled[faultAgentRollback] {
			w.agentRollback(true)
		}
	case r < 85:
		if w.enabled[faultStoreOnlyRollback] && w.chance(3) {
			w.agentRollback(false)
		}
	case r < 88:
		w.etcdSnapshot()
	case r < 90:
		if w.enabled[faultEtcdRestore] {
			w.etcdRestore(true)
		}
	case r < 91:
		if w.enabled[faultEtcdRestoreNoRestart] && w.chance(2) {
			w.etcdRestore(false)
		}
	case r < 92:
		if w.enabled[faultPartialRestore] && w.chance(2) {
			w.partialRestore()
		}
	case r < 93:
		if w.enabled[faultLedgerDeleted] && w.chance(3) {
			w.deleteLedger()
		}
	case r < 97:
		// A leader change: a second writer starts while the first still
		// runs; later the first is deposed.
		al := w.alive()
		if len(al) == 1 && !w.down {
			w.newMgr()
			w.stat("two-writers")
		} else if len(al) > 1 {
			al[0].alive = false
			w.log("manager epoch %d deposed", al[0].epoch)
		}
	case r < 99:
		if !w.down {
			for _, m := range w.alive() {
				m.alive = false
			}
			w.newMgr()
		}
	case r < 101:
		// A downgrade window: an older build that never signs grant
		// version 2 nor touches the ledger; the upgrade back starts a new
		// epoch.
		if !w.down {
			for _, m := range w.alive() {
				m.alive = false
			}
			w.down = true
			w.stat("downgrade")
			w.log("downgrade window starts")
		} else {
			w.down = false
			w.newMgr()
		}
	case r < 103:
		if w.chance(2) {
			w.reenrol()
		}
	case r < 104:
		if w.chance(2) {
			w.reinstall()
		}
	case r < 105:
		if w.chance(2) {
			w.replaceHost()
		}
	default:
		w.now = w.now.Add(time.Duration(1+w.rnd.IntN(90)) * time.Second)
	}
}

// checkSafety checks invariants 1 and 3 on the ground truth. A violation
// is exempted only when the record of the run explains it by the faults
// outside the protocol's claim (explainedRetired, explainedDuplicate);
// every other one fails the run.
func (w *pmWorld) checkSafety() {
	keys := make([]string, 0, len(w.effects))
	for k := range w.effects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := w.effects[k]
		if w.retired[k] && n > 0 {
			if w.explainedRetired(k) {
				w.flag("retired-with-effect (its record or its tombstone dropped by an agent store+anchor rollback, and the ledger lowered below it by an etcd restore)")
			} else {
				w.fail("invariant 1 violated: op %s was closed Retired and had %d effects", k[:8], n)
			}
		}
		if n > 1 {
			if w.explainedDuplicate(k) {
				w.flag("duplicate-effect (an agent store+anchor rollback to a snapshot taken before the earlier effect, between the effects)")
			} else {
				w.fail("invariant 3 violated: op %s had %d effects", k[:8], n)
			}
		}
	}
}

// explainedRetired reports whether op k's effect under a retirement is
// explained by an etcd restore and an agent store+anchor rollback together,
// in one of the two ways they defeat it:
//
//   - the record was lost: the op was accepted and ran, an agent
//     store+anchor rollback to a snapshot below its acceptance then dropped
//     its record, and an etcd restore after it ran and before the retirement lowered the
//     store's ledger below its acceptance (so the rolled-back store passed
//     its attestation);
//   - the tombstone was lost: after the retirement, an agent store+anchor
//     rollback to a snapshot below the retirement's covered position
//     dropped the tombstone, an etcd restore lowered the ledger below that
//     position (so the store passed its attestation), and the op ran only
//     after both.
func (w *pmWorld) explainedRetired(k string) bool {
	rs, ok := w.retireStep[k]
	if !ok {
		return false
	}
	if ts, ok := w.tombSeq[k]; ok {
		for _, rb := range w.rollbacks {
			if rb.step <= rs || rb.snapSeq >= ts {
				continue
			}
			for _, re := range w.restores {
				if re.step > rs && re.ledgerSeq < ts && !w.effectBetween(k, 0, max(rb.step, re.step)) {
					return true
				}
			}
		}
	}
	for _, a := range w.acceptEv[k] {
		for _, rb := range w.rollbacks {
			if rb.step <= a.step || rb.snapSeq >= a.seq || rb.step > rs || !w.effectBetween(k, a.step, rb.step) {
				continue
			}
			// The restore must come after the op's first effect of this
			// accept (an earlier one, before the op existed, explains
			// nothing) and before the retirement.
			first := w.firstEffectFrom(k, a.step)
			for _, re := range w.restores {
				if re.step > first && re.step < rs && re.ledgerSeq < a.seq {
					return true
				}
			}
		}
	}
	return false
}

// firstEffectFrom returns the step of op k's first effect at or after step
// from (effectBetween said there is one).
func (w *pmWorld) firstEffectFrom(k string, from int) int {
	first := -1
	for _, e := range w.effectSteps[k] {
		if e >= from && (first < 0 || e < first) {
			first = e
		}
	}
	return first
}

// effectBetween reports whether op k had an effect at a step in [from, to).
func (w *pmWorld) effectBetween(k string, from, to int) bool {
	for _, e := range w.effectSteps[k] {
		if e >= from && e < to {
			return true
		}
	}
	return false
}

// explainedDuplicate reports whether every repeated effect of op k is
// explained: between each two consecutive effects an agent store+anchor
// rollback restored a snapshot taken before the earlier one.
func (w *pmWorld) explainedDuplicate(k string) bool {
	es := w.effectSteps[k]
	for i := 1; i < len(es); i++ {
		found := false
		for _, rb := range w.rollbacks {
			if rb.step > es[i-1] && rb.step <= es[i] && rb.snapStep <= es[i-1] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(es) > 1
}

func (w *pmWorld) flag(what string) {
	for _, f := range w.flagged {
		if f == what {
			return
		}
	}
	w.flagged = append(w.flagged, what)
}

// lenient reports whether the run injected a fault that may hold the host
// for good.
func (w *pmWorld) lenient() bool {
	for f := range w.faults {
		if pmLenient[f] {
			return true
		}
	}
	return false
}

// quiesce stops the adversary and drives the system until nothing changes.
func (w *pmWorld) quiesce() {
	w.steps = -1
	w.log("--- quiescent phase ---")
	if w.down {
		w.down = false
		w.newMgr()
	}
	al := w.alive()
	for i := 0; i+1 < len(al); i++ {
		al[i].alive = false
	}
	if len(w.alive()) == 0 {
		w.newMgr()
	}
	for _, msg := range w.queue {
		w.deliver(msg)
	}
	w.queue = nil
	if !w.agent.up {
		w.agentOpen()
	}
	m := w.alive()[0]
	for round := 0; round < pmQuiesceRounds; round++ {
		w.stepNo++
		m.lastHold, m.persistent = "", false
		w.now = w.now.Add(61 * time.Second)
		w.agentTick()
		w.drive(m)
		w.drive(m)
		w.raiseReceipts(m)
		w.drive(m)
		for _, at := range w.attempts {
			if !at.inFlight() {
				continue
			}
			w.poll(m, at)
			if at.inFlight() && at.receipt == nil {
				w.sendAttempt(m, at)
			}
		}
		w.agentTick()
		// The probe: one more drive, past every pacing interval, on its
		// own. A transient hold counts as holding the host only when this
		// last drive meets it again (a hold met earlier in the round, and
		// cleared since, hides nothing).
		w.now = w.now.Add(61 * time.Second)
		m.lastHold, m.persistent = "", false
		w.drive(m)
		m.probeHold = m.lastHold
		w.checkSafety()
		if w.settled(m) {
			w.stats["quiesce-rounds"] += round + 1
			return
		}
	}
}

// settled reports whether every attempt is closed or the host is held.
func (w *pmWorld) settled(m *pmMgr) bool {
	for _, at := range w.attempts {
		if at.inFlight() {
			return false
		}
	}
	st, ok := w.agentState()
	if !ok {
		return false
	}
	return st.lost || (m.probeHold != "" && m.persistent) || (m.ready && st.ready)
}

// checkLiveness is invariant 5.
func (w *pmWorld) checkLiveness() {
	m := w.alive()[0]
	st, _ := w.agentState()
	var open []string
	for _, at := range w.attempts {
		if at.inFlight() {
			open = append(open, fmt.Sprintf("attempt %d (%s)", at.n, at.phase))
		}
	}
	// Held: the agent's store is lost, or the last drive of the final
	// round met a hold (persistent, or transient and met again then).
	held := st.lost || m.probeHold != ""
	switch {
	case !w.lenient() && (held || !m.ready || !st.ready || len(open) > 0):
		w.fail("invariant 5 violated: wedge in a run with no lasting fault: agent lost %v ready %v, manager ready %v hold %q, open %v (faults %v)",
			st.lost, st.ready, m.ready, m.probeHold, open, w.faultList())
	case len(open) > 0 && !held && !w.faults[faultRestoreAcross] && !w.faults[faultLedgerDeleted]:
		w.fail("invariant 5 violated: attempts left open with nothing holding the host: %v (faults %v)", open, w.faultList())
	case !st.lost && m.probeHold != "" && !pmDocumentedHolds[m.probeHold]:
		w.fail("invariant 5 violated: the host is held for an undocumented reason %q", m.probeHold)
	}
	switch {
	case st.lost:
		w.stat("end-agent-quarantined")
	case m.probeHold != "":
		w.stat("end-held-" + m.probeHold)
	case len(open) > 0:
		w.stat("end-open-after-restore-across-enrolment")
	default:
		w.stat("end-attested-all-closed")
		if w.faults[faultEtcdRestore] && !w.lenient() {
			w.stat("end-recovered-after-etcd-restore")
		}
	}
}

func (w *pmWorld) faultList() []string {
	var out []string
	for f := range w.faults {
		out = append(out, f.String())
	}
	sort.Strings(out)
	return out
}

// pmResult is what one run reports.
type pmResult struct {
	stats   map[string]int
	flagged []string
	faults  []string
	epochs  int
}

// runPM runs one seed with steps random steps (the quiescent phase after).
func runPM(seed uint64, steps int, root string) (res pmResult, trace []string, err error) {
	dir, derr := os.MkdirTemp(root, "pm-")
	if derr != nil {
		return pmResult{}, nil, derr
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var w *pmWorld
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(*pmFailure)
			if !ok {
				f = &pmFailure{msg: fmt.Sprintf("panic: %v\n%s", r, debug.Stack())}
			}
			err = f
			if w != nil {
				w.dumpLedgers()
				trace = w.trace
				if w.agent != nil && w.agent.s != nil {
					_ = w.agent.s.Close()
				}
			}
		}
	}()
	w = newPMWorld(seed, steps, filepath.Join(dir, "agent"))
	// The first contact: the manager attests the fresh store.
	for i := 0; i < 4; i++ {
		w.drive(w.mgrs[0])
	}
	forced := 1 + w.rnd.IntN(max(steps-1, 1))
	for i := 0; i < steps; i++ {
		w.stepNo = i + 1
		w.step(i, forced)
		w.checkSafety()
	}
	w.quiesce()
	w.checkSafety()
	w.checkLiveness()
	if w.epochs < 2 {
		w.fail("the run had %d manager epochs", w.epochs)
	}
	_ = w.agent.s.Close()
	return pmResult{stats: w.stats, flagged: w.flagged, faults: w.faultList(), epochs: w.epochs}, w.trace, nil
}

// dumpLedgers appends the ledger objects to the trace (on a failure).
func (w *pmWorld) dumpLedgers() {
	defer func() { _ = recover() }()
	secs := &corev1.SecretList{}
	_ = w.cli.List(w.ctx, secs)
	for i := range secs.Items {
		sec := &secs.Items[i]
		if !isHostLedgerSecret(sec) {
			continue
		}
		rec, err := decodeHostLedger(sec.Data[hostLedgerDataKey])
		if err != nil {
			continue
		}
		var enrols []string
		for _, e := range rec.GenerationEnrolments {
			enrols = append(enrols, fmt.Sprintf("%x", e[:1]))
		}
		w.log("ledger %s: label enrolment %.2s, host %s, seq %d, state %s, archived %v, generation enrolments %v, counter %d",
			rec.StorePrefix(), sec.Labels[LabelHostLedgerEnrolment], rec.HostUID[len(rec.HostUID)-3:], rec.Position.Seq,
			rec.AttestationState, rec.Archived, enrols, rec.WriteCounter)
	}
	w.log("agent store %s, manager enrolment %x, certificate %x", storeIDPrefix(w.agent.s.Position().StoreID), w.enrol[:1], w.cert[:1])
}

// shrinkPM finds the shortest prefix of seed's schedule that still fails.
func shrinkPM(seed uint64, root string) (int, []string, error) {
	lo, hi := 0, pmSteps
	_, trace, err := runPM(seed, hi, root)
	if err == nil {
		return hi, nil, nil
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if _, t, e := runPM(seed, mid, root); e != nil {
			hi, trace, err = mid, t, e
		} else {
			lo = mid + 1
		}
	}
	return hi, trace, err
}

// pmTrace formats a trace: repeated lines folded, at most the last 200.
func pmTrace(trace []string) string {
	var out []string
	for i := 0; i < len(trace); {
		j := i + 1
		for j < len(trace) && trace[j] == trace[i] {
			j++
		}
		if j-i > 1 {
			out = append(out, fmt.Sprintf("%s (x%d)", trace[i], j-i))
		} else {
			out = append(out, trace[i])
		}
		i = j
	}
	if len(out) > 200 {
		out = append([]string{fmt.Sprintf("... %d earlier lines", len(out)-200)}, out[len(out)-200:]...)
	}
	return strings.Join(out, "\n  ")
}

func pmRoot(t *testing.T) string {
	if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
		if d, err := os.MkdirTemp("/dev/shm", "vrtg-protocol-model-"); err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(d) })
			return d
		}
	}
	return t.TempDir()
}

// TestHostLedgerProtocolModel runs the whole-protocol model (see the file
// comment).
func TestHostLedgerProtocolModel(t *testing.T) {
	root := pmRoot(t)
	pmPrepareHosts()
	if v, err := strconv.ParseUint(os.Getenv(pmSeedEnv), 10, 64); err == nil {
		steps, trace, err := shrinkPM(v, root)
		if err != nil {
			t.Fatalf("seed %d fails after %d steps: %v\ntrace:\n  %s", v, steps, err, pmTrace(trace))
		}
		if pmListFlagged {
			res, trace, _ := runPM(v, pmSteps, root)
			t.Logf("seed %d passes; flagged %v; faults %v\ntrace:\n  %s", v, res.flagged, res.faults, pmTrace(trace))
		}
		return
	}
	seeds := pmDefaultSeeds
	if testing.Short() {
		seeds = pmShortSeeds
	}
	if v, err := strconv.Atoi(os.Getenv(pmSeedsEnv)); err == nil && v > 0 {
		seeds = v
	}
	workers := runtime.GOMAXPROCS(0)
	if v, err := strconv.Atoi(os.Getenv(pmWorkersEnv)); err == nil && v > 0 {
		workers = v
	}
	workers = min(workers, seeds)
	defer debug.SetGCPercent(debug.SetGCPercent(400))
	start := time.Now()
	var (
		mu      sync.Mutex
		total   = map[string]int{}
		flagged = map[string]int{}
		faults  = map[string]int{}
		epochs  int
		done    atomic.Int64
		failed  atomic.Bool
		// failures counts the failing seeds; a run goes on past a failure
		// (the first is shrunk and traced, the next few named) and stops
		// after pmMaxFailures.
		failures atomic.Int64
	)
	const pmMaxFailures = 20
	var wg sync.WaitGroup
	for wk := 0; wk < workers; wk++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seed := uint64(wk); seed < uint64(seeds) && failures.Load() < pmMaxFailures; seed += uint64(workers) {
				res, _, err := runPM(seed, pmSteps, root)
				if err != nil {
					failures.Add(1)
					if failed.CompareAndSwap(false, true) {
						steps, trace, serr := shrinkPM(seed, root)
						if serr == nil {
							serr = err
						}
						t.Errorf("seed %d fails (reproduce with %s=%d; shortest failing prefix %d steps): %v\ntrace:\n  %s",
							seed, pmSeedEnv, seed, steps, serr, pmTrace(trace))
					} else {
						t.Errorf("seed %d fails too: %v", seed, err)
					}
					continue
				}
				mu.Lock()
				for k, v := range res.stats {
					total[k] += v
				}
				for _, f := range res.flagged {
					flagged[f]++
					if pmListFlagged && flagged[f] <= 10 {
						t.Logf("flagged seed %d: %s", seed, f)
					}
				}
				for _, f := range res.faults {
					faults[f]++
				}
				epochs += res.epochs
				mu.Unlock()
				if n := done.Add(1); n%100000 == 0 {
					t.Logf("%d seeds done in %v", n, time.Since(start))
				}
			}
		}()
	}
	wg.Wait()
	keys := make([]string, 0, len(total))
	for k := range total {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %s: %d", k, total[k])
	}
	t.Logf("%d failing seeds", failures.Load())
	t.Logf("%d seeds in %v (%d workers), %d manager epochs in all; runs per injected fault %v; flagged runs (reported, not failed) %v; exercised:%s",
		seeds, time.Since(start), workers, epochs, faults, flagged, b.String())
	for _, must := range []string{"op-started", "attest-applied", "signed-mark", "signed-retire", "closed-Retired-" + retiredNeverReceived,
		"adopt-host-answer", "two-writers", "downgrade", "rebind", "reenrol", "reinstall", "reattest-applied", "archived-earlier-store",
		"closed-Retired-" + retiredUnknownToFrozenEnrolment, "end-recovered-after-etcd-restore", "reassert"} {
		if total[must] == 0 && seeds >= pmDefaultSeeds && !failed.Load() {
			t.Errorf("no run exercised %s", must)
		}
	}
}
