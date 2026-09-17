---
name: dev-session-monitor
description: Delegate long test, CI, and build monitoring to a fresh Luna subagent, then continue in the parent conversation. Use for authorized verification expected to exceed one minute and integration tests or builds of uncertain duration; keep known quick checks inline.
---

# Development monitoring

Use native delegation to reduce routine monitoring usage. Keep the parent model
and reasoning effort unchanged: the parent selects commands, interprets results,
diagnoses failures, makes changes, and decides whether to rerun or accept checks.
This skill authorizes a bounded monitoring subagent, not additional development
or deployment work. If you are already the assigned watcher, follow only the
watcher instructions below; do not delegate again.

## Parent handoff

Delegate before launching an eligible command. Use `gpt-5.6-luna` with `low`
reasoning and fresh context (`fork_turns: "none"`), explicitly selecting both
settings rather than inheriting the parent or changing global agent defaults.
Keep known quick checks inline. Do not start or wait for CI the user has excluded.

Give the watcher only the information it needs:

- Its monitoring role, this skill path, and the exact authorized command or CI
  run IDs. Include the canonical working directory, required environment and
  tested revision; do not pass the full development conversation or secrets.
- Where to keep complete logs and which status source proves completion. For
  new local runs the watcher owns execution and its tool handle. Existing runs
  need independently accessible logs and status; a parent's tool handle is not
  assumed transferable. Do not relaunch an existing run to gain ownership.
- Any caller or project deadline, cancellation rule, and escalation condition.
  State whether cancellation is authorized and identify the exact owned run.
  Do not invent a generic timeout for quiet builds or tests.
- The result format below and the requirement to return to the parent without
  edits, diagnosis, retries, deployment, acceptance decisions or nested agents.

Use one watcher for a related batch of checks when practical. Do independent
useful work while it runs, then use native waiting within the available tool
limits. Do not poll the same logs alongside the watcher or ask repeatedly for
unchanged status. Keep necessary parent wake-ups brief. Continue automatically
on the watcher's result; no user confirmation is needed for that handoff.

If the skill's delegation tools, Luna/low, or a free agent slot are unavailable,
say so once and monitor in the parent with blocking waits and bounded output.
Use the same fallback when an existing run has no transferable observation
interface. Do not silently substitute a different watcher model, interrupt
unrelated agents, or stall verification waiting for capacity.

## Watcher instructions

Run only the assigned operation, or observe the specified existing run. Preserve
the exact command's exit status when capturing output. Use blocking waits where
possible and bounded status reads otherwise; do not repeatedly dump full logs.
For CI, track the specified revision and run IDs rather than a moving branch's
latest run. Collect available failure evidence without rerunning failed jobs.

Report completion or an escalation condition promptly. Follow the supplied
cancellation rules; silence alone is not a reason to kill a process. If a run
still needs attention, notify the parent with its identity and evidence while
retaining ownership until the parent responds. Do not leave a running operation
unaccounted for or claim a timeout proves test failure.

Do not edit project files, write initiative tracking, investigate root causes,
change commands, install dependencies beyond the assigned command, retry, deploy,
approve results, or spawn agents. Local logs and result artifacts requested in
the brief are allowed. Follow any applicable project escalation requirements.

Return a compact report:

- Result: passed, failed, or incomplete; exit status or CI conclusion.
- Tested revision, command/run identity, and elapsed time.
- Log/artifact paths and a bounded excerpt identifying failures or escalation.
- Any still-running operation and any explicitly authorized cancellation taken.

Return raw evidence and uncertainty, not a diagnosis. The parent resumes the
development workflow and owns all resulting decisions. Retain evidence; deleting
an agent or its history does not reduce usage already incurred.
