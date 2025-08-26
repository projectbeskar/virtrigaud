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

// Package errors provides typed error handling for provider implementations.
package errors

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Provider error types that map to gRPC status codes.
var (
	// InvalidSpec indicates invalid configuration or specification
	ErrInvalidSpec = errors.New("invalid specification")

	// NotFound indicates the requested resource does not exist
	ErrNotFound = errors.New("resource not found")

	// AlreadyExists indicates the resource already exists
	ErrAlreadyExists = errors.New("resource already exists")

	// PermissionDenied indicates insufficient permissions
	ErrPermissionDenied = errors.New("permission denied")

	// Unavailable indicates the provider service is temporarily unavailable
	ErrUnavailable = errors.New("service unavailable")

	// Internal indicates an internal provider error
	ErrInternal = errors.New("internal error")

	// Unimplemented indicates the operation is not supported
	ErrUnimplemented = errors.New("operation not implemented")

	// Timeout indicates the operation timed out
	ErrTimeout = errors.New("operation timeout")

	// Canceled indicates the operation was canceled
	ErrCanceled = errors.New("operation canceled")
)

// ProviderError wraps a native error with provider-specific context.
type ProviderError struct {
	// Code is the gRPC status code
	Code codes.Code

	// Message is the human-readable error message
	Message string

	// Cause is the underlying error
	Cause error

	// Retryable indicates if the operation can be retried
	Retryable bool

	// RetryAfter suggests when to retry (for rate limiting)
	RetryAfter time.Duration

	// Details contains additional error context
	Details map[string]interface{}
}

// Error implements the error interface.
func (e *ProviderError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap returns the underlying error.
func (e *ProviderError) Unwrap() error {
	return e.Cause
}

// GRPCStatus converts the error to a gRPC status.
func (e *ProviderError) GRPCStatus() *status.Status {
	return status.New(e.Code, e.Error())
}

// NewInvalidSpec creates an invalid specification error.
func NewInvalidSpec(message string, args ...interface{}) *ProviderError {
	return &ProviderError{
		Code:      codes.InvalidArgument,
		Message:   fmt.Sprintf(message, args...),
		Retryable: false,
	}
}

// NewNotFound creates a resource not found error.
func NewNotFound(resource, id string) *ProviderError {
	return &ProviderError{
		Code:      codes.NotFound,
		Message:   fmt.Sprintf("%s %q not found", resource, id),
		Retryable: false,
	}
}

// NewAlreadyExists creates a resource already exists error.
func NewAlreadyExists(resource, id string) *ProviderError {
	return &ProviderError{
		Code:      codes.AlreadyExists,
		Message:   fmt.Sprintf("%s %q already exists", resource, id),
		Retryable: false,
	}
}

// NewPermissionDenied creates a permission denied error.
func NewPermissionDenied(operation string) *ProviderError {
	return &ProviderError{
		Code:      codes.PermissionDenied,
		Message:   fmt.Sprintf("permission denied: %s", operation),
		Retryable: false,
	}
}

// NewUnavailable creates a service unavailable error.
func NewUnavailable(service string, cause error) *ProviderError {
	return &ProviderError{
		Code:      codes.Unavailable,
		Message:   fmt.Sprintf("service unavailable: %s", service),
		Cause:     cause,
		Retryable: true,
	}
}

// NewInternal creates an internal error.
func NewInternal(message string, cause error) *ProviderError {
	return &ProviderError{
		Code:      codes.Internal,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewUnimplemented creates an unimplemented operation error.
func NewUnimplemented(operation string) *ProviderError {
	return &ProviderError{
		Code:      codes.Unimplemented,
		Message:   fmt.Sprintf("operation not implemented: %s", operation),
		Retryable: false,
	}
}

// NewTimeout creates a timeout error.
func NewTimeout(operation string, duration time.Duration) *ProviderError {
	return &ProviderError{
		Code:      codes.DeadlineExceeded,
		Message:   fmt.Sprintf("operation %s timed out after %v", operation, duration),
		Retryable: true,
	}
}

// NewRateLimit creates a rate limit error.
func NewRateLimit(retryAfter time.Duration) *ProviderError {
	return &ProviderError{
		Code:       codes.ResourceExhausted,
		Message:    "rate limit exceeded",
		Retryable:  true,
		RetryAfter: retryAfter,
	}
}

// NewCanceled creates a canceled operation error.
func NewCanceled(operation string) *ProviderError {
	return &ProviderError{
		Code:      codes.Canceled,
		Message:   fmt.Sprintf("operation canceled: %s", operation),
		Retryable: false,
	}
}

// Wrap wraps a native error with provider context.
func Wrap(err error, message string, args ...interface{}) *ProviderError {
	if err == nil {
		return nil
	}

	// If it's already a ProviderError, update the message
	if pe, ok := err.(*ProviderError); ok {
		return &ProviderError{
			Code:       pe.Code,
			Message:    fmt.Sprintf(message, args...),
			Cause:      pe,
			Retryable:  pe.Retryable,
			RetryAfter: pe.RetryAfter,
			Details:    pe.Details,
		}
	}

	// Classify the error based on known types
	code := classifyError(err)

	return &ProviderError{
		Code:      code,
		Message:   fmt.Sprintf(message, args...),
		Cause:     err,
		Retryable: isRetryable(code),
	}
}

// ToGRPCError converts any error to a gRPC status error.
func ToGRPCError(err error) error {
	if err == nil {
		return nil
	}

	if pe, ok := err.(*ProviderError); ok {
		return pe.GRPCStatus().Err()
	}

	// For unknown errors, return as internal
	return status.Error(codes.Internal, err.Error())
}

// FromGRPCError converts a gRPC status error to a ProviderError.
func FromGRPCError(err error) *ProviderError {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return NewInternal("unknown error", err)
	}

	return &ProviderError{
		Code:      st.Code(),
		Message:   st.Message(),
		Retryable: isRetryable(st.Code()),
	}
}

// IsRetryable checks if an error indicates a retryable operation.
func IsRetryable(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.Retryable
	}

	if st, ok := status.FromError(err); ok {
		return isRetryable(st.Code())
	}

	return false
}

// IsNotFound checks if an error indicates a resource was not found.
func IsNotFound(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.Code == codes.NotFound
	}

	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.NotFound
	}

	return errors.Is(err, ErrNotFound)
}

// IsInvalidSpec checks if an error indicates an invalid specification.
func IsInvalidSpec(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.Code == codes.InvalidArgument
	}

	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.InvalidArgument
	}

	return errors.Is(err, ErrInvalidSpec)
}

// classifyError attempts to classify a native error into a gRPC code.
func classifyError(err error) codes.Code {
	switch {
	case errors.Is(err, ErrInvalidSpec):
		return codes.InvalidArgument
	case errors.Is(err, ErrNotFound):
		return codes.NotFound
	case errors.Is(err, ErrAlreadyExists):
		return codes.AlreadyExists
	case errors.Is(err, ErrPermissionDenied):
		return codes.PermissionDenied
	case errors.Is(err, ErrUnavailable):
		return codes.Unavailable
	case errors.Is(err, ErrUnimplemented):
		return codes.Unimplemented
	case errors.Is(err, ErrTimeout):
		return codes.DeadlineExceeded
	case errors.Is(err, ErrCanceled):
		return codes.Canceled
	default:
		return codes.Internal
	}
}

// isRetryable determines if a gRPC code indicates a retryable error.
func isRetryable(code codes.Code) bool {
	switch code {
	case codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}
