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

package operation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A release build loads no enrolment file: an empty path is nothing, any
// other path is refused without being read.
func TestLoadEnrolments_ReleaseBuildRefuses(t *testing.T) {
	s, err := LoadEnrolments("")
	require.NoError(t, err)
	assert.Nil(t, s)
	_, err = LoadEnrolments("/nonexistent/enrolment.json")
	require.ErrorIs(t, err, ErrNoSigner)
}
