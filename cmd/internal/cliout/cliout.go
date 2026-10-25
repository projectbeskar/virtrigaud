/*
Copyright 2025.

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

// Package cliout writes the human-readable output of the command-line tools.
// A command's standard output has nowhere to report a failed write to, so these
// helpers drop the write error in one documented place instead of at every call.
package cliout

import (
	"fmt"
	"io"
)

// Printf formats according to format and writes to w. A write error is
// ignored: the command's output stream is the only place it could be reported.
func Printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// Println writes a to w in the manner of fmt.Println. A write error is
// ignored, as in Printf.
func Println(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}
