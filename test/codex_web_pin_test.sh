#!/usr/bin/env bash
set -euo pipefail

checker=$1
jq_bin=$2
expected=$3
version=v0.0.0-20260929193321-$expected
wrong=aaaaaaaaaaaa
[ "$expected" = "$wrong" ] && wrong=bbbbbbbbbbbb
fixture_dir=$(mktemp -d)
trap 'rm -rf "$fixture_dir"' EXIT
fixture=$fixture_dir/go.mod

write_fixture() {
  cat > "$fixture" <<EOF
module example.com/pin-contract

go 1.24.0

require github.com/aither64/codex-web $1
$2
EOF
}

accept() {
  if ! bash "$checker" "$fixture" "$expected" "$jq_bin"; then
    echo "expected pin fixture to pass" >&2
    exit 1
  fi
}

reject() {
  if bash "$checker" "$fixture" "$expected" "$jq_bin" >/dev/null 2>&1; then
    echo "expected pin fixture to fail" >&2
    exit 1
  fi
}

write_fixture "$version" ""
accept
write_fixture "v0.0.0-20260929190212-$wrong" \
  "// github.com/aither64/codex-web $version"
reject
write_fixture "$version" \
  "replace github.com/aither64/codex-web => example.com/fork v1.0.0"
reject
write_fixture "$version" \
  "replace github.com/aither64/codex-web $version => example.com/fork v1.0.0"
reject
write_fixture "$version" \
  "replace github.com/aither64/codex-web v0.0.0-20260929190212-$wrong => example.com/fork v1.0.0"
accept
write_fixture "$version" "exclude github.com/aither64/codex-web $version"
reject

echo "codex-web pin contracts passed"
