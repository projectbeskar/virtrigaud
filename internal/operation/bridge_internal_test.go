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
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
)

// The verifier and the kind table moved to grantverify. The tests in this
// package that predate the move reach a few of their internals through these
// names; they are bridges for tests only.
var (
	kindTable           = grantverify.Rows()
	parseECDSASignature = grantverify.ParseECDSASignature
	normalizeLowS       = grantverify.NormalizeLowS
	envelopeFieldName   = protoreflect.Name("operation_envelope")
)
