import {createHash} from 'node:crypto';
import {isDeepStrictEqual} from 'node:util';
import {installLinearGuards} from './lineage.mjs';

const namePattern = /^[A-Za-z_][A-Za-z0-9_.-]{0,127}$/;
const planNames = new Set(['read', 'grep', 'find', 'ls', 'request_user_input_async', 'publish_plan']);
const namesValid = names => Array.isArray(names) && names.length <= 128 && names.every((name, i) =>
  typeof name === 'string' && namePattern.test(name) && (i === 0 || names[i - 1] < name));
export const toolPolicyDigest = target => createHash('sha256').update(JSON.stringify([
  'pi-tool-policy', 1, target.Mode, target.Active, target.RegisteredCeiling])).digest('hex');
export function validateToolPolicy(target) {
  if (!target || Object.keys(target).sort().join(',') !== 'Active,Digest,Mode,RegisteredCeiling,Revision' ||
      !['plan', 'default'].includes(target.Mode) || !Number.isSafeInteger(target.Revision) || target.Revision < 1 ||
      !namesValid(target.Active) || !namesValid(target.RegisteredCeiling) || target.Digest !== toolPolicyDigest(target) ||
      target.Active.some(name => !target.RegisteredCeiling.includes(name) || target.Mode === 'plan' && !planNames.has(name))) {
    throw Error('Invalid tool policy');
  }
  return structuredClone(target);
}
const instructions = mode => mode === 'plan'
  ? 'Plan mode: inspect using the allowed read tools. Publish a plan or optional question when useful. Do not mutate the workspace or delegate work. Optional questions do not block independent work.'
  : 'Default mode: use only the tools permitted by the retained host access policy. Optional questions do not grant approval for dependent actions.';

// Construct before resource loading. Exactly this trusted inline extension must
// be loaded; no ambient extensions, MCP or deferred/router tools may be present.
export function createToolPolicy(initialTarget, {changed = () => {}} = {}) {
  let policy = validateToolPolicy(initialTarget), installed = false, applied = false, held = false;
  let running = false, frozenRun = null, instructionsInstalled = false, boundSession = null;
  const activeCalls = new Set();
  const observation = session => {
    const active = [...session.getActiveToolNames()].sort(), callable = [...session.getCallableToolNames()].sort();
    const definitions = session.getAllTools();
    const registered = definitions.map(tool => tool.name).sort();
    const safe = namesValid(active) && namesValid(callable) && namesValid(registered) &&
      definitions.every(tool => (tool.exposure ?? 'direct') === 'direct' && tool.reachesTools === undefined) &&
      registered.every(name => policy.RegisteredCeiling.includes(name)) &&
      isDeepStrictEqual(active, policy.Active) && isDeepStrictEqual(callable, policy.Active);
    return {active, callable, registered, safe};
  };
  const extensionFactory = api => {
    if (installed) throw Error('Tool policy extension is already installed');
    installed = true;
    api.on('before_agent_start', (event, ctx) => {
      const check = boundSession && observation(boundSession);
      if (!applied || held || !check?.safe) { frozenRun = null; ctx.abort(); throw Error('Tool policy is not applied'); }
      running = true; frozenRun = new Set(policy.Active); changed();
      instructionsInstalled = true;
      return {systemPrompt: `${event.systemPrompt}\n\n${instructions(policy.Mode)}`};
    });
    api.on('tool_call', (event, ctx) => {
      const allowed = frozenRun ?? new Set();
      if (held || !applied || !running || !allowed.has(event.toolName) || !observation(boundSession).safe) {
        return {block: true, reason: 'Tool is outside the applied host policy'};
      }
    });
    api.on('tool_execution_start', event => { activeCalls.add(event.toolCallId); changed(); });
    api.on('tool_execution_end', event => { activeCalls.delete(event.toolCallId); changed(); });
    api.on('agent_settled', () => { running = false; frozenRun = null; changed(); });
    installLinearGuards(api);
  };
  const inspect = (session, generation) => {
    const seen = observation(session);
    return {Policy: structuredClone(policy), Generation: generation,
      Active: seen.active, Callable: seen.callable, Registered: seen.registered,
      Applied: session === boundSession && applied && !held && installed && instructionsInstalled && seen.safe};
  };
  const apply = (session, target, generation) => {
    let next;
    try { next = validateToolPolicy(target); } catch { return {Outcome: 'rejected', Proof: inspect(session, generation)}; }
    if (!installed || (boundSession && boundSession !== session) || held || running || activeCalls.size || session.isStreaming ||
        !isDeepStrictEqual(next.RegisteredCeiling, policy.RegisteredCeiling) ||
        !(isDeepStrictEqual(next, policy) || next.Revision === policy.Revision + 1)) {
      return {Outcome: 'rejected', Proof: inspect(session, generation)};
    }
    if (isDeepStrictEqual(next, policy) && applied) {
      const proof = inspect(session, generation); if (!proof.Applied) held = true;
      return {Outcome: proof.Applied ? 'applied' : 'unknown', Proof: proof};
    }
    try {
      boundSession = session; policy = next; applied = false;
      session.setActiveToolsByName([...policy.Active]);
      const seen = observation(session);
      if (!seen.safe) throw Error('Tool policy loadout differs');
      // Retain the exact host instruction hook before allowing the first run.
      instructionsInstalled = true; applied = true;
      return {Outcome: 'applied', Proof: inspect(session, generation)};
    } catch { held = true; applied = false; return {Outcome: 'unknown', Proof: inspect(session, generation)}; }
  };
  const workflow = (session, generation) => ({proof: inspect(session, generation),
    running, activeCalls: activeCalls.size, held});
  return {extensionFactory, inspect, apply, workflow};
}
