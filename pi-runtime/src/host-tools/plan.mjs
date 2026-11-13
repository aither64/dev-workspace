// P3 may register this only with completed-origin and enforced-policy proof.
export function createPlanTools({host, boundCaller}) {
  if (!host || !boundCaller) throw Error('Plan host binding is required');
  return [{name: 'publish_plan', label: 'Publish plan', description: 'Publish a completed proposal without accepting or implementing it.',
    parameters: {type: 'object', additionalProperties: false, required: ['text'], properties: {text: {type: 'string', minLength: 1}}},
    async execute(nativeToolCallId, params, signal) {
      signal?.throwIfAborted();
      if (typeof host.publishPlan !== 'function' || typeof nativeToolCallId !== 'string' || !nativeToolCallId) throw Error('Plan publication is unavailable');
      const value = await host.publishPlan(nativeToolCallId, params, signal);
      if (!value || !/^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(value.planId) || !/^[0-9a-f]{64}$/.test(value.textSha256) || value.revision !== 1) throw Error('Plan publication was not acknowledged');
      return {content: [{type: 'text', text: JSON.stringify(value)}], details: value};
    },
  }];
}
