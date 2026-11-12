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

// Command journal-torture drives the agent journal (internal/agent/journal)
// for durability tests on real block devices: torn-write replay, injected
// I/O errors and power cuts. It is a test tool, not part of any release.
//
//	journal-torture run    -dir D -ack SINK [-n N] [-dm-mark NAME] ...
//	journal-torture verify -dir D (-ack FILE | -acked N) [-probe] [-reopen]
//	journal-torture lwmarks -log DEV
//
// run commits one operation per transaction (key op-<i>, a deterministic
// value derived from i, and a "last" key overwritten each time). Only after
// Update returns nil is operation i acknowledged: optionally as a
// dm-log-writes mark "ack:<i>", then as a line "ACK <i> <seq>" written to the
// ack sink and made durable (fsync for a file or block device, tcdrain for a
// serial line). The sink must live outside the volume under test.
//
// verify reopens the store and classifies it, printing one JSON line:
//
//	ok              every acknowledged operation is present and intact
//	detected        the store is lost (quarantined) with a reason
//	undetected_loss the store opens as usable and an acknowledged operation
//	                is missing or altered (a test failure)
//	availability    the store opened usable but a probe commit failed
//	error           Open returned an error
//
// Exit codes: 0 ok, 2 detected, 1 undetected_loss, 3 availability, 4 error,
// 5 anomaly.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

const (
	bucketOps = "ops"
	keyLast   = "last"
	opPrefix  = "op-"
)

func opKey(i uint64) []byte { return []byte(fmt.Sprintf("%s%010d", opPrefix, i)) }

// opValue is the deterministic value of operation i: 40..1640 bytes, so that
// pages split and the store grows.
func opValue(i uint64) []byte {
	n := 40 + int((i*131)%1600)
	out := make([]byte, 0, n+32)
	seed := sha256.Sum256([]byte("journal-torture-" + strconv.FormatUint(i, 10)))
	for len(out) < n {
		out = append(out, seed[:]...)
		seed = sha256.Sum256(seed[:])
	}
	return out[:n]
}

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: journal-torture run|verify|lwmarks [flags]")
		os.Exit(64)
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "verify":
		code = cmdVerify(os.Args[2:])
	case "lwmarks":
		code = cmdLWMarks(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		code = 64
	}
	os.Exit(code)
}

func storeOptions(dir string, rotate int64, checkpoint uint64) journal.Options {
	return journal.Options{
		Dir:               dir,
		Buckets:           []string{bucketOps},
		AnchorRotateBytes: rotate,
		CheckpointEvery:   checkpoint,
		OpenTimeout:       5 * time.Second,
	}
}

// ackSink is the out-of-band acknowledgement log.
type ackSink struct {
	f   *os.File
	tty bool
}

func openSink(path string) (*ackSink, error) {
	if path == "" || path == "-" {
		return &ackSink{f: os.Stdout}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOCTTY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ack sink %s: %w", path, err)
	}
	_, terr := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return &ackSink{f: f, tty: terr == nil}, nil
}

