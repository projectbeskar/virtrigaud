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

package operation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadEnrolments_DevBuild(t *testing.T) {
	s, err := LoadEnrolments("")
	require.NoError(t, err)
	assert.Nil(t, s)
	path := filepath.Join(t.TempDir(), "enrolment.json")
	require.NoError(t, os.WriteFile(path, []byte(validEnrolmentFile), 0o600))
	s, err = LoadEnrolments(path)
	require.NoError(t, err)
	_, err = s.Enrolment("ns", "p", "host-a")
	require.NoError(t, err)
	_, err = LoadEnrolments(filepath.Join(t.TempDir(), "none.json"))
	require.Error(t, err)
}
