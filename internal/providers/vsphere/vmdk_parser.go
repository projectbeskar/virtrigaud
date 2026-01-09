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

package vsphere

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// VMDKDescriptor represents a parsed VMDK descriptor file
type VMDKDescriptor struct {
	DescriptorPath string
	ExtentFiles    []string // Additional files referenced by the descriptor
}

// parseVMDKDescriptor parses a VMDK descriptor file to find all referenced files
func parseVMDKDescriptor(descriptorPath string) (*VMDKDescriptor, error) {
	file, err := os.Open(descriptorPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open VMDK descriptor: %w", err)
	}
	defer file.Close()

	descriptor := &VMDKDescriptor{
		DescriptorPath: descriptorPath,
		ExtentFiles:    []string{},
	}

	// Regular expressions to match extent entries
	// Format: RW <size> <type> "<filename>"
	// Examples:
	//   RW 167772160 SESPARSE "vm-000001-sesparse.vmdk"
	//   RW 167772160 VMFS "vm-flat.vmdk"
	//   RW 167772160 FLAT "vm-flat.vmdk"
	extentRegex := regexp.MustCompile(`^RW\s+\d+\s+\w+\s+"([^"]+)"`)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Check for extent entries
		matches := extentRegex.FindStringSubmatch(line)
		if len(matches) > 1 {
			filename := matches[1]
			descriptor.ExtentFiles = append(descriptor.ExtentFiles, filename)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read VMDK descriptor: %w", err)
	}

	return descriptor, nil
}

// extractDatastoreBasePath extracts the base directory from a datastore path
// Example: "[datastore1] vm-folder/vm-000001.vmdk" -> "[datastore1] vm-folder"
func extractDatastoreBasePath(datastorePath string) string {
	dir := path.Dir(datastorePath)
	return dir
}

// constructDatastorePath builds a full datastore path from base path and filename
// Example: "[datastore1] vm-folder", "vm-sesparse.vmdk" -> "[datastore1] vm-folder/vm-sesparse.vmdk"
func constructDatastorePath(basePath, filename string) string {
	// Extract datastore name and directory
	if strings.HasPrefix(basePath, "[") {
		endBracket := strings.Index(basePath, "]")
		if endBracket != -1 {
			datastoreName := basePath[:endBracket+1]
			directory := strings.TrimSpace(basePath[endBracket+1:])

			// Construct full path
			if directory != "" {
				return fmt.Sprintf("%s %s/%s", datastoreName, directory, filename)
			}
			return fmt.Sprintf("%s %s", datastoreName, filename)
		}
	}

	// Fallback: just join paths
	return path.Join(basePath, filename)
}
