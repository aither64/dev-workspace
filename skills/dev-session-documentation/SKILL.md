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
- Explain supported version combinations, migration order, and rollback limits
  when changing persistent data or component contracts.
- Prepare deployment and recovery instructions when an operation needs ordered
  steps, downtime, flags, repair, or manual verification. Include prerequisites,
  expected results, and how to recognize and handle failure.

Capture rationale when the decision happens. Code alone cannot establish why a
past author chose a design. Cite available evidence, label an inferred
explanation, and leave unknown reasons unresolved. Do not invent alternatives
or present untested instructions as verified.

## Keep knowledge with its owner

| Material | Home |
| --- | --- |
| Current intent, constraints, open choices | Session plan |
| Current status, next actions, verification evidence | Session state and linked artifacts |
| Supported behavior, design rationale, accepted decisions | Owning project's documentation |
| Repeatable operations and recovery | Project operations docs; site-specific procedures in the site configuration repository |
| Exact rollout revisions, actions, and results | Session rollout record or explicitly versioned release runbook |
| Reusable development-environment lessons | Workspace notes, following local conventions |

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

## Reconcile before review and handoff

Check explanations against the final implementation, relevant tests, and actual
deployment results. Verify links and example commands as far as the authorized
environment permits, and state remaining uncertainty. Apply the project's
writing conventions after settling the technical content.

Include documentation changes with the implementation under the normal commit
and review workflow. Identify the relevant paths in review and handoff records.
If no documentation change is useful, briefly explain why. Check whether a
reader unfamiliar with the session can find the purpose, consequential reasons,
constraints, and applicable deployment or recovery instructions. Finish this
work during the active task, before its records become historical artifacts.
