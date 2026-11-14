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

package daemon

import (
	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// devJournalHooks returns no hook in a release build: the crash hook
// environment is ignored.
func devJournalHooks(logr.Logger) (journal.Hooks, int64) { return journal.Hooks{}, 0 }
