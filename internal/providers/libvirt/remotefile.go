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
	"fmt"
	"os"
)

// localPrivateFileMode is the mode writeRemoteFile uses on a local (non-SSH)
// connection: owner read/write only, matching the umask 077 the SSH path
// applies (see writePrivateFileScript).
const localPrivateFileMode = 0o600

// writeRemoteFile writes content to path on the libvirt host this provider
// manages (domain XML, storage-pool XML, cloud-init user-data / meta-data).
//
// It replaces the former `bash -c "cat > '<path>' << 'EOF_…'\n<content>\nEOF_…"`
// heredoc: the content now travels on the SSH session's STDIN, never inside the
// command line, so
//
//   - no content can terminate a heredoc early and inject commands (a line
//     equal to the delimiter used to end the heredoc and run what followed);
//   - the path is a positional parameter of a fixed `sh -c` script
//     (writePrivateFileArgv), never interpolated into shell text;
//   - the content (which may carry cloud-init secrets) no longer appears in the
//     command string that runOverSSH logs and embeds in VirshError.Command;
//   - a newly created file is private to the SSH user (umask 077).
//
// Transient SSH connection failures are retried exactly like runVirshCommand
// (retryOnTransientSSH); host-key mismatches and auth failures are not. On a
// local (non-SSH) connection the "host" is this process's filesystem, so the
// file is written directly (mode localPrivateFileMode).
func (v *VirshProvider) writeRemoteFile(ctx context.Context, path string, content []byte) error {
	if err := v.refuseIfUnroutable(); err != nil {
		return err
	}
	if !v.isSSHTransport() {
		if err := os.WriteFile(path, content, localPrivateFileMode); err != nil {
			return fmt.Errorf("write local file %s: %w", path, err)
		}
		return nil
	}

	remoteCmd, err := shellJoin(writePrivateFileArgv(path))
	if err != nil {
		return fmt.Errorf("build remote write command for %s: %w", path, err)
	}
	_, err = retryOnTransientSSH(ctx, func() (*VirshResult, error) {
		// A fresh reader per attempt: a retried attempt must resend the whole
		// content, not the remainder of a partially-consumed reader.
		if runErr := runSSHStdin(ctx, v, bytes.NewReader(content), remoteCmd); runErr != nil {
			// retryOnTransientSSH classifies transient connect failures from
			// Stderr; runSSHStdin folds the remote stderr / connect error into
			// its error text, so surface that text there.
			return &VirshResult{Command: remoteCmd, ExitCode: -1, Stderr: runErr.Error()}, runErr
		}
		return &VirshResult{Command: remoteCmd}, nil
	})
	if err != nil {
		return fmt.Errorf("write remote file %s: %w", path, err)
	}
	return nil
}
