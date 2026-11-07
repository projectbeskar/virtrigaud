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
	"fmt"
	"os"
)

// LoadEnrolments reads the development enrolment file at path
// (ParseEnrolments). With an empty path it returns nil and no error: no host
// has an enrolment, and nothing is sent to a Provider that requires operation
// identity.
func LoadEnrolments(path string) (EnrolmentSource, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- an administrator-chosen development file
	if err != nil {
		return nil, fmt.Errorf("read development enrolment file: %w", err)
	}
	s, err := ParseEnrolments(data)
	if err != nil {
		return nil, err
	}
	return s, nil
}
