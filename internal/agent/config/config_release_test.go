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

package config_test

import (
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
)

// TestReleaseBuildRefusesDevelopmentSettings: a release build has no fake
// executor, no other trust directory and no development enrolment.
func TestReleaseBuildRefusesDevelopmentSettings(t *testing.T) {
	if config.DevBuild {
		t.Fatal("a build without the virtrigaud_dev tag reports a development build")
	}
	for _, args := range [][]string{
		{"--executor=fake"}, {"--dev-trust-dir=/srv/trust"}, {"--dev-enrolment-file=/srv/enrolment.json"},
	} {
		if err := parse(t, args...).Validate(); !errors.Is(err, config.ErrDevOnly) {
			t.Errorf("%v: %v, want ErrDevOnly", args, err)
		}
	}
	c := parse(t, "--executor=fake")
	if _, err := c.NewExecutor(); !errors.Is(err, config.ErrDevOnly) {
		t.Fatalf("NewExecutor(fake) = %v", err)
	}
	c = parse(t, "--dev-enrolment-file=/srv/enrolment.json")
	if _, err := c.LoadDevEnrolment(); !errors.Is(err, config.ErrDevOnly) {
		t.Fatalf("LoadDevEnrolment = %v", err)
	}
}
