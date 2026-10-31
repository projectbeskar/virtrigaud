# Go modules and their release tags

This repository holds three Go modules. From v0.4.1 on, every VirtRigaud release
publishes all three under their own tags on the same commit, so a provider
written outside this repository can require them like any other Go dependency,
without a local checkout or `replace` directives.

| Module | Directory | Release tag | Contents |
|---|---|---|---|
| `github.com/projectbeskar/virtrigaud` | `/` | `vX.Y.Z` | The operator, the providers and the CLIs |
| `github.com/projectbeskar/virtrigaud/sdk` | `sdk/` | `sdk/vX.Y.Z` | The provider SDK: gRPC server, middleware, capabilities, errors, client |
| `github.com/projectbeskar/virtrigaud/proto` | `proto/` | `proto/vX.Y.Z` | The provider gRPC contract (`provider.proto`) and its generated Go bindings |

`sdk/vX.Y.Z` and `proto/vX.Y.Z` follow the Go convention for a module in a
subdirectory: the tag is the directory followed by the version. The root
module's zip does not contain `sdk/` or `proto/` (a directory with its own
`go.mod` belongs to that module).

Releases up to and including v0.4.0 have no `sdk/` or `proto/` tags:
`go get github.com/projectbeskar/virtrigaud/sdk@v0.4.0` fails with
`module github.com/projectbeskar/virtrigaud@v0.4.0 found, but does not contain package github.com/projectbeskar/virtrigaud/sdk`.

## Using the modules

```bash
go get github.com/projectbeskar/virtrigaud/sdk@vX.Y.Z github.com/projectbeskar/virtrigaud/proto@vX.Y.Z
```

Use the same version for both. The SDK requires the root module and the proto
module at its own version, so `go get` of the SDK alone selects all three at
that version unless your module requires a newer one. The modules need the Go
version of the release's `go.mod` (`go 1.26.10` today); with the default
`GOTOOLCHAIN=auto` the `go` command downloads it.

