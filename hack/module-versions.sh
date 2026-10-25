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
# the release version itself, and the replace directives must resolve the
# in-repo modules from the working tree only: the three tags then point at the
# same commit and a consumer gets the code the release was built from. See
# docs/go-modules.md. internal/scaffold/moduleversions_test.go tests this script.
#
# Usage:
#   hack/module-versions.sh validate <version>
#       Exit 0 when <version> is a release version (vX.Y.Z or vX.Y.Z-rcN,
#       -alphaN, -betaN; major 0 or 1).
#   hack/module-versions.sh set <version>
#       Rewrite the in-repo requires of go.mod and sdk/go.mod to <version>
#       (release preparation, before the release tag is pushed).
#   hack/module-versions.sh check <version> [<commit>]
#       Fail unless the three go.mod files of the working tree (or of
#       <commit>) require the in-repo modules at <version> and replace them
#       with the working tree only.
#   hack/module-versions.sh consistent
#       Fail unless the go.mod files of the working tree pass "check" for one
#       release version; print that version.
#   hack/module-versions.sh preflight <version> [--expect-commit <sha>]
#       Read-only release check: "check" against the commit of the existing
#       tag <version> (which must be <sha> when given), and fail when $REMOTE
#       already has a module tag for <version> on another commit.
#   hack/module-versions.sh tag <version> [--push] [--expect-commit <sha>] [sdk] [proto]
#       Create annotated tags <module>/<version> (default: sdk proto) on the
#       commit of the existing tag <version> (which must be <sha> when given),
#       after "check" passes for it. An existing module tag on the same commit
#       is kept; one on another commit is an error: tags are never moved,
#       deleted or overwritten. --push pushes the new tags to $REMOTE (default
#       origin) without --force, after checking the remote has none on another
#       commit.

set -euo pipefail

readonly ROOT_MODULE="github.com/projectbeskar/virtrigaud"
readonly SDK_MODULE="${ROOT_MODULE}/sdk"
readonly PROTO_MODULE="${ROOT_MODULE}/proto"
readonly REMOTE="${REMOTE:-origin}"

# A release version: what the release workflow tags. Major 0 or 1 only: a
# major version of 2 or more needs a /vN suffix on every module path.
# internal/scaffold.releaseVersionPattern is the same expression (a test
# keeps them equal).
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

# go_mod_directives <directive> prints, for every <directive> ("require" or
# "replace") of an in-repo module in the go.mod text on stdin, in both the
# single-line and the block form: "<module> <version>" for a require and
# "<module> <target>" for a replace. Comments are skipped, and so are the
# lines of other blocks (exclude, retract, tool, godebug): outside a require
# or replace block only a line starting with require or replace counts.
go_mod_directives() {
    awk -v root="$ROOT_MODULE" -v want="$1" '
        function inrepo(m) { return m == root || m == root "/sdk" || m == root "/proto" }
        {
            line = $0
            sub(/\/\/.*/, "", line)
            n = split(line, f, /[[:space:]]+/)
            # Drop the empty fields split() makes of leading blanks.
            j = 0
            for (i = 1; i <= n; i++) if (f[i] != "") g[++j] = f[i]
            if (j == 0) next
            if (block != "") {
                if (g[1] == ")") { block = ""; next }
                start = 1
                kind = block
            } else if ((g[1] == "require" || g[1] == "replace") && j >= 2 && g[2] == "(") {
                block = g[1]; next
            } else if (g[1] == "require(" || g[1] == "replace(") {
                block = substr(g[1], 1, 7); next
            } else if ((g[1] == "require" || g[1] == "replace") && j >= 3) {
                start = 2
                kind = g[1]
            } else {
                next
            }
            if (kind != want || !inrepo(g[start])) next
            if (kind == "require") { print g[start], g[start + 1]; next }
            # replace: old [version] => new [version]
            for (k = start + 1; k <= j; k++) if (g[k] == "=>") { print g[start], g[k + 1]; break }
        }
    '
}

# expected_replaces <go.mod path> prints the replace directives the go.mod at
# path must have for the in-repo modules: the working tree, nothing else.
expected_replaces() {
    case "$1" in
    go.mod) printf '%s\n' "${PROTO_MODULE} ./proto" "${SDK_MODULE} ./sdk" ;;
    sdk/go.mod) printf '%s\n' "${ROOT_MODULE} .." "${PROTO_MODULE} ../proto" ;;
    proto/go.mod) ;;
    esac
}

