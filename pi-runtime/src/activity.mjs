// Only observed public lifecycle intervals contribute elapsed time. Retained
// entry timestamps supply counts and latest bounds, never inferred idle/work.
export class WorkerActivity {
  constructor(manager, streaming, clock = {now: () => Date.now(), monotonic: () => performance.now()}) {
    this.manager = manager; this.clock = clock;
    this.coverageStart = clock.now(); this.since = this.coverageStart;
    this.tick = clock.monotonic(); this.state = streaming ? 'unclassified' : 'idle';
    this.totals = {working: 0, waiting: 0, idle: 0, unclassified: 0};
    this.sent = 0; this.received = 0; this.tools = 0; this.messages = 0; this.turnTools = 0;
    this.length = 0; this.history();
  }
  history() {
    const branch = this.manager.getBranch();
    for (const entry of branch.slice(this.length)) {
      const timestamp = typeof entry.timestamp === 'string' ? Date.parse(entry.timestamp) : NaN;
      const time = Number.isSafeInteger(timestamp) && timestamp > 0 ? timestamp : undefined;
      if (time && !this.created) this.created = time;
      if (entry.type !== 'message') continue;
      const message = entry.message;
      if (message.role === 'user') {
        this.sent++; this.messages = 1; this.turnTools = 0;
        this.turn = entry.id; this.started = time; this.completed = undefined; this.status = 'inProgress';
      } else {
        this.messages++;
        if (message.role === 'assistant') {
          this.received++;
          const count = Array.isArray(message.content) ? message.content.filter(block => block.type === 'toolCall').length : 0;
          this.tools += count; this.turnTools += count;
          if (['stop', 'error', 'aborted'].includes(message.stopReason)) {
            this.completed = time;
            this.status = message.stopReason === 'error' ? 'failed' : message.stopReason === 'aborted' ? 'interrupted' : 'completed';
          }
        }
      }
    }
    this.length = branch.length;
  }
  transition(state) {
    const tick = this.clock.monotonic();
    this.totals[this.state] += Math.max(0, tick - this.tick);
    this.tick = tick;
    if (this.state !== state) { this.state = state; this.since = this.clock.now(); }
  }
  event(event, held = false) {
    if (held) {
      this.transition('unclassified');
      if (event.type !== 'agent_settled') return;
    }
    if (event.type === 'agent_start') {
      if (!this.running) { this.observedStart = this.clock.now(); this.observedEnd = undefined; }
      this.running = true;
      this.transition('working');
    }
    if (event.type === 'auto_retry_start' && this.running && Number.isSafeInteger(event.delayMs) && event.delayMs >= 0) {
      this.waitUntil = this.clock.now() + event.delayMs; this.transition('waiting');
    }
    if (event.type === 'auto_retry_end' && this.running) this.transition('working');
    // agent_end precedes retry/queued continuation; it does not end work.
    if (event.type === 'agent_settled') {
      if (this.observedStart) this.observedEnd = this.clock.now();
      this.running = false;
      this.transition(held ? 'unclassified' : 'idle');
    }
  }
  read(held) {
    if (held) this.transition('unclassified');
    const totals = {...this.totals};
    totals[this.state] += Math.max(0, this.clock.monotonic() - this.tick);
    return {threadId: this.manager.getSessionId(), currentTurnId: this.turn,
      messages: this.messages, toolCalls: this.turnTools, sentMessages: this.sent,
      receivedMessages: this.received, totalToolCalls: this.tools, countsComplete: true,
      createdAtMs: this.created, startedAtMs: this.observedStart ?? this.started, completedAtMs: this.observedStart ? this.observedEnd : this.completed,
      latestTurnStatus: this.running ? 'inProgress' : this.status, workingMs: Math.floor(totals.working), waitingMs: Math.floor(totals.waiting),
      waitReason: this.state === 'waiting' ? 'retry' : undefined, waitUntilMs: this.state === 'waiting' ? this.waitUntil : undefined,
      openWaitingMs: this.state === 'waiting' ? Math.floor(Math.max(0, this.clock.monotonic() - this.tick)) : 0,
      idleMs: Math.floor(totals.idle), betweenTurnsMs: Math.floor(totals.idle),
      unclassifiedMs: Math.floor(totals.unclassified), observedAtMs: this.clock.now(),
      coverageStartMs: this.coverageStart, coverageComplete: false,
      coverageReason: 'Observed since worker attachment; earlier timing is unavailable.',
      currentState: this.state, stateSinceMs: this.since, scope: 'root-thread', timingApproximate: false};
  }
}
