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

package contracts

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func statusErrWithReason(t *testing.T, reason, domain string) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, "nothing was created").
		WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	require.NoError(t, err)
	return st.Err()
}

func TestIsCreateNotPerformed(t *testing.T) {
	assert.False(t, IsCreateNotPerformed(nil))
	assert.False(t, IsCreateNotPerformed(errors.New("boom")))
	assert.False(t, IsCreateNotPerformed(statusErrWithReason(t, VMOperationFailedReason, ErrorInfoDomain)))
	assert.False(t, IsCreateNotPerformed(statusErrWithReason(t, CreateNotPerformedReason, "example.com")),
		"only VirtRigaud's domain counts")

	raw := statusErrWithReason(t, CreateNotPerformedReason, ErrorInfoDomain)
	assert.True(t, IsCreateNotPerformed(raw))
	assert.True(t, IsCreateNotPerformed(fmt.Errorf("create: %w", raw)), "found through a wrap")

	marked := MarkCreateNotPerformed(NewRetryableError("create: nothing was created", nil))
	assert.True(t, IsCreateNotPerformed(marked))
	assert.True(t, IsRetryable(marked), "the mark keeps the wrapped error's chain")
	assert.Equal(t, "Retryable: create: nothing was created", marked.Error())
	assert.Nil(t, MarkCreateNotPerformed(nil))
}
