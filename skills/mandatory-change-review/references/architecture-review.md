# Architecture and Repetition Review

Review ownership, component boundaries, abstractions, extensibility, repeated
logic, and cross-project reuse.

## Component Design

- Check whether the design fits existing architecture across vpsAdmin,
  vpsAdminOS, HaveAPI, clients, test infrastructure, and configuration
  repositories.
- Flag catalogs or registries that mirror classes, resources, actions, plugins,
  handlers, or protocols already declared elsewhere. Prefer one owning
  declaration with derived indexes. Allow finite protocol, state, or public
  contract registries when they are intentionally authoritative.
- Check whether adding, renaming, or removing one implementation is localized
  to its owner. Flag extension points requiring coordinated edits across
  unrelated class-name lists, conditionals, or metadata tables, especially when
  omissions silently disable or misclassify behavior. Require duplicate and
  conflict detection plus bidirectional coverage when metadata cannot be
  derived.
- Flag callbacks, closures, reflection, `instance_exec`, `send`, or method-name
  conventions that create a hidden interface on an unrelated receiver. Prefer
  an explicit class/module interface or a validated DSL with one evaluation
  context. Allow reflective framework boundaries when their contract and
  validation are clear.
- Flag changed modules that combine unrelated orchestration, persistence,
  policy, validation, configuration, rendering, and transport responsibilities
  when this causes central conditionals or shotgun surgery. File length alone
  is not a finding.
- Flag defensive shape or capability probing such as Ruby `respond_to?`, PHP
  `method_exists`/`property_exists`, Python `hasattr`, reflection checks,
  optional chaining that masks uncertain data, or helpers trying several input
  shapes unless a concrete boundary requires it. Prefer boundary validation and
  normalization followed by explicit internal contracts.

## Cross-Project Components and Consumers

When a component is reused or exposes a cross-project interface:

1. Identify the owning component and derive actual consumers from imports,
   dependency pins, wrappers, manifests, generated clients, documentation, and
   current repository state. Do not rely on a hard-coded consumer catalog.
2. Inspect the public contract and representative consumers at the revisions
   they actually pin, plus companion feature branches supplied in the review
   packet.
3. Decide whether the capability belongs in the shared provider, in a consumer,
   or behind an intentional extension point. Check that the design is not
   overfit to the initiating consumer.
4. Look for equivalent downstream implementations or needs that a shared
   abstraction could solve once. Also reject speculative generalization when
   consumers have genuinely different semantics.
5. Require current-scope fixes for broken consumers, unsafe compatibility, or
   incorrect ownership. Record optional adoption by other consumers as a
   concrete follow-up unless it is necessary for correctness.
6. Require provider-level tests and representative consumer validation when the
   shared contract changes. Record any dependency-pin updates and deployment or
   upgrade ordering.

Treat the vpsAdminOS test framework as a canonical example: changes to its
flake-exported test interface, Ruby runner CLI, extension hooks, test metadata,
or machine/test contracts require discovery and inspection of external suites,
wrappers, and pins such as those used by vpsAdmin and other workspace projects.

## Repeated Code and Rules

- Search changed code, adjacent implementation, and relevant consumers for
  repeated behavioral rules, policy, validation, normalization, query logic,
  protocol handling, error translation, orchestration, and test
  infrastructure.
- Require one owning abstraction when repeated logic must stay equivalent and
  has a credible drift, omission, or inconsistent-fix risk. The abstraction
  must preserve clear ownership and should be tested at the shared boundary.
- Do not demand extraction for incidental syntax, tiny glue, explicit test
  examples whose duplication aids readability, or concepts expected to evolve
  independently. Require concrete rationale when meaningful duplication is
  intentionally retained.
- Use `Blocking` when newly introduced or expanded duplication can silently
  diverge in authorization, persistence, events, protocols, deployment, or
  public contracts. Use `Important` for duplicated business or test-framework
  logic that creates credible multi-site maintenance risk. Use `Advisory` for
  smaller safe extractions.

## vpsAdmin and Data-Layer Conventions

- For new or changed vpsAdmin API relationships to live resources, prefer
  HaveAPI `resource` parameters and attributes over raw integer IDs. Allow raw
  IDs for historical or deleted references that a live association cannot
  safely represent, with a concrete rationale.
- Require human-friendly labels and useful descriptions for changed vpsAdmin
  API parameters. Omit a description only when it adds no useful information;
  identifier-derived labels such as `Expires_at` are not human-friendly.
- Require each top-level vpsAdmin API resource to have its own source file;
  nested resources may remain with their parent.
- Keep plugin-owned vpsAdmin API resources, event/type registrations, mail
  templates, sysconfig keys, metrics, routes, and transaction behavior in the
  owning plugin unless a generic core extension point is intentionally changed.
- For changed ActiveRecord migrations, prefer `change` with `reversible` blocks
  for direction-dependent data operations. Allow `up`/`down` when required or
  materially clearer, with justification.
