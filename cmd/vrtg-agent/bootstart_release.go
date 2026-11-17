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
	"fmt"
	"io"

	"github.com/go-logr/logr"
)

// bootStartCommand is the release build's placeholder: the boot-start unit
// is a development-build unit.
func bootStartCommand(_ []string, _ logr.Logger, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "vrtg-agent boot-start is available only in development builds (virtrigaud_dev build tag)")
	return 1
}
