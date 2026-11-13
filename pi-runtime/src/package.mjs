import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

export async function loadSDK(packageRoot) {
  if (!/^\/nix\/store\/[a-z0-9]{32}-[^/]+$/.test(packageRoot)) throw new Error('invalid immutable Pi package');
  const root = join(packageRoot, 'lib/pi/node_modules/@earendil-works/pi-coding-agent');
  const manifest = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
  return import(pathToFileURL(join(root, manifest.exports?.['.']?.import ?? manifest.main)).href);
}
