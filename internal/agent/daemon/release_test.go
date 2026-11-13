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

package daemon_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
)

// TestReleaseBuildRefusesTestOverrides: a release build refuses to start
// when any Go-level test override is set, such as another trust directory.
func TestReleaseBuildRefusesTestOverrides(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir(), ListenAddress: "127.0.0.1:0", DrainTimeout: 1, Executor: config.ExecutorNone}
	uid := uint32(1000)
	for name, o := range map[string]daemon.Options{
		"TrustDir": {TrustDir: "/srv/trust"},
		"Probe":    {Probe: &hostchecktest.Fake{}},
		"UID":      {UID: &uid},
		"Hooks":    {Hooks: daemon.Hooks{BeforeRecover: func(context.Context) {}}},
		"Now":      {Now: time.Now},
	} {
		o.Config, o.Executor = cfg, none.Executor{}
		err := daemon.Run(context.Background(), o, nil)
		if !errors.Is(err, daemon.ErrStartRefused) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: Run = %v", name, err)
		}
	}
}
