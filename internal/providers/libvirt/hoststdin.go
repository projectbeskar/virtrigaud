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
	"bytes"
	"context"
	"errors"
	"fmt"
)

// runHostStdin runs the host command argv on the libvirt host with stdin as its
// standard input and returns its result exactly like runVirshCommand("!",
// argv...) does (stdout, stderr, exit code; transient SSH connection failures
// retried with a fresh copy of stdin).
//
// It is for content that must never appear on a command line — which the
// provider logs and the host shows in its process list — nor rest in a file:
// the image source URL of ImagePrepare (it may embed credentials or a
// presigned token) is handed to `curl -K -` this way. argv is quoted exactly
// like any other host command (shellJoin); stdin is never interpreted by a
// shell.
func (v *VirshProvider) runHostStdin(ctx context.Context, stdin []byte, argv ...string) (*VirshResult, error) {
	if err := v.refuseIfUnroutable(); err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errors.New("no host command specified")
	}
	if !v.isSSHTransport() {
		return v.runLocalStdin(ctx, argv, bytes.NewReader(stdin))
	}
	remoteCmd, err := shellJoin(argv)
	if err != nil {
		return nil, fmt.Errorf("build remote command: %w", err)
	}
	return retryOnTransientSSH(ctx, func() (*VirshResult, error) {
		// A fresh reader per attempt: a retried attempt must resend the whole
		// input, not the remainder of a partially-consumed reader.
		return v.runOverSSHStdin(ctx, remoteCmd, bytes.NewReader(stdin))
	})
}
