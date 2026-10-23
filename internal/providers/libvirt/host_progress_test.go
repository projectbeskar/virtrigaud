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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCommandMayWrite pins which commands count as reads: only the short,
// fixed list of commands that cannot change a host. Everything else is a
// write, so a command missing from the list makes an answer less specific,
// never wrong.
func TestCommandMayWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		write bool
	}{
		"virsh list":                          {[]string{"list", "--all"}, false},
		"virsh dumpxml":                       {[]string{"dumpxml", "web", "--inactive"}, false},
		"virsh domstate":                      {[]string{"domstate", "web"}, false},
		"virsh snapshot-list":                 {[]string{"snapshot-list", "web", "--name"}, false},
		"virsh pool-dumpxml":                  {[]string{"pool-dumpxml", "default"}, false},
		"virsh with a connect URI":            {[]string{"-c", "qemu:///system", "dumpxml", "web"}, false},
		"virsh destroy":                       {[]string{"destroy", "web"}, true},
		"virsh undefine":                      {[]string{"undefine", "web"}, true},
		"virsh define":                        {[]string{"define", "/dev/stdin"}, true},
		"virsh metadata (the owner stamp)":    {[]string{"metadata", "--domain", "u", "--set", "x"}, true},
		"virsh snapshot-create-as":            {[]string{"snapshot-create-as", "web", "s"}, true},
		"virsh snapshot-revert":               {[]string{"snapshot-revert", "web", "s"}, true},
		"virsh pool-refresh":                  {[]string{"pool-refresh", "default"}, true},
		"virsh qemu-agent-command":            {[]string{"qemu-agent-command", "web", "{}"}, true},
		"an unknown virsh subcommand":         {[]string{"frobnicate"}, true},
		"no command":                          {nil, true},
		"host test":                           {[]string{"!", "test", "-r", "/dev/kvm"}, false},
		"host stat":                           {[]string{"!", "stat", "-c", "%s", "/p"}, false},
		"host qemu-img info":                  {[]string{"!", "qemu-img", "info", "--output=json", "/p"}, false},
		"host sudo qemu-img info":             {[]string{"!", "sudo", "-n", "qemu-img", "info", "-U", "/p"}, false},
		"host qemu-img info under timeout":    {[]string{"!", "timeout", "--kill-after=10s", "60s", "qemu-img", "info", "nfs://x"}, false},
		"host qemu-img convert":               {[]string{"!", "qemu-img", "convert", "-O", "qcow2", "a", "b"}, true},
		"host qemu-img convert under timeout": {[]string{"!", "timeout", "--kill-after=10s", "60s", "qemu-img", "convert", "a", "b"}, true},
		"host qemu-img create":                {[]string{"!", "qemu-img", "create", "-f", "raw", "nfs://x", "0"}, true},
		"host rm":                             {[]string{"!", "rm", "-f", "--", "/p"}, true},
		"host mkdir":                          {[]string{"!", "mkdir", "-p", "/p"}, true},
		"host sudo rm":                        {[]string{"!", "sudo", "rm", "-f", "/p"}, true},
		"host virsh as a host command":        {[]string{"!", "virsh", "-c", "qemu:///system", "list", "--all"}, false},
		"host virsh destroy":                  {[]string{"!", "virsh", "-c", "qemu:///system", "destroy", "web"}, true},
		"a read-only probe script":            {[]string{"!", "sh", "-c", existingPathsScript, "sh", "/p"}, false},
		"the disk kind probe":                 {[]string{"!", "sh", "-c", diskKindScript, "sh", "/p"}, false},
		"the chain member probe":              {[]string{"!", "sh", "-c", chainMemberScript, "sh", "/p", "/q"}, false},
		"the guard script":                    {[]string{"!", "sh", "-c", hostGuardScript, "sh", "/d", "/l", "/t", "flock"}, true},
		"a script that writes":                {[]string{"!", "sh", "-c", writeStdinToFileScript, "sh", "/p"}, true},
		"an unknown shell script":             {[]string{"!", "sh", "-c", "echo hi"}, true},
		"timeout with nothing after it":       {[]string{"!", "timeout", "10s"}, true},
		"host no command":                     {[]string{"!"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.write, commandMayWrite(tc.args))
		})
	}
}

// TestHostProgress_Stages: what a call records decides how far it got.
func TestHostProgress_Stages(t *testing.T) {
	transport := &VirshError{ExitCode: -1, Stderr: "ssh: handshake failed"}
	rejected := &VirshError{ExitCode: 1, Stderr: "error: nope"}
	libvirtdDown := &VirshError{ExitCode: 1, Stderr: "error: failed to connect to the hypervisor"}

	t.Run("nothing recorded", func(t *testing.T) {
		assert.Equal(t, hostDownNothingRan, (&hostProgress{}).stage())
	})
	t.Run("a read answered", func(t *testing.T) {
		p := &hostProgress{}
		p.begin([]string{"list", "--all"})(nil)
		assert.Equal(t, hostDownReadsOnly, p.stage())
	})
	t.Run("a read that the host answered with an error is still an answer", func(t *testing.T) {
		p := &hostProgress{}
		p.begin([]string{"dumpxml", "x"})(rejected)
		assert.Equal(t, hostDownReadsOnly, p.stage())
	})
	t.Run("a read that dropped is not an answer", func(t *testing.T) {
		p := &hostProgress{}
		p.begin([]string{"list", "--all"})(transport)
		assert.Equal(t, hostDownNothingRan, p.stage())
	})
	t.Run("libvirtd down is not an answer", func(t *testing.T) {
		p := &hostProgress{}
		p.begin([]string{"list", "--all"})(libvirtdDown)
		assert.Equal(t, hostDownNothingRan, p.stage())
	})
	t.Run("a cancelled call is not an answer", func(t *testing.T) {
		p := &hostProgress{}
		p.begin([]string{"list", "--all"})(&VirshError{ExitCode: -1, Cause: context.Canceled})
		assert.Equal(t, hostDownNothingRan, p.stage())
	})
	t.Run("a write is recorded when it is sent, before its result", func(t *testing.T) {
		p := &hostProgress{}
		done := p.begin([]string{"destroy", "web"})
		assert.Equal(t, hostDownWriteSent, p.stage(), "the session may drop with the command still running")
		done(transport)
		assert.Equal(t, hostDownWriteSent, p.stage())
	})
	t.Run("reads after a write stay a write stage", func(t *testing.T) {
		p := &hostProgress{}
		p.begin([]string{"destroy", "web"})(nil)
		p.begin([]string{"domstate", "web"})(transport)
		assert.Equal(t, hostDownWriteSent, p.stage())
	})
	t.Run("a stream is a write", func(t *testing.T) {
		p := &hostProgress{}
		p.beginStream()(errors.New("broken pipe"))
		assert.Equal(t, hostDownWriteSent, p.stage())
	})
	t.Run("an untracked call assumes a write", func(t *testing.T) {
		var p *hostProgress
		assert.Equal(t, hostDownWriteSent, p.stage())
		assert.NotPanics(t, func() { p.begin([]string{"list"})(nil) })
	})
	t.Run("the context carries the record", func(t *testing.T) {
		assert.Nil(t, hostProgressFrom(context.Background()))
		ctx, p := withHostProgress(context.Background())
		assert.Same(t, p, hostProgressFrom(ctx))
	})
}
