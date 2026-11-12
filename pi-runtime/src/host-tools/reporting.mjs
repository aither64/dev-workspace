// Inert definitions only. callHost is injected by an authenticated P3 channel;
// no process/environment identity, transport, activation or final-answer hook.
const messageID = /^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/;
const address = /^(?:lead|[a-z][a-z0-9]{0,31}[0-9]+)$/;

function checked(name, args) {
  const required = name === 'report_to_lead' ? ['message_id', 'message'] : ['message_id', 'message', 'to'];
  if (!args || typeof args !== 'object' || Array.isArray(args) ||
      Object.keys(args).length !== required.length || required.some(key => !Object.hasOwn(args, key)) ||
      required.some(key => typeof args[key] !== 'string' || !args[key].isWellFormed() || args[key].includes('\0')) ||
      !messageID.test(args.message_id) || !args.message.trim() || Buffer.byteLength(args.message, 'utf8') > 20000 ||
      (name === 'team_message' && (!address.test(args.to) || args.to.length > 64))) {
    throw Error('Invalid team tool arguments');
  }
  return Object.fromEntries(required.map(key => [key, args[key]]));
}

export function createReportingTools(callHost) {
  return ['report_to_lead', 'team_message'].map(name => ({
    name,
    label: name === 'report_to_lead' ? 'Report to lead' : 'Message team member',
    description: name === 'report_to_lead' ? "Send an explicit result or blocking question to this session's lead." : 'Send a message to a permitted member of this session.',
    parameters: {type: 'object', additionalProperties: false,
      required: name === 'report_to_lead' ? ['message_id', 'message'] : ['message_id', 'message', 'to'],
      properties: {message_id: {type: 'string', pattern: messageID.source}, message: {type: 'string', minLength: 1},
        ...(name === 'team_message' ? {to: {type: 'string', pattern: address.source}} : {})}},
    async execute(toolCallID, args, signal) {
      const payload = checked(name, args);
      if (typeof toolCallID !== 'string' || !toolCallID || !toolCallID.isWellFormed() ||
          toolCallID.includes('\0') || Buffer.byteLength(toolCallID, 'utf8') > 4096) {
        throw Error('Invalid native team tool call ID');
      }
      if (typeof callHost !== 'function') throw Error('Team delivery is unavailable');
      if (signal?.aborted) throw signal.reason ?? Error('Team delivery canceled');
      const result = await callHost(name, payload, {signal, nativeToolCallID: toolCallID});
      if (!result || !['held', 'unavailable', 'attempted'].includes(result.disposition)) {
        throw Error('Team delivery outcome is unknown');
      }
      if ((result.disposition === 'attempted' && !['accepted', 'not_submitted', 'unknown'].includes(result.outcome)) ||
          (result.disposition !== 'attempted' && (result.native || result.outcome))) {
        throw Error('Team delivery outcome is unknown');
      }
      return {content: [{type: 'text', text: JSON.stringify(result)}], details: result};
    },
  }));
}

// Utility-only monitor definition. The original operation/input/utility caller
// is captured by callHost, never accepted from params. Launch/cancel/resolve and
// retained-member tools are outside this factory's scope.
export function createMonitorTools(callHost) {
  return [{name: 'monitor_operation', label: 'Monitor operation',
    description: 'Read this verification operation or publish its explicit result.',
    parameters: {type: 'object', additionalProperties: false, required: ['action'], properties: {
      action: {type: 'string', enum: ['status', 'report']},
      slot: {type: 'string', enum: ['escalation', 'terminal']},
      outcome: {type: 'string', enum: ['passed', 'failed', 'incomplete']},
      exitStatus: {type: 'integer'}, conclusion: {type: 'string'},
      elapsedMillis: {type: 'integer', minimum: 0},
      evidence: {type: 'array', maxItems: 16, items: {type: 'string'}},
      stillRunning: {type: 'string'}, detail: {type: 'string'},
    }},
    async execute(toolCallID, args, signal) {
      const allowed = ['action', 'slot', 'outcome', 'exitStatus', 'conclusion', 'elapsedMillis', 'evidence', 'stillRunning', 'detail'];
      const text = (value, max) => typeof value === 'string' && value.isWellFormed() && !value.includes('\0') && Buffer.byteLength(value, 'utf8') <= max;
      if (!args || typeof args !== 'object' || Array.isArray(args) || Object.keys(args).some(key => !allowed.includes(key)) ||
          !['status', 'report'].includes(args.action) ||
          !text(toolCallID, 4096) || !toolCallID) throw Error('Invalid monitor tool arguments');
      if (args.action === 'status') {
        if (Object.keys(args).length !== 1) throw Error('Invalid monitor status arguments');
      } else {
        if (!['escalation', 'terminal'].includes(args.slot) || !['passed', 'failed', 'incomplete'].includes(args.outcome) ||
            !Number.isSafeInteger(args.elapsedMillis) || args.elapsedMillis < 0 ||
            (Object.hasOwn(args, 'exitStatus') && !Number.isSafeInteger(args.exitStatus)) ||
            (Object.hasOwn(args, 'conclusion') && !text(args.conclusion, 256)) ||
            (Object.hasOwn(args, 'detail') && !text(args.detail, 4096)) ||
            (Object.hasOwn(args, 'stillRunning') && !text(args.stillRunning, 4096)) ||
            (Object.hasOwn(args, 'evidence') && (!Array.isArray(args.evidence) || args.evidence.length > 16 || args.evidence.some(value => !text(value, 1024)))) ||
            (args.slot === 'escalation' && (args.outcome !== 'incomplete' || !args.detail || !args.stillRunning)) ||
            (args.outcome === 'incomplete' && !args.detail) ||
            (args.outcome !== 'incomplete' && !Object.hasOwn(args, 'exitStatus') && !args.conclusion) ||
            (args.outcome === 'passed' && ((Object.hasOwn(args, 'exitStatus') && args.exitStatus !== 0) || (args.conclusion && args.conclusion !== 'success'))) ||
            (args.outcome === 'failed' && (args.exitStatus === 0 || args.conclusion === 'success'))) throw Error('Invalid monitor report arguments');
      }
      if (typeof callHost !== 'function') throw Error('Monitor operation is unavailable');
      if (signal?.aborted) throw signal.reason ?? Error('Monitor operation canceled');
      const result = await callHost('monitor_operation', {...args}, {signal, nativeToolCallID: toolCallID});
      if (args.action === 'report' ? (typeof result !== 'string' || !messageID.test(result)) :
          (!result || result.schema !== 2 || result.backend !== 'pi' || !messageID.test(result.id))) {
        throw Error('Monitor operation outcome is unknown');
      }
      return {content: [{type: 'text', text: JSON.stringify(result)}], details: result};
    },
  }];
}
