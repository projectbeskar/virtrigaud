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
#
# module-versions.sh keeps the three Go modules of this repository in lockstep
# and tags the sdk and proto modules of a release.
#
#   github.com/projectbeskar/virtrigaud        (go.mod)        tag vX.Y.Z
#   github.com/projectbeskar/virtrigaud/sdk    (sdk/go.mod)    tag sdk/vX.Y.Z
#   github.com/projectbeskar/virtrigaud/proto  (proto/go.mod)  tag proto/vX.Y.Z
#
# The root and sdk modules import each other's packages and both import proto.
# The replace directives in go.mod and sdk/go.mod only apply inside this
# repository; a consumer ignores them and resolves every require from the
# tags. So at a release commit every require of an in-repo module must name
# the release version itself: the three tags then point at the same commit and
# a consumer gets the code the release was built and tested from. See
# docs/go-modules.md.
#
# Usage:
#   hack/module-versions.sh validate <version>
#       Exit 0 when <version> is a release version (vX.Y.Z or vX.Y.Z-rcN,
#       -alphaN, -betaN; major 0 or 1).
#   hack/module-versions.sh set <version>
#       Rewrite the in-repo requires of go.mod and sdk/go.mod to <version>
#       (release preparation, before the release tag is pushed).
#   hack/module-versions.sh check <version> [<commit>]
#       Fail unless every in-repo require in the three go.mod files of the
#       working tree (or of <commit>) names <version>.
#   hack/module-versions.sh tag <version> [--push] [<module>...]
#       Create annotated tags <module>/<version> (default: sdk proto) on the
#       commit of the existing tag <version>, after "check" passes for it.
#       An existing module tag on the same commit is kept; one on another
#       commit is an error: tags are never moved, deleted or overwritten.
#       --push pushes the new tags to $REMOTE (default origin) without
#       --force, after checking the remote has none on another commit.

set -euo pipefail

readonly ROOT_MODULE="github.com/projectbeskar/virtrigaud"
readonly SDK_MODULE="${ROOT_MODULE}/sdk"
readonly PROTO_MODULE="${ROOT_MODULE}/proto"
readonly REMOTE="${REMOTE:-origin}"

# A release version: what the release workflow tags. Major 0 or 1 only: a
# major version of 2 or more needs a /vN suffix on every module path.
readonly RELEASE_VERSION_RE='^v(0|1)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.?(0|[1-9][0-9]*))?$'

die() {
    echo "module-versions: $*" >&2
    exit 1
}

usage() {
    sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//' >&2
    exit 2
}

repo_root() {
    git rev-parse --show-toplevel
}

validate_version() {
    local version="$1"
    [[ "$version" =~ $RELEASE_VERSION_RE ]] ||
        die "\"${version}\" is not a release version (want vX.Y.Z or vX.Y.Z-rcN with major 0 or 1)"
}

# go_mod_content <relative go.mod path> [<commit>] prints the go.mod of the
# working tree, or of <commit> when given.
go_mod_content() {
    local path="$1" commit="${2:-}"
    if [[ -n "$commit" ]]; then
        git -C "$(repo_root)" show "${commit}:${path}"
    else
        cat "$(repo_root)/${path}"
    fi
}

# in_repo_requires prints "<module> <version>" for every require of an in-repo
# module in the go.mod text on stdin, in both the single-line and the block
# form. Replace directives are not requires and are skipped.
in_repo_requires() {
    awk -v root="$ROOT_MODULE" '
        /^[[:space:]]*\/\// { next }
        /^require[[:space:]]*\(/ { inblock = 1; next }
        inblock && /^[[:space:]]*\)/ { inblock = 0; next }
        {
            line = $0
            sub(/\/\/.*/, "", line)
            n = split(line, f, /[[:space:]]+/)
            # Drop the empty field split() makes of leading blanks.
            j = 0
            for (i = 1; i <= n; i++) if (f[i] != "") g[++j] = f[i]
            if (inblock && j >= 2) { mod = g[1]; ver = g[2] }
            else if (!inblock && j >= 3 && g[1] == "require") { mod = g[2]; ver = g[3] }
            else next
            if (mod == root || mod == root "/sdk" || mod == root "/proto") print mod, ver
        }
    '
}

