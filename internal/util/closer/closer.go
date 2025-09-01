/*
Copyright 2025.

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

package closer

import (
	"io"

	"go.uber.org/zap"
)

// CloseQuietly closes an io.Closer and logs any error without panicking.
// This is useful for defer statements where the error is not critical to the operation.
func CloseQuietly(c io.Closer, log *zap.SugaredLogger, ctx string) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		if log != nil {
			log.Warnw("close failed", "ctx", ctx, "err", err)
		}
	}
}

// CloseQuietlyWithoutLogger closes an io.Closer and ignores any error.
// Use this only when logging is not available and the close error is not critical.
func CloseQuietlyWithoutLogger(c io.Closer) {
	if c != nil {
		_ = c.Close() //nolint:errcheck // Intentionally ignored for cleanup
	}
}
