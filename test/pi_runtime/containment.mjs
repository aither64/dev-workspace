import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomUUID } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';

// Finite disposable unit, never a session supervisor or live workspace service.
const self = fileURLToPath(import.meta.url);
function observe() {
  return { pid: process.pid, uid: process.getuid(), gid: process.getgid(), groups: process.getgroups(),
    mountNamespace: statSync('/proc/self/ns/mnt').ino, userNamespace: statSync('/proc/self/ns/user').ino,
    uidMap: readFileSync('/proc/self/uid_map', 'utf8'), gidMap: readFileSync('/proc/self/gid_map', 'utf8'),
    cgroup: readFileSync('/proc/self/cgroup', 'utf8') };
}
if (process.argv[2] === '--worker') {
  console.log(JSON.stringify(observe()));
} else if (process.argv[2] === '--inside') {
  const own = observe();
  const children = [0, 1].map(() => {
    const child = spawnSync(process.execPath, [self, '--worker'], { encoding: 'utf8' });
    assert.equal(child.status, 0, child.stderr);
    return JSON.parse(child.stdout);
  });
  for (const child of children) {
    assert.equal(child.mountNamespace, own.mountNamespace);
    assert.equal(child.cgroup, own.cgroup);
    assert.equal(child.uid, own.uid);
  }
  const repository = process.argv[3];
  const tools = {};
  for (const [name, argv] of Object.entries({ id: ['id'], nix: ['nix', '--extra-experimental-features', 'nix-command', 'store', 'ping', '--store', 'daemon'],
    sudo: ['/run/wrappers/bin/sudo', '-n', 'true'] })) {
    const result = spawnSync(argv[0], argv.slice(1), { encoding: 'utf8', timeout: 5000 });
    tools[name] = { status: result.status, signal: result.signal, error: result.error?.code, stderr: result.stderr?.trim().slice(-500) };
  }
  const repoReadable = readFileSync(join(repository, 'AGENTS.md'), 'utf8').length > 0;
  writeFileSync('/tmp/p0-retained-marker', 'owned scratch\n');
  writeFileSync('/var/tmp/p0-retained-marker', 'owned scratch\n');
  // Keep both finite sessions alive together; sequential namespace inode values
  // may be reused and cannot establish simultaneous separation.
  await delay(1000);
  console.log(JSON.stringify({ own, children, tools, repoReadable, invocationId: process.env.INVOCATION_ID,
    nixSocketExists: existsSync('/nix/var/nix/daemon-socket/socket') }));
} else {
  if (!process.env.XDG_RUNTIME_DIR) throw new Error('XDG_RUNTIME_DIR required for owned records outside scratch');
  const root = mkdtempSync(join(process.env.XDG_RUNTIME_DIR, 'pi-p0-'));
  const repository = resolve(process.argv[2] ?? '.');
  const before = observe();
  const hostSudo = spawnSync('/run/wrappers/bin/sudo', ['-n', 'true'], { encoding: 'utf8', timeout: 5000 });
  before.sudo = { status: hostSudo.status, signal: hostSudo.signal, error: hostSudo.error?.code, stderr: hostSudo.stderr?.trim() };
  const runs = await Promise.all(['a', 'b'].map(async (session) => {
    const backing = join(root, session);
    mkdirSync(join(backing, 'tmp'), { recursive: true });
    mkdirSync(join(backing, 'var-tmp'));
    const unit = `pi-p0-${randomUUID()}.service`;
    const argv = ['--user', '--wait', '--pipe', '--collect', `--unit=${unit}`,
      '--property=Type=exec', '--property=Restart=no', '--property=KillMode=control-group',
      '--property=RuntimeMaxSec=20', '--property=TimeoutStopSec=5',
      '--property=PrivateUsers=yes', '--property=PrivateMounts=yes',
      `--property=BindPaths=${backing}/tmp:/tmp ${backing}/var-tmp:/var/tmp`,
      '--', process.execPath, self, '--inside', repository];
    const result = await new Promise((resolveResult) => {
      const child = spawn('systemd-run', argv, { timeout: 30000 });
      let stdout = ''; let stderr = ''; let error;
      child.stdout.on('data', (chunk) => { stdout += chunk; });
      child.stderr.on('data', (chunk) => { stderr += chunk; });
      child.on('error', (value) => { error = value; });
      child.on('close', (status, signal) => resolveResult({ status, signal, error, stdout, stderr }));
    });
    const run = { unit, argv, status: result.status, signal: result.signal, error: result.error?.code,
      stdout: result.stdout.trim(), stderr: result.stderr.trim() };
    if (result.error || result.signal || result.status !== 0) return run;
    run.observation = JSON.parse(result.stdout.trim().split('\n').at(-1));
    assert.notEqual(run.observation.own.mountNamespace, before.mountNamespace);
    assert.equal(run.observation.own.uid, before.uid);
    assert(existsSync(join(backing, 'tmp/p0-retained-marker')));
    assert(existsSync(join(backing, 'var-tmp/p0-retained-marker')));
    return run;
  }));
  writeFileSync(join(root, 'receipt.json'), JSON.stringify({ before, runs }));
  if (runs.some((run) => run.error || run.signal)) {
    // A launcher timeout is not proof that the unit stopped. Keep exact identity.
    console.log(JSON.stringify({ result: 'Unknown', root, before, runs, cleanup: 'held: inspect exact owned unit' }));
    process.exitCode = 2;
  } else {
    const succeeded = runs.length === 2 && runs.every((run) => run.status === 0);
    if (succeeded) {
      assert.notEqual(runs[0].observation.own.mountNamespace, runs[1].observation.own.mountNamespace);
      assert.notEqual(runs[0].observation.own.cgroup, runs[1].observation.own.cgroup);
    }
    console.log(JSON.stringify({ result: succeeded ? 'structural-probe-passed' : 'rootless-route-failed', before, runs,
      retention: succeeded ? 'owned disk backing survived unit exits; restart rebind not tested' : 'not tested',
      cleanup: 'systemd-run returned; no independent post-collection cgroup proof' }));
    rmSync(root, { recursive: true });
  }
}
