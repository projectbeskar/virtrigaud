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

// mutationGuard names a safety check of the manager that a mutation test
// build can switch off, so that a test proves it notices the check's
// removal (as the per-host agent's guards do). In every ordinary build
// mutationOff is a constant false and the compiler removes the branch; only
// a test binary built with the virtrigaud_mutation build tag reads the
// VIRTRIGAUD_MUTATION environment variable.
type mutationGuard string

// The manager's mutation guards.
const (
	// guardRetireLedgerRaise: an applied retirement is closed only after
	// the host ledger covers the retirement answer's position.
	guardRetireLedgerRaise mutationGuard = "retire-ledger-raise"
	// guardRetireAnswerPosition: an applied retirement is closed only when
	// its answer is not behind the store position the manager holds.
	guardRetireAnswerPosition mutationGuard = "retire-answer-position"
)

// mutationGuards lists every manager mutation guard.
func mutationGuards() []mutationGuard {
	return []mutationGuard{guardRetireLedgerRaise, guardRetireAnswerPosition}
}
