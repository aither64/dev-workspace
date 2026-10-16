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

Evaluate the actual selected Codex version and output; inspect downstream
patches and runtime layout. Keep mkCodexPackage unless upstream satisfies the
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
