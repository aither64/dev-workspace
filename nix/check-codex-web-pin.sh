#!/usr/bin/env bash
set -euo pipefail

module_file=$1
expected=$2
jq_bin=$3

if ! go mod edit -json "$module_file" | "$jq_bin" -e \
  --arg module github.com/aither64/codex-web --arg expected "$expected" '
    [.Require[]? | select(.Path == $module)] as $required |
    if ($required | length) != 1 then false
    else
      $required[0].Version as $version |
      ($version == $expected) and
      ([.Replace[]? | select(.Old.Path == $module and
        ((.Old.Version // "") == "" or .Old.Version == $version))] | length == 0) and
      ([.Exclude[]? | select(.Path == $module and .Version == $version)] | length == 0)
    end
  ' >/dev/null; then
  echo "$module_file does not select codex-web version $expected" >&2
  exit 1
fi