`vrtg-provider init` from a release writes these requires for you, pinned to
its own version: see [`docs/cli-tools.md`](cli-tools.md#vrtg-provider).

Release candidates are tagged too (`sdk/v0.4.1-rc1`). The `go` command never
selects a prerelease for `@latest` once a release of that module exists, so an
rc tag affects only the projects that ask for it.

`go install github.com/projectbeskar/virtrigaud/cmd/<tool>@vX.Y.Z` does not
work: the root `go.mod` has `replace` directives (below), which `go install
<package>@<version>` refuses. Download the CLI binaries from the GitHub release
instead.

## How the modules depend on each other

```
root  ──requires──▶  sdk    (the providers use sdk/provider/errors, ...)
sdk   ──requires──▶  root   (sdk/provider/server imports internal/obs/health and internal/version)
root  ──requires──▶  proto
sdk   ──requires──▶  proto
proto ──requires──▶  (no VirtRigaud module)
```

Root and SDK require each other. Go allows a cycle between modules (not between
packages, and there is none). The SDK may import the root module's `internal/`
packages because Go decides `internal/` visibility by import path, and the
SDK's import paths are under `github.com/projectbeskar/virtrigaud/`.

Inside this repository, `go.mod` and `sdk/go.mod` have `replace` directives
that resolve the other modules from the working tree, so a build always uses
the code next to it. A consumer **ignores the `replace` directives of its
dependencies**: it resolves every `require` from the published tags. So every
`require` of an in-repo module must name a version that exists, and the code
of that version must be what the release was built and tested with.

## The rule: lockstep versions on the release commit

At the commit a release tag points at:

- `go.mod` requires `.../sdk` and `.../proto` at the release version and
  replaces them with `./sdk` and `./proto`, nothing else;
- `sdk/go.mod` requires the root module and `.../proto` at the release version
  and replaces them with `../` and `../proto`, nothing else;
- `proto/go.mod` requires no VirtRigaud module.

All three tags point at that commit, so every version a consumer resolves
exists and is the code the release was built from. No commit is made at
release time: the versions are set in the release preparation, before the tag
is pushed, and the `replace` directives make the not-yet-existing versions
irrelevant inside the repository in the meantime. Once a release preparation
is merged, the in-repo requires on `main` name that release; until then they
name the previous one.

Only release tags are supported versions of the SDK and proto modules. A
pseudo-version of an arbitrary commit (`go get .../sdk@<commit>`) resolves the
root and proto modules at whatever release that commit's `sdk/go.mod` names,
not at the same commit.

`hack/module-versions.sh` implements the rule
(`internal/scaffold/moduleversions_test.go` tests it, and checks on every
`go test ./...` that the repository's own `go.mod` files name one release
version):

| Command | Make target | What it does |
|---|---|---|
| `validate <v>` | | Exit 0 when `<v>` is a release version: `vX.Y.Z`, optionally `-rcN` (or `-rc.N`), `-alphaN` or `-betaN`, major 0 or 1 (a major version of 2 or more would need a `/vN` module path). `dev` and `git describe` output are not. |
| `set <v>` | `make set-module-versions VERSION=<v>` | Rewrites the in-repo requires of `go.mod` and `sdk/go.mod` to `<v>` (`go mod edit`; no network) |
| `check <v> [<commit>]` | `make check-module-versions VERSION=<v>` | Fails unless the three `go.mod` files of the working tree (or `<commit>`) follow the rule for `<v>` |
| `consistent` | | Fails unless the working tree's `go.mod` files follow the rule for one release version; prints it |
| `preflight <v> [--expect-commit <sha>]` | | Read-only: `check` against the commit of the existing tag `<v>` (which must be `<sha>`), and fails when the remote already has `sdk/<v>` or `proto/<v>` on another commit |
| `tag <v> [--push] [--expect-commit <sha>] [sdk] [proto]` | `make release-go-modules VERSION=<v>` (`release-sdk`, `release-proto` for one) | Runs `check` against the commit of the existing tag `<v>` (which must be `<sha>`), then creates annotated tags `sdk/<v>` and `proto/<v>` on that commit; `--push` pushes them |

The make targets refuse to run without an explicit `VERSION` (the Makefile's
`git describe` default is not a release), and only create local tags.

Go compares prerelease identifiers as text, so `-rc10` sorts before `-rc2`. A
release that may need ten or more candidates should use `-rc.N` (`-rc.2`,
`-rc.10`), which sorts numerically.

## Releasing

1. In the release preparation pull request (the one that carries the release
   notes), set the versions and commit the two `go.mod` files:

   ```bash
   make set-module-versions VERSION=v0.4.2
   ```

   Do this for every release candidate and for the final release.
2. Before pushing the tag, on the commit to be tagged:

   ```bash
   make check-module-versions VERSION=v0.4.2
   ```

3. Push the release tag as usual (`git tag v0.4.2 && git push origin v0.4.2`).
   To re-run a release by hand (`workflow_dispatch`), start the workflow from
   the tag ("Use workflow from: v0.4.2"), not from a branch: the run
   refuses to build a commit other than the tag's.

The release workflow (`.github/workflows/release.yml`) then:

- in `prepare` (`contents: read` only), checks out the tag, checks that the run
  builds the tag's commit, and runs `hack/module-versions.sh preflight`. A tag
  whose `go.mod` files do not follow the rule, that is not a release version,
  or whose module tags already exist on another commit, fails the release
  before anything is built or published;
- after the GitHub Release is created, runs the `tag-go-modules` job:
  `hack/module-versions.sh tag <tag> --push --expect-commit <commit>` creates
  `sdk/<tag>` and `proto/<tag>` on the commit of `<tag>`, the one every job of
  the run built, and pushes them. The job has only `contents: write`. The tags
  are pushed with `GITHUB_TOKEN`, which starts no workflow run, and neither
  `sdk/...` nor `proto/...` matches the workflow's `v*` trigger. The tag
  annotation names the release commit and the workflow run.

If `prepare` fails on the module check, nothing has been built or published,
but the release tag itself is public, and it is a version of the root module:
the Go proxy may already have fetched it. **Do not reuse the version.** Leave
the tag, fix the `go.mod` files in a pull request, and release the next
version (for example `-rc2`). Delete and re-create the tag only if you are
certain nothing fetched it.

If `tag-go-modules` fails, the GitHub Release and its `vrtg-provider` binaries
are already public, and a project that binary scaffolds does not resolve until
the module tags exist. Re-run the failed job: it is idempotent and pushes only
the tags that are missing. Or, from a checkout with the release tag, run
`make release-go-modules VERSION=<tag>` and push the two tags without
`--force` (`git push --atomic origin refs/tags/sdk/<tag> refs/tags/proto/<tag>`).

## Tags are never moved

The Go checksum database (`sum.golang.org`) records the content of a module
version the first time anyone fetches it, and every later download is checked
against that record. A moved or re-created `vX.Y.Z`, `sdk/` or `proto/` tag
would make that version fail verification for everyone. So:

- `hack/module-versions.sh tag` keeps a module tag that already points at the
  release commit (a re-run of the job does nothing) and fails, without
  changing anything, when one points at another commit, locally or on the
  remote, or when the release tag is not on the commit the run built;
- it pushes without `--force`, so `git` itself also refuses to replace a tag
  the remote already has; `--atomic` makes the push all or nothing;
- a broken module version is fixed with a new release, never by re-tagging. A
  later release can mark it with a `retract` directive in the module's
  `go.mod`.

These are client-side safeguards. **Required repository setting**, the
server-side enforcement: a tag ruleset on `refs/tags/v*`, `refs/tags/sdk/v*`
and `refs/tags/proto/v*` with "Restrict updates" and "Restrict deletions", and
an empty bypass list (no bypass for repository administrators either). Do not
enable "Restrict creations" on these patterns: rulesets apply to
`GITHUB_TOKEN`, so it would stop the release workflow from creating the module
tags.

## Verifying a release

After the release workflow finished for `<tag>` (for example `v0.4.2`):

1. The three tags exist and point at the same commit:

   ```bash
   git ls-remote --tags https://github.com/projectbeskar/virtrigaud \
     "refs/tags/<tag>^{}" "refs/tags/sdk/<tag>^{}" "refs/tags/proto/<tag>^{}"
   ```

   All three lines show the same commit.
2. The `go` command finds the versions, directly and through the public proxy
   and checksum database:

   ```bash
   GOPROXY=direct go list -m -versions \
     github.com/projectbeskar/virtrigaud/sdk github.com/projectbeskar/virtrigaud/proto
   GOPROXY=https://proxy.golang.org go list -m \
     github.com/projectbeskar/virtrigaud/sdk@<tag> github.com/projectbeskar/virtrigaud/proto@<tag>
   ```

3. A module outside the repository builds against them, with no `replace`
   directive:

   ```bash
   mkdir /tmp/consumer && cd /tmp/consumer && go mod init example.com/consumer
   cat > main.go <<'EOF'
   package main

   import (
   	"fmt"

   	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
   	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
   )

   func main() {
   	var _ providerv1.ProviderServer
   	fmt.Println(server.DefaultConfig() != nil)
   }
   EOF
   go get github.com/projectbeskar/virtrigaud/sdk@<tag>
   go mod tidy && go build ./... && go vet ./... && go mod verify
   go list -m all | grep projectbeskar   # all three at <tag>
   ```

4. The release's `vrtg-provider` scaffolds a project that builds on its own:
   download `vrtg-provider-<os>-<arch>` from the GitHub release, then, outside
   any checkout:

   ```bash
   ./vrtg-provider-linux-amd64 init demo -o /tmp/author
   cd /tmp/author/providers/demo
   grep projectbeskar go.mod              # the three modules at <tag>, no replace
   go mod tidy && make build && make test
   docker build -t provider-demo:test .   # builds from the project directory alone
   ```

## Before v0.4.1

`vrtg-provider init` resolved the SDK and proto modules only from a local
checkout through `replace` directives, and the generated `Dockerfile` could not
build on its own. `sdk/go.mod` required the root module at `v0.1.0` (an old
release) and `github.com/projectbeskar/virtrigaud/proto v0.1.0`, which was never
tagged, and `go.mod` required the SDK at a placeholder pseudo-version
(`v0.0.0-00010101000000-000000000000`), so even a pseudo-version of the SDK
(`go get .../sdk@<commit>`) failed with `unknown revision proto/v0.1.0`.
