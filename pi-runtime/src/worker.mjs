import { runWorker } from './protocol.mjs';

// The only production entry point: trusted supervisor identity/config arrives
// over inherited pipes. No model, credential or worker identity comes from env.
await runWorker();
