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

package logging

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactURL(t *testing.T) {
	for raw, want := range map[string]string{
		"": "",
		"https://joe:p4ss@images.example.com/a/b.qcow2?X-Amz-Signature=s3cr3t#frag": "https://images.example.com/a/b.qcow2?…",
		"https://images.example.com/b.qcow2":                                        "https://images.example.com/b.qcow2",
		"http://images.example.com/b.qcow2?":                                        "http://images.example.com/b.qcow2?…",
		"https://images.example.com/%zz?token=t":                                    "<unparseable URL>",
	} {
		assert.Equal(t, want, RedactURL(raw), raw)
	}
}