// emit writes line and returns only when it is durable outside the store.
func (a *ackSink) emit(line string) error {
	if _, err := io.WriteString(a.f, line+"\n"); err != nil {
		return fmt.Errorf("write ack: %w", err)
	}
	if a.f == os.Stdout {
		return nil
	}
	if a.tty {
		// tcdrain: wait until the bytes left the UART. The Go runtime's
		// preemption signals interrupt it; retry.
		for {
			err := unix.IoctlSetInt(int(a.f.Fd()), unix.TCSBRK, 1)
			if err == nil {
				return nil
			}
			if !errors.Is(err, unix.EINTR) {
				return fmt.Errorf("drain ack: %w", err)
			}
		}
	}
	if err := a.f.Sync(); err != nil {
		return fmt.Errorf("sync ack: %w", err)
	}
	return nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("dir", "", "state directory")
	ack := fs.String("ack", "-", "ack sink: file, block device or serial line ('-' = stdout)")
	n := fs.Uint64("n", 0, "operations to commit (0 = until an error or a kill)")
	dmMark := fs.String("dm-mark", "", "dm-log-writes device to mark after each acknowledged op")
	rotate := fs.Int64("rotate", 4096, "anchor log rotation size in bytes")
	checkpoint := fs.Uint64("checkpoint", 16, "commits between chain checkpoints")
	interval := fs.Duration("interval", 0, "pause between commits")
	keepOpen := fs.Bool("hold-on-error", false, "after a commit error, stay alive (store open) until killed")
	_ = fs.Parse(args)
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "run: -dir is required")
		return 64
	}
	sink, err := openSink(*ack)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	ctx := context.Background()
	st, err := journal.Open(ctx, storeOptions(*dir, *rotate, *checkpoint))
	if err != nil {
		_ = sink.emit("OPENERR " + oneLine(err))
		return 4
	}
	defer func() { _ = st.Close() }()
	if s, r := st.State(); s == journal.StateLost {
		_ = sink.emit("LOST-AT-OPEN " + string(r))
		return 2
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		_ = sink.emit("CONTINUITY-ERR " + oneLine(err))
		return 3
	}
	var next uint64 = 1
	if err := st.View(ctx, func(tx *journal.Tx) error {
		if v := tx.Get(bucketOps, []byte(keyLast)); len(v) == 8 {
			next = binary.BigEndian.Uint64(v) + 1
		}
		return nil
	}); err != nil {
		_ = sink.emit("VIEW-ERR " + oneLine(err))
		return 4
	}
	_ = sink.emit(fmt.Sprintf("START %d seq=%d", next, st.Position().Seq))
	for i := next; *n == 0 || i < next+*n; i++ {
		pos, err := st.Update(ctx, func(tx *journal.Tx) error {
			if err := tx.Put(bucketOps, opKey(i), opValue(i)); err != nil {
				return err
			}
			return tx.Put(bucketOps, []byte(keyLast), be64(i))
		})
		if err != nil {
			state, reason := st.State()
			_ = sink.emit(fmt.Sprintf("ERR %d state=%s reason=%s err=%s", i, state, reason, oneLine(err)))
			if *keepOpen {
				select {}
			}
			if state == journal.StateLost {
				return 2
			}
			return 4
		}
		if *dmMark != "" {
			if out, err := exec.Command("dmsetup", "message", *dmMark, "0", "mark", "ack:"+strconv.FormatUint(i, 10)).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "dm mark %d: %v %s\n", i, err, out)
				return 4
			}
		}
		if err := sink.emit(fmt.Sprintf("ACK %d %d", i, pos.Seq)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 4
		}
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}
	return 0
}

var ackRE = regexp.MustCompile(`ACK (\d+) (\d+)`)

// readAcked returns the highest acknowledged op in an ack log.
func readAcked(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open ack log: %w", err)
	}
	defer func() { _ = f.Close() }()
	var max uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		for _, m := range ackRE.FindAllStringSubmatch(sc.Text(), -1) {
			v, _ := strconv.ParseUint(m[1], 10, 64)
			if v > max {
				max = v
			}
		}
	}
	return max, sc.Err()
}

