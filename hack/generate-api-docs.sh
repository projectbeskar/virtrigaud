#!/usr/bin/env bash
# Copyright 2025.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Script to generate API documentation from Go source code
# This extracts GoDoc comments and creates markdown documentation

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DOCS_DIR="${PROJECT_ROOT}/docs/src/api-reference"
API_DIR="${PROJECT_ROOT}/api/infra.virtrigaud.io/v1beta1"
INTERNAL_DIR="${PROJECT_ROOT}/internal"
SDK_DIR="${PROJECT_ROOT}/sdk"

echo "📚 Generating API documentation from Go source code..."

# Create API reference directory if it doesn't exist
mkdir -p "${DOCS_DIR}"

# Function to extract package documentation
extract_package_doc() {
    local package_dir="$1"
    local output_file="$2"
    local package_name="$3"

    echo "  Processing ${package_name}..."

    # Use go doc to extract documentation
    if ! cd "${package_dir}" 2>/dev/null; then
        echo "    ⚠️  Warning: Could not access ${package_dir}"
        return
    fi

    {
        echo "# ${package_name}"
        echo ""
        echo "_Auto-generated from Go source code_"
        echo ""

        # Get package-level documentation
        if package_doc=$(go doc -all 2>/dev/null); then
            echo "\`\`\`"
            echo "${package_doc}"
            echo "\`\`\`"
        fi

        echo ""
        echo "---"
        echo ""
        echo "_Generated on: $(date -u +"%Y-%m-%d %H:%M:%S UTC")_"
    } > "${output_file}"

    cd "${PROJECT_ROOT}"
}

