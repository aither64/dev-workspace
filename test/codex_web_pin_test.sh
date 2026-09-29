#!/usr/bin/env bash
set -euo pipefail

checker=$1
jq_bin=$2
expected=$3
wrong=aaaaaaaaaaaa
wrong_timestamp=v0.0.0-20200101000000-${expected##*-}
[ "$wrong_timestamp" = "$expected" ] && wrong_timestamp=v0.0.0-20210101000000-${expected##*-}
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

write_fixture "$expected" ""
accept
write_fixture "v0.0.0-20260929190212-$wrong" \
  "// github.com/aither64/codex-web $expected"
reject
write_fixture "$wrong_timestamp" ""
reject
write_fixture "$expected" \
  "replace github.com/aither64/codex-web => example.com/fork v1.0.0"
reject
write_fixture "$expected" \
  "replace github.com/aither64/codex-web $expected => example.com/fork v1.0.0"
reject
write_fixture "$expected" \
  "replace github.com/aither64/codex-web v0.0.0-20260929190212-$wrong => example.com/fork v1.0.0"
accept
write_fixture "$expected" "replace example.com/unrelated => example.com/fork v1.0.0"
accept
write_fixture "$expected" "exclude github.com/aither64/codex-web $expected"
reject

echo "codex-web pin contracts passed"
