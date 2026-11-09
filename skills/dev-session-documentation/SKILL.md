---
name: dev-session-documentation
description: Maintain useful project documentation and decision rationale during development sessions, including design constraints, deployment, verification, and recovery. Use when planning or implementing substantive changes, preparing reviews or handoffs, or investigating knowledge worth preserving.
---

# Development documentation

Treat documentation as part of the work you own. Choose what future developers
or operators need to understand, and maintain it while the task context is
available. Follow the user's scope, the active collaboration mode, and workspace
and repository instructions. Routine authoring choices do not need a separate
permission step; writing an operational procedure does not authorize executing it.

## Find the relevant context

Read the project's documentation entry point and the pages relevant to the
subsystem before choosing an approach. Inspect nearby code, tests, and recorded
decisions when the explanation and implementation disagree. Record material
uncertainty instead of silently choosing one as authoritative.

In the session plan, identify likely readers and documentation destinations.
Revise them as the work reveals what needs explaining. Use existing layouts and
link new material from the project's README or documentation index. Add a short
pointer in repository instructions when agents would otherwise miss that entry
point.

Save useful session files beneath `work/<slug>/` and link them from the session
records. They appear automatically in Files, including Git-ignored and
unregistered regular files. After archival, Files uses `archive/<slug>/` with the same session-relative URLs
for retained Markdown files and portal.yml. Raw logs, generated clients and other
supporting artifacts are disposable and are removed during archival. File registration is
unnecessary; `dev-session artifact add` only prints a notice and changes no files.

For a sensitive session, keep its plan, state, notes, review evidence and handoff
artifacts under its excluded tracking directory. Maintain them without tracking
commits. Do not promote session content into tracked notes, documentation or other
sessions. Project-code commits keep their normal workflow; any publication of
sensitive session content needs explicit user direction.

## Choose the smallest useful explanation

Use judgment about the change rather than creating a fixed set of files:

- Explain non-obvious invariants and choices that a future refactor could undo.
  A nearby comment or paragraph can be enough for a small fix.
- Describe a new subsystem's responsibilities, interactions, and assumptions
  in the owning project's documentation.
- For a consequential choice, record its context, the chosen approach, actual
  alternatives considered, consequences, and conditions for reconsideration.
  Use a separate decision record only when that makes it easier to find or
  maintain. Mark proposals and link superseded decisions to their successors.
- Keep supported version combinations and lasting compatibility constraints
  with the owning component. Put ordered upgrade and software rollback steps
  in the applicable upgrade or operations guide.
- Prepare deployment and recovery instructions when an operation needs ordered
  steps, downtime, flags, repair, or manual verification. Include prerequisites,
  expected results, and how to recognize and handle failure.

Capture rationale when the decision happens. Code alone cannot establish why a
past author chose a design. Cite available evidence, label an inferred
explanation, and leave unknown reasons unresolved. Do not invent alternatives
or present untested instructions as verified.

## Keep knowledge with its owner

Choose a home by the material's applicability, useful lifetime and owner.
Feature documentation describes the system at the documented revision; it
should remain useful after the initiative's deployment is complete.

| Material | Home |
| --- | --- |
| Current intent, constraints, open choices | Session plan |
| Current status, next actions, verification evidence | Session state and linked artifacts |
| Supported behavior, design rationale, invariants, failure semantics | Owning project's documentation |
| Repeatable deployment, diagnosis, operations and recovery | Separate project operations documentation; site-specific procedures in the site configuration repository |
| Instructions for a supported upgrade path | Project upgrade guide scoped to source and target versions or schema boundaries |
| An individual rollout's plan, revisions, actions, results and rollback preparation | Session rollout record; a dated deployment-repository record only when local policy permits it |
| Temporary branch state, review fixtures, disposable database resets | Session records |
| Reusable development-environment lessons | Workspace notes, following local conventions |

