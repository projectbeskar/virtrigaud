#!/usr/bin/env bash
# Spike 1B: build the experiment manager image (REV, or the working tree with
# SOURCE=worktree, + manager-experiment.patch,
# applied to an exported copy, never to the working tree) and the test-only fake
# provider image, and load both into the kind cluster.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
REV="${REV:-ed74c8fee01c1b88168b3d96597ed873949196b6}"
CLUSTER="${CLUSTER:-virtrigaud-spike1b}"
MANAGER_IMAGE="${MANAGER_IMAGE:-spike1b.local/manager:exp}"
FAKE_IMAGE="${FAKE_IMAGE:-spike1b.local/fakeprovider:dev}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# SOURCE=worktree builds the manager from the working tree's tracked files
# (uncommitted changes included) instead of REV.
if [[ "${SOURCE:-rev}" == "worktree" ]]; then
	git ls-files -z | tar --null -T - -c | tar -x -C "$WORK"
	REV="worktree@$(git rev-parse --short HEAD)"
else
	git archive "$REV" | tar -x -C "$WORK"
fi
(cd "$WORK" && patch -p1 --quiet <"$OLDPWD/hack/bench/spike1b/manager-experiment.patch")
docker build -q -f "$WORK/build/Dockerfile.manager" -t "$MANAGER_IMAGE" \
	--build-arg VERSION=spike1b-exp --build-arg GIT_SHA="$REV" "$WORK"
docker build -q -f hack/bench/spike1b/Dockerfile.fakeprovider -t "$FAKE_IMAGE" .
if kind get clusters | grep -qx "$CLUSTER"; then
	kind load docker-image --name "$CLUSTER" "$MANAGER_IMAGE" "$FAKE_IMAGE"
fi
echo "built $MANAGER_IMAGE (rev $REV + experiment patch) and $FAKE_IMAGE"
