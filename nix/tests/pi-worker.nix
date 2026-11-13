{ pkgs, piWorker, piRuntimeManifest, src }:
pkgs.runCommand "dev-workspace-pi-worker-contract" {
  nativeBuildInputs = [ pkgs.python3 pkgs.jq ];
} ''
  export HOME="$TMPDIR/home"
  mkdir -p "$HOME"
  # Exercise the actual CI filter, including the VM exclusion, without a VM.
  python3 - ${src}/.github/workflows/check.yml ${pkgs.jq}/bin/jq <<'PY'
  import json, re, subprocess, sys
  workflow = open(sys.argv[1], encoding="utf-8").read()
  filters = re.findall(r"jq -r '([^']+)' <<< \"\$check_names\"", workflow)
  assert len(filters) == 1, "ambiguous workflow filter"
  names = ["package", "pi-worker", "pi-runtime-host", "host-module-idempotency",
           "session-recovery-boot", "session-archive-lifecycle", "session-creation"]
  fast = subprocess.check_output([sys.argv[2], "-r", filters[0]], input=json.dumps(names), text=True).splitlines()
  assert fast == [".#checks.x86_64-linux.package", ".#checks.x86_64-linux.pi-worker"], fast
  host = workflow.split("\n  host:\n", 1)[1]
  assert host.splitlines()[0].strip() == "if: github.event_name == 'workflow_dispatch' || (github.event_name == 'push' && github.ref == 'refs/heads/master')"
  assert re.findall(r"\.#checks\.x86_64-linux\.([a-z-]+)", host) == [
      "host-module-idempotency", "session-recovery-boot", "pi-runtime-host"]
  PY
  # No ambient tools, npm resources, caller HOME, account files or model calls.
  ${pkgs.coreutils}/bin/env -i PATH=/empty HOME="$HOME" TMPDIR="$TMPDIR" PI_OFFLINE=1 \
    ${piWorker.nodejs}/bin/node --input-type=module - <<'JS'
  import assert from 'node:assert/strict';
  import { spawn } from 'node:child_process';
  import { once } from 'node:events';
  import { randomUUID } from 'node:crypto';
  import { mkdtempSync, mkdirSync, readFileSync, readdirSync, renameSync, statSync, writeFileSync } from 'node:fs';
  import { tmpdir } from 'node:os';
  import { join } from 'node:path';
  import { pathToFileURL } from 'node:url';
  import { loadSDK } from '${piWorker}/libexec/pi-runtime/package.mjs';

  const manifest = JSON.parse(readFileSync('${piWorker}/share/dev-workspace/pi-worker.json', 'utf8'));
  assert.deepEqual(manifest, {
    schema: 1, worker_protocol: 1,
    node_executable: '${piWorker.nodejs}/bin/node',
    worker_entrypoint: '${piWorker}/libexec/pi-runtime/worker.mjs',
    pi_package_root: '${piWorker.piPackage}',
  });
  assert(Buffer.byteLength(JSON.stringify(manifest)) <= 4096);
  assert(statSync(manifest.node_executable).isFile());
  assert(statSync(manifest.worker_entrypoint).isFile());
  const wrapper = readFileSync(join(manifest.pi_package_root, 'bin/pi'), 'utf8');
  assert(wrapper.includes(manifest.node_executable), 'worker Node differs from coherent upstream Pi');
  assert.deepEqual(JSON.parse(readFileSync('${piRuntimeManifest}', 'utf8')), {
    schema: 1, host_launcher_contract: 1, launch_request_schema: 1, handshake_schema: 1,
    supervisor_entrypoint: 'bin/workspace-portal', supervisor_command: ['pi-runtime', 'supervise'],
  });
  function moduleFiles(root) {
    return readdirSync(root, { withFileTypes: true }).flatMap(entry =>
      entry.isDirectory() ? moduleFiles(join(root, entry.name)) : [join(root, entry.name)]);
  }
  for (const file of moduleFiles('${piWorker}/libexec/pi-runtime')) {
    assert(file.endsWith('.mjs') && !file.endsWith('.test.mjs'), 'fixture or resource installed in production worker');
    assert(!/from\s+['"][^'"]*test\//.test(readFileSync(file, 'utf8')));
  }

  const child = spawn(manifest.node_executable, [manifest.worker_entrypoint], {
    env: { PI_OFFLINE: '1' }, stdio: ['pipe', 'pipe', 'pipe'],
  });
  let stdout = "", stderr = "";
  child.stdout.on('data', value => { stdout += value; });
  child.stderr.on('data', value => { stderr += value; });
  const closed = once(child, 'close');
  child.stdin.end(JSON.stringify({ schema: 1, id: 'packaged-smoke', identity: {}, op: 'version', body: {} }) + '\n');
  const [code] = await closed;
  assert.equal(code, 0, stderr);
  assert.deepEqual(JSON.parse(stdout.trim()), {
    schema: 1, id: 'packaged-smoke', identity: {}, body: { workerProtocol: 1 },
  });

  const sdk = await loadSDK(manifest.pi_package_root);
  assert.equal(typeof sdk.createAgentSession, 'function');
  const aiRoot = join(manifest.pi_package_root, 'lib/pi/node_modules/@earendil-works/pi-ai');
  const aiManifest = JSON.parse(readFileSync(join(aiRoot, 'package.json'), 'utf8'));
  const ai = await import(pathToFileURL(join(aiRoot, aiManifest.exports?.['.']?.import ?? aiManifest.main)).href);
  const root = mkdtempSync(join(tmpdir(), 'pi-package-private-'));
  const auth = join(root, 'shared-fake-auth');
  mkdirSync(auth, { mode: 0o700 });
  const authFile = join(auth, 'auth.json');
  writeFileSync(authFile, JSON.stringify({ openai: { type: 'api_key', key: 'fake-initial' } }), { mode: 0o600 });
  const members = [];
  for (const name of ['lead', 'member']) {
    const member = join(root, name);
    for (const dir of ["", '/repo', '/agent', '/history']) mkdirSync(member + dir, { mode: 0o700 });
    const settings = sdk.SettingsManager.create(join(member, 'agent'), join(member, 'agent'));
    settings.setDefaultModel(name + '-model');
    await settings.flush();
    const history = sdk.SessionManager.create(join(member, 'repo'), join(member, 'history'), { id: randomUUID() });
    history.appendMessage({ role: 'user', content: [{ type: 'text', text: name + ' private history' }], timestamp: Date.now() });
    history.appendMessage(ai.fauxAssistantMessage('offline fixture'));
    const runtime = await sdk.ModelRuntime.create({ authPath: authFile, modelsPath: null, allowModelNetwork: false });
    assert.equal((await runtime.getAuth('openai')).auth.apiKey, 'fake-initial');
    members.push({ member, settings, history, runtime });
  }
  assert.notEqual(members[0].history.getSessionFile(), members[1].history.getSessionFile());
  for (const { member, history } of members) {
    assert(history.getSessionFile().startsWith(join(member, 'history') + '/'));
    assert.equal(sdk.SessionManager.open(history.getSessionFile()).getEntries().filter(entry => entry.type === 'message' && entry.message.role === 'user').length, 1);
  }
  members[0].settings.setDefaultModel('lead-changed');
  await members[0].settings.flush();
  assert.equal(JSON.parse(readFileSync(join(members[1].member, 'agent/settings.json'), 'utf8')).defaultModel, 'member-model');
  const next = join(auth, 'replacement.json');
  writeFileSync(next, JSON.stringify({ openai: { type: 'api_key', key: 'fake-replaced' } }), { mode: 0o600 });
  renameSync(next, authFile);
  for (const { runtime } of members) {
    assert.equal((await runtime.getAuth('openai')).auth.apiKey, 'fake-replaced');
    await runtime.settleCredentialWrites();
  }
  JS
  ${pkgs.coreutils}/bin/env -i PATH=/empty HOME="$HOME" TMPDIR="$TMPDIR" PI_OFFLINE=1 \
    ${piWorker.nodejs}/bin/node --test ${src}/pi-runtime/src/host-tools/reporting.test.mjs
  # Keep the checked runtime closure alive with the successful check output.
  mkdir -p "$out"
  ln -s ${piWorker} "$out/worker"
''