An individual rollout checklist belongs with that rollout even if it contains
no dates or revision hashes. Maintain reusable procedures as supported operations
change. Scope upgrade guidance to actual source/target releases or state
boundaries, without inventing version numbers. Retain a supported path until
the owning maintainer explicitly retires it with evidence covering old persisted
state, backup restoration and rollback. A completed rollout alone does not end
support. When retirement evidence is unknown, retain the instructions and mark
their applicability unresolved; do not hide them in private session records.
Completed deployment records describe what happened, without presenting their
steps as the current procedure.

Keep lasting constraints discoverable with the feature and link to them from
procedures. For example, transaction rollback guarantees describe application
behavior; steps to revert deployed software belong in operations or upgrade
guidance. Split mixed passages by meaning so moving a deployment section does
not bury an API contract or discard a recovery requirement.

Give a cross-project contract one authoritative home and link its consumers to
it. Keep project explanations understandable without access to a private
coordination workspace. Respect repository boundaries for internal site details.

Describe supported behavior in current documentation. Preserve historical
decisions with their status and applicable versions. Distinguish a prepared
rollout from one that has been executed and verified.

Keep the current state summary easy to read. Summarize superseded checkpoints
and link detailed commands, failures, and review evidence. Preserve the local
tracking commit cadence and lifecycle rules. Improve older project docs when
related work touches them; do not launch a historical backfill unless requested.

## Directory and README convention

Use this convention for new material. Existing subsystem documentation and
renderer layouts are equivalent homes when their purpose is clear; keep them
and link from their entry points. Create directories only when they have content.
This does not require a file per category, per-page metadata, a fixed document
bundle or a deployment heading on every feature page.

```text
project/
  README.md                         purpose, basic start, grouped docs links
  docs/
    index.md or README.md            use the existing renderer's index
    reference/<topic>.md             behavior, constraints, rationale
    how-to/<task>.md                 user/developer tasks with the product
    operations/
      <service-or-task>.md           maintained operator/maintainer runbooks
      upgrades/
        <from>-to-<to>.md            supported transitions and rollback

coordination-workspace/
  work/<slug>/{plan,state,design}.md  intent, current evidence, proposals
  work/<slug>/rollout.md             one rollout's steps, results and recovery
  archive/<slug>/...                 records after authorized archival
  notes/<project>/<dated-topic>.md   reusable workspace development lessons

deployment-repository/              only when local policy permits records
  docs/deployments/YYYY-MM-DD-<topic>.md  one historical execution
```

The root README is the entry point: purpose, intended readers, major components,
basic requirements and start commands, and grouped documentation links. Keep a
brief prerequisite or warning beside a basic start command when it is needed
to use that command safely. Put specialist test profiles, CI implementation
details, operational sequences, migration plans and execution evidence in their
topic homes. Short descriptive links to those pages belong in the README.
There is no arbitrary line limit.

Reference explains what the supported system does and why. How-to explains a
user or developer task with the product. Operations explains how maintainers
administer a deployed service or development infrastructure, including
prerequisites, expected results and failure handling. A runbook may include
concise configuration facts needed to operate safely; link to fuller reference
instead of duplicating it. Use stable topic/task filenames for maintained docs
and dates for individual execution records. Classify mixed passages by meaning,
not by their heading.

## Reconcile before review and handoff

Check explanations against the final implementation, relevant tests, and actual
deployment results. Verify links and example commands as far as the authorized
environment permits, and state remaining uncertainty. Apply the project's
writing conventions after settling the technical content.

When moving content, preserve consequential prerequisites, failure and partial
success conditions, compatibility restrictions, data-loss warnings and recovery
steps. Preserve old anchors or leave short forwarding links where callers may
depend on them. Check the factual diff as well as navigation; fluent wording
must not weaken a technical contract.

Include documentation changes with the implementation under the normal commit
and review workflow. Identify the relevant paths in review and handoff records.
If no documentation change is useful, briefly explain why. Check whether a
reader unfamiliar with the session can find the purpose, consequential reasons,
constraints, and applicable deployment or recovery instructions in their proper
homes. Check placement as well as completeness: feature docs should not depend
on knowing the branch's rollout status. Finish this work during the active task,
before its records become historical artifacts.
