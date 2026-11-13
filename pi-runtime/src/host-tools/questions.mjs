// A caller-bound factory. The host callback owns identity/proof and durable ACK.
// No SDK dependency, network endpoint, worker activation or UI dialog is hidden here.
export function createQuestionTools({host, boundCaller}) {
  if (!host || !boundCaller) throw Error('Question host binding is required');
  return [{name: 'request_user_input_async', label: 'Ask optional questions',
    description: 'Publish optional questions and continue working. Answers arrive later as user messages.',
    parameters: {type: 'object', additionalProperties: false, required: ['questions'], properties: {
      questions: {type: 'array', minItems: 1, maxItems: 3, items: {type: 'object', additionalProperties: false,
        required: ['title'], properties: {title: {type: 'string', minLength: 1}, options: {type: 'array', maxItems: 32, items: {type: 'string'}}}}}}},
    async execute(nativeToolCallId, params, signal) {
      signal?.throwIfAborted();
      if (typeof host.publishQuestion !== 'function' || typeof nativeToolCallId !== 'string' || !nativeToolCallId) throw Error('Question publication is unavailable');
      const result = await host.publishQuestion(nativeToolCallId, params, signal);
      if (!result || !/^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(result.id) || result.revision !== 1 || result.status !== 'pending') throw Error('Question publication was not acknowledged');
      const pending = {id: result.id, revision: result.revision, status: 'pending'};
      return {content: [{type: 'text', text: JSON.stringify(pending)}], details: pending};
    },
  }];
}
