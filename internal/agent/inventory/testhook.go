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

package inventory

import "testing"

// SetEncodedLimitForTest lowers the encoded size bound Validate applies to n
// bytes until t ends, so that a test can reach the size fallback, which the
// other bounds make unreachable with real data. It has no use outside tests.
func SetEncodedLimitForTest(t testing.TB, n int) {
	t.Helper()
	old := encodedLimit.Swap(int64(n))
	t.Cleanup(func() { encodedLimit.Store(old) })
}
