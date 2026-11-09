"""Prepare the release tag's stale local versions without resolving dependencies."""

import copy
import hashlib
import json
import pathlib
import re
import sys
import tomllib

root = pathlib.Path(sys.argv[1])
version = sys.argv[2]
lock_path = root / "Cargo.lock"
original = lock_path.read_bytes()
before = tomllib.loads(original.decode())
workspace = tomllib.loads((root / "Cargo.toml").read_text())["workspace"]
assert workspace["package"]["version"] == version
manifests = {}
for path in root.rglob("Cargo.toml"):
    package = tomllib.loads(path.read_text()).get("package")
    if package:
        name = package["name"]
        assert name not in manifests, name
        manifests[name] = package
local = [package for package in before["package"] if "source" not in package]
assert len(local) == 171
for package in local:
    assert package["version"] == "0.0.0", package["name"]
    assert manifests[package["name"]]["version"] == {"workspace": True}
# An explicit old local-version edge needs a separately reviewed rewrite.
for package in before["package"]:
    for edge in package.get("dependencies", []):
        assert not any(edge.startswith(p["name"] + " 0.0.0") for p in local), edge

blocks = original.decode().split("[[package]]")
changed = 0
for index, block in enumerate(blocks[1:], 1):
    package = tomllib.loads("[[package]]" + block)["package"][0]
    if "source" not in package:
        blocks[index], count = re.subn(
            r'^version = "0\.0\.0"$', f'version = "{version}"', block, flags=re.M
        )
        assert count == 1
        changed += count
normalized = "[[package]]".join(blocks).encode()
expected = copy.deepcopy(before)
for package in expected["package"]:
    if "source" not in package:
        package["version"] = version
assert changed == 171
assert tomllib.loads(normalized.decode()) == expected
lock_path.write_bytes(normalized)
pathlib.Path(sys.argv[3]).write_text(json.dumps({
    "changedLocalVersions": changed,
    "originalSHA256": hashlib.sha256(original).hexdigest(),
    "preparedSHA256": hashlib.sha256(normalized).hexdigest(),
    "version": version,
    "externalPackagesAndDependencyEdgesUnchanged": True,
}, sort_keys=True) + "\n")
