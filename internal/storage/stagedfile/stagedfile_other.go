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

//go:build !linux

package stagedfile

import (
	"io/fs"
	"os"
)

// OpenRead is unsupported off Linux: the providers that stage files run only
// on Linux, and failing closed beats an unconfined open.
func OpenRead(root, rel string) (*os.File, error) {
	return nil, failure(OpOpen, root, rel, ErrUnsupported)
}

// Create is unsupported off Linux (see OpenRead).
func Create(root, rel string, _, _ fs.FileMode) (*os.File, error) {
	return nil, failure(OpCreate, root, rel, ErrUnsupported)
}

// Remove is unsupported off Linux (see OpenRead).
func Remove(root, rel string) error {
	return failure(OpRemove, root, rel, ErrUnsupported)
}