cmd_check() {
    local version="$1" commit="${2:-}"
    validate_version "$version"
    local where="the working tree"
    [[ -n "$commit" ]] && where="commit ${commit}"

    local failed=0 path mod ver
    for path in go.mod sdk/go.mod proto/go.mod; do
        local content
        content="$(go_mod_content "$path" "$commit")" || die "cannot read ${path} of ${where}"
        while read -r mod ver; do
            [[ -z "$mod" ]] && continue
            if [[ "$path" == "proto/go.mod" ]]; then
                echo "module-versions: ${path} requires ${mod} ${ver}; the proto module must not depend on the other VirtRigaud modules" >&2
                failed=1
            elif [[ "$ver" != "$version" ]]; then
                echo "module-versions: ${path} requires ${mod} ${ver}, want ${version}" >&2
                failed=1
            fi
        done < <(printf '%s\n' "$content" | in_repo_requires)
    done
    # The sdk must require proto: the published sdk module imports it.
    if ! go_mod_content sdk/go.mod "$commit" | in_repo_requires | grep -q "^${PROTO_MODULE} "; then
        echo "module-versions: sdk/go.mod does not require ${PROTO_MODULE}" >&2
        failed=1
    fi
    if [[ "$failed" -ne 0 ]]; then
        die "the go.mod files of ${where} are not consistent with ${version}; run: hack/module-versions.sh set ${version} (see docs/go-modules.md)"
    fi
    echo "module-versions: the go.mod files of ${where} require the VirtRigaud modules at ${version}"
}

cmd_set() {
    local version="$1"
    validate_version "$version"
    local root
    root="$(repo_root)"
    # Requires of the main module itself are not allowed, so each go.mod gets
    # the other two modules only. go mod edit does not touch the network.
    (cd "$root" && go mod edit -require="${PROTO_MODULE}@${version}" -require="${SDK_MODULE}@${version}")
    (cd "$root/sdk" && go mod edit -require="${ROOT_MODULE}@${version}" -require="${PROTO_MODULE}@${version}")
    cmd_check "$version"
}

# tag_commit <tag> prints the commit a local tag points at, or nothing.
tag_commit() {
    git rev-parse -q --verify "refs/tags/$1^{commit}" 2>/dev/null || true
}

# remote_tag_commit <tag> prints the commit a tag on $REMOTE points at (the
# peeled commit of an annotated tag), or nothing when the remote has no such tag.
remote_tag_commit() {
    local tag="$1" out
    out="$(git ls-remote --tags "$REMOTE" "refs/tags/${tag}" "refs/tags/${tag}^{}")" ||
        die "cannot list the tags of remote ${REMOTE}"
    # An annotated tag lists the tag object, then the peeled commit (^{}).
    local peeled plain
    peeled="$(awk -v r="refs/tags/${tag}^{}" '$2 == r { print $1 }' <<<"$out")"
    plain="$(awk -v r="refs/tags/${tag}" '$2 == r { print $1 }' <<<"$out")"
    if [[ -n "$peeled" ]]; then
        echo "$peeled"
    else
        echo "$plain"
    fi
}

cmd_tag() {
    local version="" push=0 modules=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
        --push) push=1 ;;
        sdk | proto) modules+=("$1") ;;
        -*) die "unknown flag $1" ;;
        *)
            [[ -z "$version" ]] || die "unexpected argument $1"
            version="$1"
            ;;
        esac
        shift
    done
    [[ -n "$version" ]] || usage
    validate_version "$version"
    [[ ${#modules[@]} -gt 0 ]] || modules=(sdk proto)

    local commit
    commit="$(tag_commit "$version")"
    [[ -n "$commit" ]] || die "tag ${version} does not exist; the module tags go on the commit of the release tag"
    cmd_check "$version" "$commit"

    local to_push=() module tag existing
    for module in "${modules[@]}"; do
        tag="${module}/${version}"
        if [[ "$push" -eq 1 ]]; then
            existing="$(remote_tag_commit "$tag")"
            if [[ -n "$existing" && "$existing" != "$commit" ]]; then
                die "${REMOTE} already has ${tag} on ${existing}, not on ${commit}; tags are never moved"
            fi
            if [[ -n "$existing" ]]; then
                echo "module-versions: ${REMOTE} already has ${tag} on ${commit}; nothing to do"
                continue
            fi
        fi
        existing="$(tag_commit "$tag")"
        if [[ -n "$existing" && "$existing" != "$commit" ]]; then
            die "tag ${tag} already exists on ${existing}, not on ${commit}; tags are never moved"
        fi
        if [[ -z "$existing" ]]; then
            git tag -a "$tag" -m "${module} module ${version}" "$commit"
            echo "module-versions: tagged ${tag} on ${commit}"
        else
            echo "module-versions: ${tag} already on ${commit}"
        fi
        to_push+=("refs/tags/${tag}")
    done

    if [[ "$push" -eq 1 && ${#to_push[@]} -gt 0 ]]; then
        # No --force: git refuses to replace a tag the remote already has.
        # --atomic: either every module tag lands or none does.
        git push --atomic "$REMOTE" "${to_push[@]}"
        echo "module-versions: pushed ${to_push[*]} to ${REMOTE}"
    fi
}

main() {
    [[ $# -ge 2 ]] || usage
    local command="$1"
    shift
    case "$command" in
    validate) validate_version "$1" ;;
    set) cmd_set "$1" ;;
    check) cmd_check "$@" ;;
    tag) cmd_tag "$@" ;;
    *) usage ;;
    esac
}

main "$@"
