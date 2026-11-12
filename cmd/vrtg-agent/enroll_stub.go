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

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
)

// enrollCommand is the placeholder of host enrolment, which arrives with a
// later change; until then a development build enrols a fresh store at
// start-up from its certificate and a development enrolment file.
func enrollCommand(_ []string, stderr io.Writer) int {
	_, _ = fmt.Fprintf(stderr, "vrtg-agent enroll is not available yet; a development build enrols at start-up with --%s\n",
		config.FlagDevEnrolmentFile)
	return 1
}
