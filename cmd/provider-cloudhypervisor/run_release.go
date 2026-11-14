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

package main

import (
	"fmt"
	"io"

	"github.com/projectbeskar/virtrigaud/internal/version"
)

// releaseRefusal is what a release build prints instead of running.
const releaseRefusal = "provider-cloudhypervisor is available in development builds only (build tag virtrigaud_dev); this release build does not run it"

// realMain refuses to run: a release build links none of the Provider. Only
// --version answers.
func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintf(stdout, "virtrigaud-provider-cloudhypervisor %s (release build: not runnable)\n", version.String())
		return exitOK
	}
	_, _ = fmt.Fprintln(stderr, releaseRefusal)
	return exitError
}
