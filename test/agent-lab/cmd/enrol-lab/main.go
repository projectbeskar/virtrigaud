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

// Command enrol-lab is test tooling for per-host agent enrolment runs in a
// disposable cluster. It is not part of any release and is built only with
// the virtrigaud_dev tag.
//
//	enrol-lab anchor -signer-key FILE
//	    prints the development operation signer's grant anchor certificate
//	    (PEM), for the agent's grant-anchors.pem.
//	enrol-lab image -binary FILE -name REF -out TAR [-arg A]...
//	    writes a single-layer image archive (docker save layout) whose
//	    entrypoint is the binary with the given arguments, for
//	    "ctr images import".
//	enrol-lab revoke-probe -kubeconfig F -namespace NS -host H -provider ADDR
//	    revokes a Host (spec.revoked=true) while probing the Provider with
//	    GetHostInfo for that host, and prints one JSON line with the time from
//	    the revocation write to the Host condition, the inventory Secret
//	    without the host and the first refused call.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain dispatches a subcommand.
func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: enrol-lab anchor|image|revoke-probe [flags]")
		return exitUsage
	}
	var err error
	switch args[0] {
	case "anchor":
		err = runAnchor(args[1:], stdout, stderr)
	case "image":
		err = runImage(args[1:], stderr)
	case "revoke-probe":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err = runRevokeProbe(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		return exitUsage
	}
	if err != nil {
		if err == flag.ErrHelp {
			return exitUsage
		}
		_, _ = fmt.Fprintln(stderr, "enrol-lab:", err)
		return exitError
	}
	return exitOK
}
