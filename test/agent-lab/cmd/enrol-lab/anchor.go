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

package main

import (
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/projectbeskar/virtrigaud/internal/operation"
)

// runAnchor prints the grant anchor certificate of a development operation
// signer key file.
func runAnchor(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("anchor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("signer-key", "", "development operation signer key file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" {
		return errors.New("-signer-key is required")
	}
	s, err := operation.LoadDevSignerKeyFile(*keyFile)
	if err != nil {
		return fmt.Errorf("load the signer key: %w", err)
	}
	if err := pem.Encode(stdout, &pem.Block{Type: "CERTIFICATE", Bytes: s.Anchor().Raw}); err != nil {
		return fmt.Errorf("write the anchor: %w", err)
	}
	return nil
}
