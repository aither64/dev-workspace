# Controlled portal and coordinator CPU comparison

`TestCPUFixture` is a one-off, opt-in experiment for scheduler and status-read
changes. Ordinary tests and CI skip it. Never add this long benchmark to routine
tests or CI; deterministic work-count and correctness tests remain the regression
gates. Timings provide one-off diagnosis and acceptance evidence.

The fixture creates its own temporary workspace, authority records, recovery and
retry stores, retained teams, and valid coordination ledger. The default scale is
20 owners/roots and five members per root, with roughly 3 MiB of synthetic retained
receipts. It never reads another session or writes registered workspace state.
The test removes only its own temporary files after its children exit.

The real portal retry/recovery workers and coordination status handler run with
a counting synthetic native Client/Backend. Ruby invokes the real coordinator
workspace pass; the baseline adapter reproduces its old per-workspace traversal.
Both retain the five-second fallback and wake socket. Collection is omitted only
in this disposable fixture. Native notification replies, tmux and owner admission
use fixture adapters. Status still starts a real Ruby subprocess and validates
the ledger; notification verification/preparation/delivery still cross a real
subprocess boundary. No systemd worker, Nix store operation or native recipient is
launched by those adapters.

The native Codex daemon, model service, activity observer's native connection,
browser rendering and unrelated host services are excluded. `ObserveActivity` is
off on both revisions. This measures the changed hot paths and helper children;
it does not reproduce the complete deployed service or model workload. Corroborate
the result with separately reported live service-cgroup samples after rollout,
including differences in active workload and open browsers.

## Prepare and validate

Use the repository's Nix environment. This quick mode validates synthetic input,
one full retry admission, status, and a coordinator pass without a timed run:

```sh
nix develop --command env DEV_WORKSPACE_CPU_BENCH=validate \
  CPU_BENCH_ROOTS=1 CPU_BENCH_MEMBERS=1 \
  go -C portal test -mod=readonly ./internal/web -run '^TestCPUFixture$' -count=1 -v
```

Validate the retained-state shape with `CPU_BENCH_ROOTS=20` and
`CPU_BENCH_MEMBERS=5`. The ledger target defaults to 3145728 bytes; set
`CPU_BENCH_LEDGER_BYTES=32768` for a small-state control. Output records actual
ledger bytes and owner count. Scale is bounded to 100 roots and ten members/root.

Compare exact committed source exports of the baseline and candidate. Copy only
`portal/internal/web/cpu_benchmark_test.go` and `test/support/cpu_fixture.rb` from
the reviewed candidate into the baseline export. Record their hashes and the
baseline/candidate Git heads and flake locks. Keep source exports and binaries
outside the repository. Compile each source's test binary in its own Nix shell:

```sh
nix develop --command go -C portal test -mod=readonly -c \
  -o /tmp/cpu-bench-candidate.test ./internal/web
```

Builds and timed runs follow independent final review and use the verification
watcher. Do not compare a race-enabled binary with an ordinary binary.

## Run the comparison

Run each binary in its own transient user service, with its package directory as
working directory. The fixture refuses measurement outside a cgroup whose unit
name begins `workspace-cpu-bench-`. An example, launched from the source Nix shell:

```sh
systemd-run --user --wait --pipe --collect \
  --unit=workspace-cpu-bench-candidate-idle0-1 \
  --property=CPUAccounting=yes --working-directory="$PWD/portal/internal/web" \
  --setenv=PATH="$PATH" --setenv=DEV_WORKSPACE_CPU_BENCH=measure \
  --setenv=CPU_BENCH_CONSUMERS=0 --setenv=CPU_BENCH_ACTIVE=0 \
  /tmp/cpu-bench-candidate.test -test.run '^TestCPUFixture$' -test.v -test.timeout=6m
```

Warm-up waits for passive observation of every candidate and a persisted,
completed controller transaction for every retry state. It takes at least ten
seconds in measurement mode. The real two-worker queue cannot visit all 120
conversations in ten seconds; a fixed delay alone would compare cold admissions
with settled work. The sweep bound is candidate count times the one-second
scheduler cadence, plus its fifteen-second worker bound. Quick validation uses
the same coverage proof without the ten-second minimum.

Each run then measures for thirty seconds, finishing at a quiescent Ruby pass
boundary. Consumers request the same global status scope concurrently every
five seconds. Run four cells: idle with 0, 1 and 5 consumers, then active with 5.
Active publishes a completed-turn change and due notification on a polling tick
only after the prior native version was observed. At the end it drains that one
outstanding update within the sweep bound, still counting CPU and polls. This
prevents synthetic versions from overwriting an update before a fair scheduler
sweep can see it. Report actual intervals and event counts; do not require six
observations per run.

Repeat each cell three times, alternating baseline/candidate order between pairs.
At the representative scale, expect 30–60 seconds of warm-up and about 24–36
minutes for the matrix, plus active drains and setup. Reassess at 45 minutes;
escalate the first fixture failure without automatic retries or extra cells.

`CPU_BENCH_RESULT` records whole-cgroup CPU seconds (including every descendant),
portal self CPU, coordinator self plus reaped synchronous-child CPU, Go/Ruby
allocations, exact helper counts, passive native observations, verification calls,
warm-up duration/complete conversation count, actual active update count, and raw
request/observation/delivery delays. Ruby samples Process.times only
between passes; its interval can differ slightly from the parent cgroup interval.
Normalize each CPU value by its own reported interval. The fixture itself and its
small measurement overhead are included identically in both revisions.

Report all three samples, median, range and baseline-relative reduction for each
cell. Compute request and reaction p50/p95 from the raw arrays, identifying the
small sample count. Check that active updates were observed and notifications
delivered; missing samples are failures, not zero latency. Inspect repeatable
active regressions above 20%. The experimental targets are 80% coordinator and
50% combined idle CPU reduction. If variance prevents a useful conclusion, stop
and diagnose the fixture or competing load before expanding the campaign.

Keep exact commands, source/input identities, results and uncertainty in the
owning session's verification record. These measurements grant no deployment or
integration permission. Coordination ownership remains governed by the
[coordination contract](coordination.md).
