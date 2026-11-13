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
    async execute(_toolCallID, args, signal) {
      const payload = checked(name, args);
      if (typeof callHost !== 'function') throw Error('Team delivery is unavailable');
      if (signal?.aborted) throw signal.reason ?? Error('Team delivery canceled');
      const result = await callHost(name, payload, {signal});
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
