import * as fs from 'node:fs';
import { dirname } from 'node:path';

// This is a storage barrier for an already verified native input prefix. It
// never creates, truncates, parses or writes native history contents.
export function syncHistoryPrefix(file, expected, io = fs) {
  const opened = [];
  let failure;
  try {
    const history = io.openSync(file, 'r'); opened.push(history);
    const actual = io.fstatSync(history);
    if (!actual.isFile() || actual.dev !== expected.dev || actual.ino !== expected.ino || actual.uid !== process.getuid()) {
      throw new Error('native history identity changed');
    }
    io.fsyncSync(history);
    const directory = io.openSync(dirname(file), 'r'); opened.push(directory);
    const parent = io.fstatSync(directory);
    if (!parent.isDirectory() || parent.uid !== process.getuid()) throw new Error('native history directory unavailable');
    io.fsyncSync(directory);
  } catch (error) { failure = error; }
  finally {
    for (const descriptor of opened.reverse()) {
      try { io.closeSync(descriptor); } catch (error) { failure ??= error; }
    }
  }
  if (failure) throw failure;
}
