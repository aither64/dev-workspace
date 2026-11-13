#!/usr/bin/env bash
set -euo pipefail

# Run only in the declared repository Nix environment within the released check scope.
# This touches disposable files, never a serving profile or a real session.
source_root=$(cd "$(dirname "$0")/.." && pwd)
baseline=924c0ec28c41dd8b56aaf17f2212b302ca614899
fixture_root=$(mktemp -d)
trap 'rm -rf "$fixture_root"' EXIT
export PREPARATION_FIXTURE_ROOT="$fixture_root/state"
mkdir -p "$PREPARATION_FIXTURE_ROOT" "$fixture_root/baseline"
git -C "$source_root" archive "$baseline" portal | tar -x -C "$fixture_root/baseline"
cp "$source_root/test/fixtures/preparation_baseline_test.go" "$fixture_root/baseline/portal/internal/uploads/preparation_baseline_test.go"

if [[ "${1:-}" != "--reader-only" ]]; then
go -C "$source_root/portal" test -mod=readonly -tags=preparation_compatibility ./internal/web -run '^TestPreparationCompatibilityFixtureWrite$' -count=1
go -C "$fixture_root/baseline/portal" test -mod=readonly -tags=preparation_compatibility ./internal/uploads -run '^TestPreparationBaselineFixture$' -count=1
go -C "$source_root/portal" test -mod=readonly -tags=preparation_compatibility ./internal/web -run '^TestPreparationCompatibilityFixtureRecover$' -count=1
fi

reader_baseline=e0925346dae8e862df518b3fecc6e857561c1000
mkdir -p "$fixture_root/e092" "$fixture_root/reader"
git -C "$source_root" archive "$reader_baseline" portal | tar -x -C "$fixture_root/e092"
cp "$source_root/test/fixtures/preparation_e092_baseline_test.go" "$fixture_root/e092/portal/internal/web/preparation_e092_baseline_test.go"
export PREPARATION_FIXTURE_ROOT="$fixture_root/reader"
go -C "$source_root/portal" test -mod=readonly -tags=preparation_compatibility ./internal/web -run '^TestPreparationE092ReaderFixtureWrite$' -count=1
git -C "$source_root" rev-parse "$reader_baseline^{tree}"
sha256sum "$source_root/test/fixtures/preparation_e092_baseline_test.go" "$PREPARATION_FIXTURE_ROOT/"*.json
go -C "$fixture_root/e092/portal" test -mod=readonly -tags=preparation_compatibility ./internal/web -run '^TestPreparationE092ReaderFixture$' -count=1 -v