# Function to generate API type documentation
generate_api_types_doc() {
    echo "  Generating CRD API Types documentation..."

    local output="${DOCS_DIR}/api-types.md"

    {
        echo "# API Types Reference"
        echo ""
        echo "_Auto-generated from CRD type definitions in \`api/infra.virtrigaud.io/v1beta1/\`_"
        echo ""
        echo "This document provides a comprehensive reference for all Custom Resource Definitions (CRDs)"
        echo "used by VirtRigaud. The Go types are the source of truth for these APIs."
        echo ""
        echo "---"
        echo ""

        # Process each CRD type file
        for types_file in "${API_DIR}"/*_types.go; do
            if [[ -f "${types_file}" ]] && [[ ! "${types_file}" =~ zz_generated ]]; then
                filename=$(basename "${types_file}")
                crd_name="${filename%_types.go}"

                echo "## ${crd_name}"
                echo ""
                echo "**Source:** [\`api/infra.virtrigaud.io/v1beta1/${filename}\`](../../api/infra.virtrigaud.io/v1beta1/${filename})"
                echo ""

                # Extract documentation using go doc
                if type_doc=$(cd "${API_DIR}" && go doc -all "${filename%.go}" 2>/dev/null); then
                    echo "\`\`\`go"
                    echo "${type_doc}" | head -100  # Limit output
                    echo "\`\`\`"
                else
                    echo "_Documentation not available via go doc_"
                fi

                echo ""
                echo "---"
                echo ""
            fi
        done

        echo ""
        echo "_Generated on: $(date -u +"%Y-%m-%d %H:%M:%S UTC")_"
    } > "${output}"

    echo "    ✅ Created ${output}"
}

# Function to generate provider contract documentation
generate_provider_contract_doc() {
    echo "  Generating Provider Contract documentation..."

    local output="${DOCS_DIR}/provider-contract.md"
    local contract_dir="${INTERNAL_DIR}/providers/contracts"

    if [[ ! -d "${contract_dir}" ]]; then
        echo "    ⚠️  Warning: Provider contracts directory not found"
        return
    fi

    {
        echo "# Provider Contract Reference"
        echo ""
        echo "_Auto-generated from provider contract interfaces in \`internal/providers/contracts/\`_"
        echo ""
        echo "This document describes the Go interface that all VirtRigaud providers must implement."
        echo ""
        echo "---"
        echo ""

        # Extract Provider interface documentation
        if contract_doc=$(cd "${contract_dir}" && go doc -all Provider 2>/dev/null); then
            echo "## Provider Interface"
            echo ""
            echo "\`\`\`go"
            echo "${contract_doc}"
            echo "\`\`\`"
            echo ""
        fi

        # Extract types documentation
        echo "## Contract Types"
        echo ""
        if types_doc=$(cd "${contract_dir}" && go doc -all . 2>/dev/null); then
            echo "\`\`\`go"
            echo "${types_doc}" | head -200  # Limit output
            echo "\`\`\`"
        fi

        echo ""
        echo "---"
        echo ""
        echo "_Generated on: $(date -u +"%Y-%m-%d %H:%M:%S UTC")_"
    } > "${output}"

    echo "    ✅ Created ${output}"
}

# Function to generate SDK documentation
generate_sdk_doc() {
    echo "  Generating SDK documentation..."

    if [[ ! -d "${SDK_DIR}" ]]; then
        echo "    ⚠️  Warning: SDK directory not found, skipping..."
        return
    fi

    local output="${DOCS_DIR}/sdk.md"

    {
        echo "# Provider SDK Reference"
        echo ""
        echo "_Auto-generated from SDK packages in \`sdk/\`_"
        echo ""
        echo "The VirtRigaud Provider SDK helps you build custom providers that integrate"
        echo "with the VirtRigaud operator."
        echo ""
        echo "## Installation"
        echo ""
        echo "\`\`\`bash"
        echo "go get github.com/projectbeskar/virtrigaud/sdk/provider"
        echo "\`\`\`"
        echo ""
        echo "---"
        echo ""

        # Document SDK packages
        if [[ -d "${SDK_DIR}/provider" ]]; then
            echo "## SDK Packages"
            echo ""

            # Server package
            if [[ -d "${SDK_DIR}/provider/server" ]]; then
                echo "### Server Package"
                echo ""
                if server_doc=$(cd "${SDK_DIR}/provider/server" && go doc -all . 2>/dev/null); then
                    echo "\`\`\`go"
                    echo "${server_doc}" | head -100
                    echo "\`\`\`"
                fi
                echo ""
            fi

            # Client package
            if [[ -d "${SDK_DIR}/provider/client" ]]; then
                echo "### Client Package"
                echo ""
                if client_doc=$(cd "${SDK_DIR}/provider/client" && go doc -all . 2>/dev/null); then
                    echo "\`\`\`go"
                    echo "${client_doc}" | head -100
                    echo "\`\`\`"
                fi
                echo ""
            fi
        fi

        echo ""
        echo "---"
        echo ""
        echo "_Generated on: $(date -u +"%Y-%m-%d %H:%M:%S UTC")_"
    } > "${output}"

    echo "    ✅ Created ${output}"
}

# Function to generate utilities documentation
generate_utilities_doc() {
    echo "  Generating Utilities documentation..."

    local output="${DOCS_DIR}/utilities.md"

    {
        echo "# Internal Utilities Reference"
        echo ""
        echo "_Auto-generated from internal utility packages_"
        echo ""
        echo "These are internal utilities used by VirtRigaud controllers and providers."
        echo ""
        echo "---"
        echo ""

        # Document key utility packages
        for util_pkg in "k8s" "resilience" "util"; do
            util_dir="${INTERNAL_DIR}/${util_pkg}"
            if [[ -d "${util_dir}" ]]; then
                echo "## ${util_pkg}"
                echo ""
                echo "**Path:** \`internal/${util_pkg}/\`"
                echo ""

                if util_doc=$(cd "${util_dir}" && go doc -all . 2>/dev/null); then
                    echo "\`\`\`go"
                    echo "${util_doc}" | head -150
                    echo "\`\`\`"
                fi

                echo ""
                echo "---"
                echo ""
            fi
        done

        echo ""
        echo "_Generated on: $(date -u +"%Y-%m-%d %H:%M:%S UTC")_"
    } > "${output}"

    echo "    ✅ Created ${output}"
}

# Function to create API reference index
create_api_index() {
    echo "  Creating API reference index..."

    local output="${DOCS_DIR}/README.md"

    {
        echo "# API Reference"
        echo ""
        echo "_Auto-generated Go API documentation for VirtRigaud_"
        echo ""
        echo "This section contains automatically generated documentation from Go source code."
        echo "The documentation is extracted from GoDoc comments and regenerated on every build."
        echo ""
        echo "## Available References"
        echo ""
        echo "- **[API Types](api-types.md)** - Custom Resource Definitions (CRDs)"
        echo "- **[Provider Contract](provider-contract.md)** - Provider interface specification"
        echo "- **[SDK Reference](sdk.md)** - Provider SDK documentation"
        echo "- **[Utilities](utilities.md)** - Internal utility packages"
        echo ""
        echo "## Manual API References"
        echo ""
        echo "- **[CLI Reference](cli.md)** - Command-line tools"
        echo "- **[Metrics](metrics.md)** - Observability metrics"
        echo ""
        echo "---"
        echo ""
        echo "## Documentation Source"
        echo ""
        echo "All API documentation is automatically generated from:"
        echo ""
        echo "- **CRD Types**: \`api/infra.virtrigaud.io/v1beta1/*_types.go\`"
        echo "- **Provider Contract**: \`internal/providers/contracts/*.go\`"
        echo "- **SDK**: \`sdk/provider/\`"
        echo "- **Utilities**: \`internal/k8s/\`, \`internal/resilience/\`, \`internal/util/\`"
        echo ""
        echo "The source code GoDoc comments are the authoritative documentation."
        echo ""
        echo "---"
        echo ""
        echo "_Last generated: $(date -u +"%Y-%m-%d %H:%M:%S UTC")_"
        echo ""
        echo "## Generating Documentation"
        echo ""
        echo "To regenerate this documentation, run:"
        echo ""
        echo "\`\`\`bash"
        echo "make docs-build"
        echo "\`\`\`"
    } > "${output}"

    echo "    ✅ Created ${output}"
}

# Main execution
main() {
    echo ""
    echo "🔧 VirtRigaud API Documentation Generator"
    echo "=========================================="
    echo ""

    # Generate all documentation
    generate_api_types_doc
    generate_provider_contract_doc
    generate_sdk_doc
    generate_utilities_doc
    create_api_index

    echo ""
    echo "✅ API documentation generated successfully!"
    echo ""
    echo "📂 Output directory: ${DOCS_DIR}"
    echo ""
    echo "The documentation has been generated from Go source code and will be"
    echo "included in the mdBook build."
    echo ""
}

# Run main function
main "$@"
