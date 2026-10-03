#!/usr/bin/env bash
set -euo pipefail

# Run only in the declared repository Nix environment, after change review.
# This touches disposable files, never a serving profile or a real session.
source_root=$(cd "$(dirname "$0")/.." && pwd)
baseline=924c0ec28c41dd8b56aaf17f2212b302ca614899
fixture_root=$(mktemp -d)
trap 'rm -rf "$fixture_root"' EXIT
export PREPARATION_FIXTURE_ROOT="$fixture_root/state"
mkdir -p "$PREPARATION_FIXTURE_ROOT" "$fixture_root/baseline"
git -C "$source_root" archive "$baseline" portal | tar -x -C "$fixture_root/baseline"
cp "$source_root/test/fixtures/preparation_baseline_test.go" "$fixture_root/baseline/portal/internal/uploads/preparation_baseline_test.go"

go -C "$source_root/portal" test -mod=readonly -tags=preparation_compatibility ./internal/web -run '^TestPreparationCompatibilityFixtureWrite$' -count=1
go -C "$fixture_root/baseline/portal" test -mod=readonly -tags=preparation_compatibility ./internal/uploads -run '^TestPreparationBaselineFixture$' -count=1
go -C "$source_root/portal" test -mod=readonly -tags=preparation_compatibility ./internal/web -run '^TestPreparationCompatibilityFixtureRecover$' -count=1
