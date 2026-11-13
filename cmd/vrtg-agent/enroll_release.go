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

	"github.com/go-logr/logr"
)

// enrollNoStepStatus is the exit status of "enroll" with no step: a release
// build has none.
const enrollNoStepStatus = 1

// enrollCommand is the release build's placeholder: host enrolment from the
// agent's side exists in development builds only, until the manager's
// re-enrolment flow lands. A release build enrols nothing and its host
// answers stay unsigned.
func enrollCommand(_ []string, _ logr.Logger, _, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "vrtg-agent enroll is available only in development builds (virtrigaud_dev build tag)")
	return 1
}
