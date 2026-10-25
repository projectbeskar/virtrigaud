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

package libvirt

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// How far a routed call got on its host (R25).
//
// A host failure is not one answer. The manager reads HOST_UNAVAILABLE as a
// DEFINITE "nothing was done" (it re-places, re-sends and clears markers on
// it), so it may be answered only when no command of the call ran on the host.
// Once a command that may change the host has been sent, a dropped connection
// says nothing about what that command did. A leased host's call therefore
// carries a hostProgress in its context (withHostConn), and the commands every
// routed core runs — through the host connection or straight on its
// VirshProvider, both end in the funnels below — record into it:
//
//   - answered: a host command of this call came back with an answer of its
//     own (it succeeded, or ended with an exit status), so the host was
//     reached;
//   - wrote: a command that may change the host (anything not on the read-only
//     lists below) was SENT: its session was opened (opened), or it ended with
//     an answer. It is recorded when the command starts, not when it ends: a
//     command whose session dropped may still be running. A command whose SSH
//     dial or session open failed ran nothing and is not a write.
//
// hostOpRPCError turns a transport failure into the answer the record
// supports: nothing answered and nothing written -> HOST_UNAVAILABLE; only
// reads answered -> "not performed"; anything written -> unknown outcome. The
// routed ImportDisk keeps its own, finer record (importProgress).
//
// A command is a read only when it is on a short, fixed list of commands that
// cannot change a host. Everything else counts as a write, so a command
// missing from the list makes an answer less specific (unknown instead of
// "not performed"), never wrong.

// hostProgress records how far one routed call got on its host. It is safe for
// concurrent use; the zero value is empty. A nil *hostProgress ignores
// everything (a call that is not tracked: every single-host call).
type hostProgress struct {
	mu       sync.Mutex
	answered bool
	wrote    bool
	// pendingWrites counts the commands that may change the host that were
	// begun and have not ended; opened promotes them to wrote.
	pendingWrites int
}

// hostProgressKey is the context key of the call's hostProgress.
type hostProgressKey struct{}

// withHostProgress returns ctx carrying a new, empty hostProgress, and it.
func withHostProgress(ctx context.Context) (context.Context, *hostProgress) {
	p := &hostProgress{}
	return context.WithValue(ctx, hostProgressKey{}, p), p
}

// hostProgressFrom returns the hostProgress ctx carries, or nil.
func hostProgressFrom(ctx context.Context) *hostProgress {
	p, ok := ctx.Value(hostProgressKey{}).(*hostProgress)
	if !ok {
		return nil
	}
	return p
}

// hostDownStage says how far a call got on its host when the host stopped
// answering.
type hostDownStage int

const (
	// hostDownNothingRan: no command of the call was answered and none that may
	// change the host was sent — the lease, the dial, or the very first command
	// found the connection dead. Nothing was done.
	hostDownNothingRan hostDownStage = iota
	// hostDownReadsOnly: the host answered at least one command, and no
	// command that may change it was sent. Nothing was done.
	hostDownReadsOnly
	// hostDownWriteSent: a command that may change the host was sent. It may
	// have completed, or still be running: the outcome is unknown.
	hostDownWriteSent
)

// stage returns how far the call got. A nil hostProgress (a call that was not
// tracked) is assumed to have sent a command that may change the host.
func (p *hostProgress) stage() hostDownStage {
	if p == nil {
		return hostDownWriteSent
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.wrote:
		return hostDownWriteSent
	case p.answered:
		return hostDownReadsOnly
	}
	return hostDownNothingRan
}

// begin records that the command args (as runVirshCommand takes them: a virsh
// subcommand and its arguments, or "!" and a host argv) is about to be sent,
// and returns the function that records its result. It does nothing on a nil
// receiver.
func (p *hostProgress) begin(args []string) func(error) {
	if p == nil {
		return func(error) {}
	}
	return p.beginCommand(commandMayWrite(args))
}

// beginStream is begin for a command that streams data (the S3 export's read
// of the flattened disk, a copy onto the host): always a write.
func (p *hostProgress) beginStream() func(error) {
	return p.beginCommand(true)
}

// beginCommand is begin for a command already classified. A write is pending
// until its session is opened (opened) or it ends with an answer; a write whose
// dial or session open failed never ran anything and leaves wrote unset.
func (p *hostProgress) beginCommand(write bool) func(error) {
	if p == nil {
		return func(error) {}
	}
	if write {
		p.mu.Lock()
		p.pendingWrites++
		p.mu.Unlock()
	}
	return func(err error) {
		answer := err == nil || hostAnswered(err) || sshExited(err)
		p.mu.Lock()
		defer p.mu.Unlock()
		if write {
			p.pendingWrites--
			if answer {
				p.wrote = true
			}
		}
		if answer {
			p.answered = true
		}
	}
}

