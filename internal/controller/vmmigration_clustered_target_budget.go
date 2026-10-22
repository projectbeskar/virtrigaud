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

import "time"

// notPerformedBudget is how long a series of consecutive "not performed"
// answers to a routed import may last before the attempt fails (#416 M3):
// routedImportNotPerformedBudget, or the test override.
func (r *VMMigrationReconciler) notPerformedBudget() time.Duration {
	if r.routedImportNotPerformedLimit > 0 {
		return r.routedImportNotPerformedLimit
	}
	return routedImportNotPerformedBudget
}