// report is verify's output.
type report struct {
	Verdict      string `json:"verdict"`
	Acked        uint64 `json:"acked"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Seq          uint64 `json:"seq"`
	Present      uint64 `json:"present"`
	Missing      uint64 `json:"missing"`
	Altered      uint64 `json:"altered"`
	FirstBad     uint64 `json:"first_bad,omitempty"`
	Extra        uint64 `json:"extra"`
	Last         uint64 `json:"last"`
	ReadErr      string `json:"read_err,omitempty"`
	Probe        string `json:"probe,omitempty"`
	ReopenState  string `json:"reopen_state,omitempty"`
	ReopenReason string `json:"reopen_reason,omitempty"`
	Err          string `json:"err,omitempty"`
	// Tail is what the anchor log looked like before Open, against the
	// store position after Open: "equal", "store_ahead_1" (an unacknowledged
	// last commit re-anchored by the tail rule), "torn_tail" (a partial last
	// anchor record cut off), "none" (no anchor log), or "other".
	Tail      string `json:"tail,omitempty"`
	AnchorSeq uint64 `json:"anchor_seq"`
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "", "state directory")
	ack := fs.String("ack", "", "ack log to read the acknowledged ops from")
	acked := fs.Int64("acked", -1, "ops 1..N acknowledged (instead of -ack)")
	probe := fs.Bool("probe", false, "on a usable store: establish continuity and commit a probe")
	reopen := fs.Bool("reopen", false, "after the check, close and reopen and report the state again")
	_ = fs.Parse(args)
	r := report{}
	switch {
	case *acked >= 0:
		r.Acked = uint64(*acked)
	case *ack != "":
		a, err := readAcked(*ack)
		if err != nil {
			r.Verdict, r.Err = "error", oneLine(err)
			return emitReport(r)
		}
		r.Acked = a
	default:
		fmt.Fprintln(os.Stderr, "verify: -ack or -acked is required")
		return 64
	}
	ctx := context.Background()
	opts := storeOptions(*dir, 0, 0)
	torn, haveAnchor := false, false
	if raw, err := os.ReadFile(filepath.Join(*dir, journal.AnchorFile)); err == nil {
		haveAnchor = true
		if l, perr := journal.ParseAnchorLog(raw); perr == nil {
			torn = l.TornTail
			for _, rec := range l.Records {
				if rec.Type == journal.AnchorCommit {
					r.AnchorSeq = rec.Seq
				}
			}
		}
	}
	st, err := journal.Open(ctx, opts)
	if err != nil {
		r.Verdict, r.Err = "error", oneLine(err)
		return emitReport(r)
	}
	state, reason := st.State()
	r.State, r.Reason, r.Seq = state.String(), string(reason), st.Position().Seq
	switch {
	case !haveAnchor:
		r.Tail = "none"
	case torn:
		r.Tail = "torn_tail"
	case r.Seq == r.AnchorSeq:
		r.Tail = "equal"
	case r.Seq == r.AnchorSeq+1:
		r.Tail = "store_ahead_1"
	default:
		r.Tail = "other"
	}
	seen := map[uint64]bool{}
	verr := st.View(ctx, func(tx *journal.Tx) error {
		if v := tx.Get(bucketOps, []byte(keyLast)); len(v) == 8 {
			r.Last = binary.BigEndian.Uint64(v)
		}
		for i := uint64(1); i <= r.Acked; i++ {
			v := tx.Get(bucketOps, opKey(i))
			switch {
			case v == nil:
				r.Missing++
				if r.FirstBad == 0 {
					r.FirstBad = i
				}
			case !bytes.Equal(v, opValue(i)):
				r.Altered++
				if r.FirstBad == 0 {
					r.FirstBad = i
				}
			default:
				r.Present++
			}
			seen[i] = true
		}
		return tx.ForEach(bucketOps, []byte(opPrefix), func(k, _ []byte) error {
			i, perr := strconv.ParseUint(string(k[len(opPrefix):]), 10, 64)
			if perr == nil && i > r.Acked {
				r.Extra++
			}
			return nil
		})
	})
	if verr != nil {
		r.ReadErr = oneLine(verr)
	}
	// Re-read the state: a read can quarantine a corrupt store.
	state, reason = st.State()
	r.State, r.Reason = state.String(), string(reason)
	switch {
	case state == journal.StateLost:
		r.Verdict = "detected"
	case verr != nil:
		// Usable store that cannot be read: treat as a failure to classify.
		r.Verdict = "anomaly"
	case r.Missing > 0 || r.Altered > 0:
		r.Verdict = "undetected_loss"
	case r.Extra > 1:
		r.Verdict = "anomaly"
	default:
		r.Verdict = "ok"
	}
	if r.Verdict == "ok" && *probe {
		if err := st.EstablishContinuity(ctx); err != nil {
			r.Verdict, r.Probe = "availability", "continuity: "+oneLine(err)
		} else if _, err := st.Update(ctx, func(tx *journal.Tx) error {
			return tx.Put(bucketOps, []byte("probe"), be64(uint64(time.Now().UnixNano())))
		}); err != nil {
			r.Verdict, r.Probe = "availability", "commit: "+oneLine(err)
		} else {
			r.Probe = "ok"
		}
	}
	_ = st.Close()
	if *reopen {
		st2, err := journal.Open(ctx, opts)
		if err != nil {
			r.ReopenState = "open-error: " + oneLine(err)
		} else {
			s2, r2 := st2.State()
			r.ReopenState, r.ReopenReason = s2.String(), string(r2)
			_ = st2.Close()
			if r.Verdict == "detected" && s2 != journal.StateLost {
				r.Verdict = "anomaly" // a quarantine was lifted by a restart
			}
		}
	}
	return emitReport(r)
}

func emitReport(r report) int {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
	switch r.Verdict {
	case "ok":
		return 0
	case "detected":
		return 2
	case "undetected_loss":
		return 1
	case "availability":
		return 3
	case "anomaly":
		return 5
	}
	return 4
}

// dm-log-writes on-disk format (drivers/md/dm-log-writes.c).
const (
	lwMagic    = 0x6a736677736872
	lwFlagMark = 1 << 3
	lwFlagDisc = 1 << 2
)

// cmdLWMarks lists the mark entries of a dm-log-writes log with their entry
// index (0-based, in log order), one "index name" per line.
func cmdLWMarks(args []string) int {
	fs := flag.NewFlagSet("lwmarks", flag.ExitOnError)
	logDev := fs.String("log", "", "dm-log-writes log device")
	_ = fs.Parse(args)
	f, err := os.Open(*logDev)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, 28)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	if binary.LittleEndian.Uint64(hdr[0:]) != lwMagic {
		fmt.Fprintln(os.Stderr, "not a dm-log-writes log")
		return 4
	}
	nr := binary.LittleEndian.Uint64(hdr[16:])
	ss := int64(binary.LittleEndian.Uint32(hdr[24:]))
	off := ss
	ent := make([]byte, 32)
	w := bufio.NewWriter(os.Stdout)
	defer func() { _ = w.Flush() }()
	fmt.Fprintf(w, "# entries %d sectorsize %d\n", nr, ss)
	for idx := uint64(0); idx < nr; idx++ {
		if _, err := f.ReadAt(ent, off); err != nil {
			fmt.Fprintf(os.Stderr, "entry %d: %v\n", idx, err)
			return 4
		}
		nrSectors := binary.LittleEndian.Uint64(ent[8:])
		flags := binary.LittleEndian.Uint64(ent[16:])
		dataLen := binary.LittleEndian.Uint64(ent[24:])
		off += ss
		switch {
		case flags&lwFlagMark != 0:
			name := make([]byte, dataLen)
			if _, err := f.ReadAt(name, off-ss+32); err != nil {
				fmt.Fprintf(os.Stderr, "mark %d: %v\n", idx, err)
				return 4
			}
			fmt.Fprintf(w, "%d %s\n", idx, string(bytes.TrimRight(name, "\x00")))
		case flags&lwFlagDisc != 0:
			// A discard carries no data.
		default:
			// Data follows the entry header; nr_sectors counts log sectors
			// (as replay-log reads it).
			off += int64(nrSectors) * ss
		}
	}
	return 0
}

func oneLine(err error) string {
	if err == nil {
		return ""
	}
	return regexp.MustCompile(`\s+`).ReplaceAllString(err.Error(), " ")
}