# check_go_mods <version> [<commit>] reports every way the three go.mod files
# break the lockstep rule for <version> on stderr; it returns 1 when any does.
check_go_mods() {
    local version="$1" commit="${2:-}"
    local failed=0 path mod ver content requires replaces
    for path in go.mod sdk/go.mod proto/go.mod; do
        content="$(go_mod_content "$path" "$commit")" || die "cannot read ${path}"
        requires="$(printf '%s\n' "$content" | go_mod_directives require)"
        while read -r mod ver; do
            [[ -z "$mod" ]] && continue
            if [[ "$path" == "proto/go.mod" ]]; then
                echo "module-versions: ${path} requires ${mod} ${ver}; the proto module must not depend on the other VirtRigaud modules" >&2
                failed=1
            elif [[ "$ver" != "$version" ]]; then
                echo "module-versions: ${path} requires ${mod} ${ver}, want ${version}" >&2
                failed=1
            fi
        done <<<"$requires"
        if [[ "$path" == "sdk/go.mod" && "$requires" != *"${PROTO_MODULE} "* ]]; then
            echo "module-versions: sdk/go.mod does not require ${PROTO_MODULE}" >&2
            failed=1
        fi
        # Replace targets, a trailing slash dropped, compared as sorted sets.
        replaces="$(printf '%s\n' "$content" | go_mod_directives replace | sed 's#/$##' | sort)"
        if [[ "$replaces" != "$(expected_replaces "$path" | sort)" ]]; then
            echo "module-versions: ${path} replaces the VirtRigaud modules with [${replaces//$'\n'/; }], want [$(expected_replaces "$path" | sort | tr '\n' ';' | sed 's/;$//; s/;/; /g')]" >&2
            failed=1
        fi
    done
    return "$failed"
}

cmd_check() {
    local version="$1" commit="${2:-}"
    validate_version "$version"
    local where="the working tree"
    [[ -n "$commit" ]] && where="commit ${commit}"
    if ! check_go_mods "$version" "$commit"; then
        die "the go.mod files of ${where} are not consistent with ${version}; run: hack/module-versions.sh set ${version} (see docs/go-modules.md)"
    fi
    echo "module-versions: the go.mod files of ${where} require the VirtRigaud modules at ${version}"
}

cmd_consistent() {
    local version
    version="$(go_mod_content sdk/go.mod | go_mod_directives require | awk -v m="$PROTO_MODULE" '$1 == m { print $2 }')"
    [[ -n "$version" ]] || die "sdk/go.mod does not require ${PROTO_MODULE}"
    validate_version "$version"
    check_go_mods "$version" || die "the go.mod files do not require the VirtRigaud modules at one release version"
    echo "$version"
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

# release_commit <version> <expected sha> prints the commit of the release tag
# <version>, after checking it is <expected sha> when that is given.
release_commit() {
    local version="$1" expect="$2" commit
    commit="$(tag_commit "$version")"
    [[ -n "$commit" ]] || die "tag ${version} does not exist; the module tags go on the commit of the release tag"
    if [[ -n "$expect" && "$commit" != "$expect" ]]; then
        die "tag ${version} is on ${commit}, but this run builds ${expect}; refusing to tag modules of another commit"
    fi
    echo "$commit"
}

# tag_message <module> <version> <commit> prints the annotation of a module
# tag: the release commit and, in GitHub Actions, the run that created it.
tag_message() {
    local module="$1" version="$2" commit="$3"
    printf '%s module %s\n\nRelease commit: %s\n' "$module" "$version" "$commit"
    if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
        printf 'Created by: %s/%s/actions/runs/%s (actor %s)\n' \
            "${GITHUB_SERVER_URL:-https://github.com}" "${GITHUB_REPOSITORY:-}" "$GITHUB_RUN_ID" "${GITHUB_ACTOR:-}"
    fi
}

# parse_tag_args sets version, push, expect and modules from the arguments of
# "preflight" and "tag".
parse_tag_args() {
    version="" push=0 expect="" modules=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
        --push) push=1 ;;
        --expect-commit)
            [[ $# -ge 2 ]] || die "--expect-commit needs a commit"
            expect="$2"
            shift
            ;;
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
}

cmd_preflight() {
    local version push expect modules
    parse_tag_args "$@"
    local commit module existing
    commit="$(release_commit "$version" "$expect")"
    cmd_check "$version" "$commit"
    for module in "${modules[@]}"; do
        existing="$(remote_tag_commit "${module}/${version}")"
        if [[ -n "$existing" && "$existing" != "$commit" ]]; then
            die "${REMOTE} already has ${module}/${version} on ${existing}, not on ${commit}; tags are never moved"
        fi
    done
    echo "module-versions: ${REMOTE} has no ${modules[*]} tag for ${version} on another commit"
}

cmd_tag() {
    local version push expect modules
    parse_tag_args "$@"
    local commit
    commit="$(release_commit "$version" "$expect")"
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
            git tag -a "$tag" -m "$(tag_message "$module" "$version" "$commit")" "$commit"
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
    [[ $# -ge 1 ]] || usage
    local command="$1"
    shift
    case "$command" in
    validate)
        [[ $# -eq 1 ]] || usage
        validate_version "$1"
        ;;
    set)
        [[ $# -eq 1 ]] || usage
        cmd_set "$1"
        ;;
    check)
        [[ $# -ge 1 && $# -le 2 ]] || usage
        cmd_check "$@"
        ;;
    consistent)
        [[ $# -eq 0 ]] || usage
        cmd_consistent
        ;;
    preflight) cmd_preflight "$@" ;;
    tag) cmd_tag "$@" ;;
    *) usage ;;
    esac
}

main "$@"
