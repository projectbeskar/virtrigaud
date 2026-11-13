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

package agenttarget

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// errInfoOf is a google.rpc.ErrorInfo in the contract's domain.
func errInfoOf(reason string, md map[string]string) *errdetails.ErrorInfo {
	return &errdetails.ErrorInfo{Reason: reason, Domain: contracts.ErrorInfoDomain, Metadata: md}
}
