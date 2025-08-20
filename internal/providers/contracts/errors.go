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

package contracts

import (
	"fmt"
)

// ErrorType represents the category of error
type ErrorType string

const (
	// ErrorTypeNotFound indicates resource not found
	ErrorTypeNotFound ErrorType = "NotFound"
	// ErrorTypeInvalidSpec indicates invalid specification
	ErrorTypeInvalidSpec ErrorType = "InvalidSpec"
	// ErrorTypeRetryable indicates a transient error
	ErrorTypeRetryable ErrorType = "Retryable"
	// ErrorTypeUnauthorized indicates authentication/authorization failure
	ErrorTypeUnauthorized ErrorType = "Unauthorized"
	// ErrorTypeNotSupported indicates unsupported operation
	ErrorTypeNotSupported ErrorType = "NotSupported"
)

// ProviderError represents a categorized error from a provider
type ProviderError struct {
	// Type categorizes the error
	Type ErrorType
	// Message describes the error
	Message string
	// Cause contains the underlying error
	Cause error
	// Retryable indicates if the operation should be retried
	Retryable bool
}

// Error implements the error interface
func (e *ProviderError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Type, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// Unwrap returns the underlying error
func (e *ProviderError) Unwrap() error {
	return e.Cause
}

// IsRetryable returns true if the error is retryable
func (e *ProviderError) IsRetryable() bool {
	return e.Retryable || e.Type == ErrorTypeRetryable
}

// NewNotFoundError creates a not found error
func NewNotFoundError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeNotFound,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewInvalidSpecError creates an invalid spec error
func NewInvalidSpecError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeInvalidSpec,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewRetryableError creates a retryable error
func NewRetryableError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeRetryable,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
}

// NewUnauthorizedError creates an unauthorized error
func NewUnauthorizedError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeUnauthorized,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewNotSupportedError creates a not supported error
func NewNotSupportedError(message string) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeNotSupported,
		Message:   message,
		Retryable: false,
	}
}
