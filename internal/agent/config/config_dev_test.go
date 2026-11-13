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

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
)

// TestDevBuildSettings: a development build accepts the fake executor, a
// trust directory and a development enrolment file.
func TestDevBuildSettings(t *testing.T) {
	if !config.DevBuild {
		t.Fatal("a virtrigaud_dev build reports a release build")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "enrolment.json")
	if err := os.WriteFile(file, []byte(`{"startEpoch": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := parse(t, "--executor=fake", "--state-dir="+dir, "--dev-trust-dir=/srv/trust", "--dev-enrolment-file="+file)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.TrustDir(); d != "/srv/trust" {
		t.Fatalf("trust dir = %q", d)
	}
	e, err := c.NewExecutor()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*fake.Executor); !ok {
		t.Fatalf("executor = %T", e)
	}
	if en, err := c.LoadDevEnrolment(); err != nil || en.StartEpoch != 2 {
		t.Fatalf("enrolment = %+v, %v", en, err)
	}
}
