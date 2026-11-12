import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

// Consume the public exports from one coherent, pinned upstream Nix package.
// No npm resolution, source patches or credentials from the caller's home.
export const packageRoot = process.env.PI_PROBE_PACKAGE;
if (!packageRoot?.startsWith('/nix/store/')) {
  throw new Error('PI_PROBE_PACKAGE must name the pinned upstream Nix output');
}
export const nodeBinary = readFileSync(join(packageRoot, 'bin/pi'), 'utf8')
  .match(/\/nix\/store\/[a-z0-9]{32}-nodejs-[^/\s"']+\/bin\/node/)?.[0];
if (!nodeBinary) throw new Error('Pinned Pi wrapper does not identify its Node runtime');
export async function publicPackage(name) {
  const dir = join(packageRoot, 'lib/pi/node_modules', name);
  const manifest = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
  if (manifest.version !== '1.5.2') throw new Error(`Unexpected ${name} version`);
  const entry = manifest.exports?.['.']?.import ?? manifest.main;
  if (typeof entry !== 'string') throw new Error(`No public import for ${name}`);
  return import(pathToFileURL(join(dir, entry)).href);
}
export const sdk = await publicPackage('@earendil-works/pi-coding-agent');
export const ai = await publicPackage('@earendil-works/pi-ai');
