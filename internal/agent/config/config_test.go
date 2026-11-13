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
	"flag"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
)

func parse(t *testing.T, args ...string) *config.Config {
	t.Helper()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	c := config.Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	c := parse(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.StateDir != config.DefaultStateDir || c.Executor != config.ExecutorNone || c.PKIDir() != config.DefaultStateDir+"/pki" {
		t.Fatalf("defaults = %+v", c)
	}
	if c.SignerSocket != config.DefaultSignerSocket || c.SignerUser != config.DefaultSignerUser || c.SignerKeyFile != config.DefaultSignerKeyFile {
		t.Fatalf("signer defaults = %q %q %q", c.SignerSocket, c.SignerUser, c.SignerKeyFile)
	}
	if c.ScrubInterval != config.DefaultScrubInterval || config.DefaultScrubInterval != time.Hour {
		t.Fatalf("scrub interval = %v", c.ScrubInterval)
	}
	if d, err := c.TrustDir(); err != nil || d != "/usr/share/virtrigaud/trust" {
		t.Fatalf("trust dir = %q, %v", d, err)
	}
	e, err := c.NewExecutor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Plan(1); err == nil {
		t.Fatal("the default executor plans an operation")
	}
}

func TestValidateRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"--state-dir=relative"}, {"--state-dir=/var/lib/x/"}, {"--listen-address="}, {"--drain-timeout=0s"}, {"--scrub-interval=0s"}, {"--scrub-interval=-1m"},
		{"--executor=libvirt"}, {"--dev-trust-dir=relative"},
		{"--signer-socket=relative.sock"}, {"--signer-socket="}, {"--signer-socket=/run/x/../y.sock"}, {"--signer-user="},
		{"--signer-key-file=host.key"}, {"--signer-key-file="},
	} {
		if err := parse(t, args...).Validate(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestParseDevEnrolment(t *testing.T) {
	e, err := config.ParseDevEnrolment([]byte(`{"startEpoch": 3, "predecessors": ["000102030405060708090a0b0c0d0e0f"]}`))
	if err != nil || e.StartEpoch != 3 || len(e.Predecessors) != 1 || e.Predecessors[0][15] != 15 {
		t.Fatalf("%+v, %v", e, err)
	}
	for _, bad := range []string{
		`{}`, `{"startEpoch": 0}`, `{"startEpoch": 1, "other": 1}`, `{"startEpoch": 1} {}`,
		`{"startEpoch": 1, "predecessors": ["00"]}`, `not json`,
	} {
		if _, err := config.ParseDevEnrolment([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestScrubIntervalFlag(t *testing.T) {
	c := parse(t, "--scrub-interval=90s")
	if err := c.Validate(); err != nil || c.ScrubInterval != 90*time.Second {
		t.Fatalf("%v, %v", c.ScrubInterval, err)
	}
}
