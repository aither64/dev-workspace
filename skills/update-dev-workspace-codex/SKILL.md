---
name: update-dev-workspace-codex
description: Upgrade dev-workspace's packaged Codex and matching codex-web support, including naming prerequisites, package contracts and native compatibility checks.
---

# Update the packaged Codex runtime

Read AGENTS.md, docs/codex-package.md and the package-switch section of
docs/workspace-portal.md. Accept an explicit version from the parent. For an
unspecified latest version, resolve the latest stable OpenAI Codex release and
an exact llm-agents revision containing it; freeze both for this update. A moving
llm-agents main or matching version text is insufficient evidence.

## Carry the release-based native patch

The runtime owns the raw `packages.x86_64-linux.codex-native` export and its
fixed source receipt. Start a candidate in `aither64/codex` on the exact latest
stable `rust-vVERSION`, never upstream main. Carry only the owned downstream
provider-switching patch and inspect the full release-to-candidate diff and
range-diff from the previous patch. Preserve upstream default-off behavior and
managed/provider/authentication errors. A conflicting or failing carry blocks
the candidate; keep the last accepted moving/release refs and deployed package.

Publish the reviewed candidate's exact commit before selecting it in the runtime
source receipt. Record upstream tag/base, fork SHA/source hash, patch revision,
Cargo hash and frozen llm-agents revision. Reuse `nix/codex-native.nix` and the
recipe's `srcOverride`/`cargoVendor` interfaces; do not copy the Rust recipe or
apply the fork's compatibility patch twice. Preserve daemon/V8/build/layout
adjustments. An unchanged Cargo.lock can retain its hash only with matching
dependency-derivation evidence. Configuration consumes the runtime's raw native
export and keeps no independent fork/source pin.

Complete committed native/builder/launcher source and quick checks, then obtain
independent review before the expensive custom Rust build. Build one reviewed
native output, retain exact roots and source/closure receipts, then update
actual-binary fixtures with the measured executable hash and prepared source.
Run the required `codex-provider-enforcement` flake check as well: native/package
build success is insufficient. Refresh its exact module test inventory when
carrying the patch and require a nonzero complete list before execution. Remove
the producer's documented legacy zero-match command at the next native-input or
release rebuild; that edit changes the embedded native output identity.
Do not use the upstream same-version binary's hash or placeholder fixture values.
Finish dependent fixtures/consumers and obtain final review before acceptance
suites. Publish immutable accepted release refs and advance the dedicated tested
moving patch branch only after its required gates; never rewrite accepted refs
or fork main. This procedure creates no automatic updater or deployment grant.

## Update the dependency first

Read skills/update-codex-web/SKILL.md from the explicit codex-web checkout or
the selected source. Its canonical repository is
git@github.com:aither64/codex-web.git. Resolve that source through the declared
dependency or caller-provided root, not an assumed sibling directory. Follow
that skill for protocol, catalogs and utility isolation before consuming its
published exact commit. Return to the parent for any material scope change.

Update this repository's llm-agents lock to the frozen revision using Nix's
scoped flake update. Inspect transitive input changes and preserve unrelated
inputs. Select the final codex-web commit in flake.nix/flake.lock and the matching
Go pseudo-version/checksum in portal/go.mod and portal/go.sum. Run go mod tidy
to remove superseded checksums and refresh nix/workspace-portal.nix vendorHash
from the final module set. Verify a fresh dependency derivation: a cached
fixed-output result can conceal a stale hash. Keep the existing pin-consistency
check passing. Publish dependencies before consumers.

## Keep package and naming contracts aligned

Evaluate the actual selected custom Codex version and output; inspect the fork
patch and recipe adjustments separately. Verify the shared server
`--allow-host-provider-switching` launch-only flag and its exact target-package
argv in launch markers/probes. Remote terminals keep their existing connection.
Registered workspace naming children consume the exact package's validated
`codex.app_server_argv` after provider projection; see the
[naming launch contract](../../docs/session-preparations.md#naming-and-reservation).
Generic naming launches and valid legacy packages without a declaration use
strict `[]`; standalone and model-discovery children remain strict. Validate actual
child inheritance and reject stale/unpatched executable overrides. Inspect runtime
layout. Keep mkCodexPackage unless upstream satisfies the
documented materialized-resource, daemon, copy and retention contract for every
consumer. A new manifest or closed upstream issue alone does not remove it.

Update naming's exact native version/source prerequisites, model catalog hash
and byte-length assertions, and actual-binary fixtures together. Preserve old
version fixtures that test retained installations. Review the local workspace
protocol corpus as well as the dependency's public client corpus. Do not globally
replace version strings or remove restrictions to accommodate the new release.

Document the supported package/daemon behavior, catalog boundary and recovery
contract in their existing guides. Native daemon selection can differ from the
CLI executable. Inspect actual selected daemon identity independently.

## Verify and hand off

Use declared Nix environments for focused Go/Ruby/browser and package-contract
checks. Record exact selected source/output identities. Complete and commit
intended changes and run independent final review before longer integration
checks. An orchestrating parent may perform this final review across repositories.

After review run applicable packaged checks, native utility tests and managed
naming lifecycle/positive canary checks described by the package contract and
codex-web skill. Test startup and saved-thread resume/fork across versions using
private disposable state; establish downgrade reader support before recommending
an older binary for recovery. Keep retained roots and shared credentials intact.

Return the published runtime and codex-web commits, exact llm-agents/Codex
identities, catalogs, checks and deployment constraints. Host configuration and
application profile selection belong to their consumers. Changed native
invocations require idle-session activation; application recovery is forward-only.
