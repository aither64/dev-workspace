import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { syncHistoryPrefix } from './persistence.mjs';

test('native-prefix barrier orders read-only file and directory sync without changing bytes', () => {
  const root = fs.mkdtempSync(join(tmpdir(), 'pi-native-sync-'));
  const file = join(root, 'owned-fixture');
  fs.writeFileSync(file, 'owned bytes\n', { mode: 0o600 });
  const bytes = fs.readFileSync(file);
  try {
    const events = []; const paths = new Map();
    syncHistoryPrefix(file, fs.statSync(file), { ...fs,
      openSync: (path, flags) => { assert.equal(flags, 'r'); const fd = fs.openSync(path, flags); paths.set(fd, path); events.push(['open', path]); return fd; },
      fsyncSync: (fd) => { events.push(['sync', paths.get(fd)]); fs.fsyncSync(fd); },
      closeSync: (fd) => { events.push(['close', paths.get(fd)]); fs.closeSync(fd); },
    });
    assert.deepEqual(events, [['open', file], ['sync', file], ['open', root], ['sync', root], ['close', root], ['close', file]]);
    assert.deepEqual(fs.readFileSync(file), bytes);
    for (const point of ['file-open', 'file-sync', 'directory-open', 'directory-sync', 'directory-close', 'file-close']) {
      const descriptors = new Map(); let openCount = 0;
      const io = { ...fs,
        openSync: (path, flags) => {
          openCount++;
          if (point === (openCount === 1 ? 'file-open' : 'directory-open')) throw new Error(point);
          const fd = fs.openSync(path, flags); descriptors.set(fd, path); return fd;
        },
        fsyncSync: (fd) => { if (point === (descriptors.get(fd) === file ? 'file-sync' : 'directory-sync')) throw new Error(point); fs.fsyncSync(fd); },
        closeSync: (fd) => { fs.closeSync(fd); const path = descriptors.get(fd); descriptors.delete(fd); if (point === (path === file ? 'file-close' : 'directory-close')) throw new Error(point); },
      };
      assert.throws(() => syncHistoryPrefix(file, fs.statSync(file), io), new RegExp(point));
      assert.equal(descriptors.size, 0); assert.deepEqual(fs.readFileSync(file), bytes);
    }
    assert.throws(() => syncHistoryPrefix(file, { ...fs.statSync(file), ino: -1 }));
    assert.throws(() => syncHistoryPrefix(join(root, 'missing'), fs.statSync(file)));
    assert.equal(fs.existsSync(join(root, 'missing')), false);
  } finally { fs.rmSync(root, { recursive: true }); }
});
