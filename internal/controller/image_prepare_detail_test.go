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

package controller

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

func TestSanitizeProviderDetail(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil": {nil, ""},
		"a categorized error renders its message only": {
			contracts.NewInvalidSpecError("image prepare: bad path", errors.New("rpc error: code = InvalidArgument")),
			"image prepare: bad path"},
		"control characters and newlines become one space": {
			errors.New("line one\nline two\r\n\tthree\x1b[31m"), "line one line two three [31m"},
		"URL userinfo is removed": {
			errors.New("download https://user:s3cret@images.example.com/a.qcow2?x=1 failed; retry ftp://u@h"),
			"download https://images.example.com/a.qcow2?x=1 failed; retry ftp://h"},
		"an '@' after the authority is kept": {
			errors.New("GET https://images.example.com/a@b.qcow2: 404"), "GET https://images.example.com/a@b.qcow2: 404"},
		"invalid UTF-8 becomes a space": {errors.New("bad \xff byte"), "bad byte"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeProviderDetail(tc.err))
		})
	}

	t.Run("a long detail is cut on a rune boundary", func(t *testing.T) {
		got := sanitizeProviderDetail(errors.New(strings.Repeat("é", 400)))
		assert.LessOrEqual(t, len(got), maxProviderDetailBytes)
		assert.True(t, utf8.ValidString(got))
		assert.True(t, strings.HasSuffix(got, providerDetailEllipsis))
	})
}