// opened records that a session to the host was opened (or, for a local
// connection, that a process is about to start): a pending command that may
// change the host may now be running. It does nothing on a nil receiver.
func (p *hostProgress) opened() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingWrites > 0 {
		p.wrote = true
	}
}

// sshExited reports whether err is a remote command that ended with an exit
// status (a streamed command's failure carries no *VirshError).
func sshExited(err error) bool {
	var exit *ssh.ExitError
	return stderrors.As(err, &exit)
}

// commandMayWrite reports whether the command args — as runVirshCommand takes
// them — may change the host: false only for a virsh subcommand or a host
// command on the read-only lists.
func commandMayWrite(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if args[0] == "!" {
		return hostArgvMayWrite(args[1:])
	}
	return !virshReadOnly(args)
}

// virshReadOnlySubcommands are the virsh subcommands that only read when they
// are run without arguments that set something (see virshReadOnly).
var virshReadOnlySubcommands = map[string]bool{
	"list": true, "dominfo": true, "domstate": true, "dumpxml": true, "domblklist": true, "domblkinfo": true,
	"domblkstat": true, "domiflist": true, "domifaddr": true, "domuuid": true, "domid": true, "domname": true,
	"dommemstat": true, "domjobinfo": true, "domstats": true, "guestinfo": true, "cpu-stats": true,
	"vcpucount": true, "vcpuinfo": true, "snapshot-list": true, "snapshot-dumpxml": true, "snapshot-info": true,
	"snapshot-parent": true, "pool-list": true, "pool-info": true, "pool-dumpxml": true,
	"vol-list": true, "vol-info": true, "vol-dumpxml": true, "vol-path": true, "vol-name": true, "vol-key": true,
	"net-list": true, "net-info": true, "net-dumpxml": true, "nodeinfo": true, "version": true, "uri": true,
	"hostname": true, "capabilities": true, "domcapabilities": true, "sysinfo": true,
}

// virshReadOnly reports whether the virsh command args (a subcommand, after any
// "-c <uri>" pair) is a read-only subcommand. snapshot-current is not on the
// list: it can set the current snapshot. dommemstat reads unless it is asked to
// set the balloon statistics period (--period, with --config, --live or
// --current).
func virshReadOnly(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "--connect" {
			i++
			continue
		}
		if !virshReadOnlySubcommands[args[i]] {
			return false
		}
		if args[i] == "dommemstat" {
			for _, a := range args[i+1:] {
				if strings.HasPrefix(a, "--period") || a == "--config" || a == "--live" || a == "--current" {
					return false
				}
			}
		}
		return true
	}
	return false
}

// readOnlyHostTools are host commands that only read, whatever their
// arguments.
var readOnlyHostTools = map[string]bool{
	"test": true, "stat": true, "ls": true, "readlink": true, "realpath": true, "df": true, "id": true,
	"uname": true, "getent": true,
}

// readOnlyHostScripts are the fixed `sh -c` scripts of the routed cores that
// only read the host (the disk and path probes). Each is a constant of this
// package, so the command is recognised by its text.
var readOnlyHostScripts = map[string]bool{
	existingPathsScript: true,
	backingKindScript:   true,
	chainMemberScript:   true,
	diskKindScript:      true,
	targetKindScript:    true,
	pathExistsScript:    true,
	pathPresentScript:   true,
	diskDirModeScript:   true,
	stagedHeadScript:    true,
	// The V12 overlay probe of a snapshot create (snapshot_overlay_collision.go).
	overlayCollisionScript: true,
}

// hostArgvMayWrite reports whether the host command argv may change the host.
// A leading `sudo -n` and `timeout [options] <duration>` are looked through.
func hostArgvMayWrite(argv []string) bool {
	for len(argv) > 0 && (argv[0] == "sudo" || argv[0] == sudoNonInteractive) {
		argv = argv[1:]
	}
	if len(argv) > 0 && argv[0] == "timeout" {
		i := 1
		for i < len(argv) && strings.HasPrefix(argv[i], "-") {
			i++
		}
		// i is the duration; the command follows it.
		if i+1 >= len(argv) {
			return true
		}
		return hostArgvMayWrite(argv[i+1:])
	}
	if len(argv) == 0 {
		return true
	}
	switch {
	case readOnlyHostTools[argv[0]]:
		return false
	case argv[0] == "virsh":
		return !virshReadOnly(argv[1:])
	case argv[0] == "qemu-img":
		return len(argv) < 2 || argv[1] != "info"
	case argv[0] == "sh":
		return len(argv) < 3 || argv[1] != "-c" || !readOnlyHostScripts[argv[2]]
	}
	return true
}
