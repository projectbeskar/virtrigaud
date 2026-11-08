//go:build !unix

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

package journal

import "errors"

const noFollow = 0

type fileLock struct{}

// acquireLock is not supported off Unix: the journal refuses to open.
func acquireLock(string) (*fileLock, error) {
	return nil, errors.New("the journal's single-writer lock needs a Unix system")
}

func (l *fileLock) release() error { return nil }
