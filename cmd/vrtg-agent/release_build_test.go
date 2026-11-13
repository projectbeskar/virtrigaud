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
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
)

// TestReleaseBuildRefusesDevelopmentFlags: a release build of vrtg-agent
// refuses to start with the fake executor, another trust directory or a
// development enrolment file, and runs only the no-op executor, which
// refuses every operation kind.
func TestReleaseBuildRefusesDevelopmentFlags(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--executor=fake"}, {"run", "--dev-trust-dir=/srv/trust"}, {"run", "--dev-enrolment-file=/srv/e.json"},
		{"shutdown-vms", "--executor=fake"},
	} {
		var out, errOut bytes.Buffer
		if code := realMain(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "development build") {
			t.Errorf("%v: exit %d\n%s", args, code, errOut.String())
		}
	}
	cfg := &config.Config{StateDir: "/var/lib/virtrigaud-agent", ListenAddress: ":1", DrainTimeout: 1, Executor: config.ExecutorNone}
	e, err := cfg.NewExecutor()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(none.Executor); !ok {
		t.Fatalf("release executor = %T", e)
	}
	for _, row := range grantverify.Rows() {
		if _, err := e.Plan(row.Kind); !errors.Is(err, executor.ErrUnsupportedKind) {
			t.Errorf("release executor plans %v", row.Kind)
		}
	}
}
