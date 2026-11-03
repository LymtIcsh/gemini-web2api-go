import assert from 'node:assert/strict';
import test from 'node:test';
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdir, mkdtemp, readFile, rm, access, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, isAbsolute, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createInterface } from 'node:readline';
import { once } from 'node:events';
import { camoufoxFactory } from '../worker.mjs';

const workerURL = new URL('../worker.mjs', import.meta.url);
const shutdown = JSON.stringify({ id: 'shutdown', account_id: 0, op: 'shutdown' }) + '\n';

async function temporaryFixture() {
  const base = resolve(tmpdir());
  const directory = await mkdtemp(join(base, 'gw2a-temp-runtime-'));
  return {
    directory,
    async remove() {
      // 只清理本测试在临时目录下实际创建的目录。
      assert.equal(dirname(directory), base);
      assert.ok(directory.startsWith(join(base, 'gw2a-temp-runtime-')));
      await rm(directory, { recursive: true, force: true });
    },
  };
}

function childEnvironment(profileRoot, inheritedTemp) {
  return {
    ...process.env,
    BROWSER_PROFILE_ROOT: profileRoot,
    BROWSER_MAX_BROWSERS: '1',
    BROWSER_IDLE_TIMEOUT_MS: '0',
    TEMP: inheritedTemp,
    TMP: inheritedTemp,
    TMPDIR: inheritedTemp,
  };
}

function probeProcess(source, environment, signal) {
  return new Promise((resolveResult, reject) => {
    const child = spawn(process.execPath, ['--input-type=module', '-e', source], {
      env: environment, signal, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'],
    });
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => { stdout += chunk; });
    child.stderr.on('data', chunk => { stderr += chunk; });
    child.once('error', reject);
    child.once('close', code => resolveResult({ code, stdout, stderr }));
    child.stdin.end(shutdown);
  });
}

// 故障目标：IPC 启动继续依赖会话 TEMP，目录删除后连普通 mkdtemp 都失败。
for (const inheritedExists of [false, true]) {
  test('IPC 临时文件独立于' + (inheritedExists ? '有效' : '已删除') + '的会话目录', { timeout: 15000 }, async t => {
    const fixture = await temporaryFixture();
    t.after(() => fixture.remove());
    const profileRoot = join(fixture.directory, '账号 档案');
    const inheritedTemp = join(fixture.directory, 'session-temp');
    if (inheritedExists) await mkdir(inheritedTemp);
    const parentTemp = tmpdir();
    const source = [
      'import { runIPC } from ' + JSON.stringify(workerURL.href) + ';',
      "import { tmpdir } from 'node:os';",
      "import { mkdtemp, writeFile, readFile } from 'node:fs/promises';",
      "import { join } from 'node:path';",
      'await runIPC();',
      "const directory = await mkdtemp(join(tmpdir(), 'probe-'));",
      "await writeFile(join(directory, 'value.txt'), 'temporary-files-work');",
      "console.log(JSON.stringify({ type: 'probe', root: tmpdir(), value: await readFile(join(directory, 'value.txt'), 'utf8') }));",
    ].join('\n');
    const result = await probeProcess(source, childEnvironment(profileRoot, inheritedTemp), t.signal);
    assert.equal(result.code, 0, result.stderr);
    const events = result.stdout.trim().split('\n').map(line => JSON.parse(line));
    assert.equal(events[0].type, 'ready');
    assert.equal(events.find(event => event.id === 'shutdown')?.type, 'end');
    const probe = events.find(event => event.type === 'probe');
    assert.equal(probe?.value, 'temporary-files-work');
    const withinProfile = relative(profileRoot, probe.root);
    assert.ok(withinProfile && !withinProfile.startsWith('..') && !isAbsolute(withinProfile));
    assert.equal(await access(inheritedTemp).then(() => true, () => false), inheritedExists);
    assert.equal(tmpdir(), parentTemp, '子进程配置不得修改调用方的环境');
  });
}

// 故障目标：Playwright 创建 artifacts 或附件下载文件仍使用失效的会话目录。
test('失效 TEMP 下真实 IPC 浏览器可启动、流式请求和附件下载', { timeout: 60000 }, async t => {
  const fixture = await temporaryFixture();
  const profileRoot = join(fixture.directory, 'profiles');
  const inheritedTemp = join(fixture.directory, 'deleted-session-temp');
  const bytes = Buffer.from([0, 255, 0x61, 0xe2, 0x82, 0xac, 0xe4, 0xb8, 0xad]);
  const server = createServer((request, response) => {
    if (request.url === '/bytes' || request.url === '/attachment') {
      const headers = { 'Content-Type': 'application/octet-stream' };
      if (request.url === '/attachment') headers['Content-Disposition'] = 'attachment; filename="sample.bin"';
      response.writeHead(200, headers);
      response.write(bytes.subarray(0, 4));
      response.end(bytes.subarray(4));
    } else {
      response.writeHead(200, { 'Content-Type': 'text/html' });
      response.end('<!doctype html><title>临时目录本地验证</title>');
    }
  });
  let child, lines, completion;
  let stderr = '';
  t.after(async () => {
    if (child && child.exitCode === null && !child.killed) {
      child.stdin.end(shutdown);
      const force = setTimeout(() => {
        if (process.platform === 'win32') {
          spawn('taskkill.exe', ['/PID', String(child.pid), '/T', '/F'], { windowsHide: true, stdio: 'ignore' });
        } else child.kill('SIGKILL');
      }, 10000);
      force.unref();
      await completion;
      clearTimeout(force);
    }
    lines?.close();
    server.closeAllConnections();
    await new Promise(resolveClosed => server.close(resolveClosed));
    await fixture.remove();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const origin = 'http://127.0.0.1:' + server.address().port;
  const identity = await camoufoxFactory.generate({ os: process.platform === 'win32' ? 'windows' : process.platform === 'darwin' ? 'macos' : 'linux' });
  const profileDirectory = join(profileRoot, 'local-smoke');
  await mkdir(profileDirectory, { recursive: true });
  const identityFile = join(profileDirectory, 'identity.json');
  const savedIdentity = JSON.stringify(identity);
  await writeFile(identityFile, savedIdentity);
  child = spawn(process.execPath, [fileURLToPath(workerURL)], {
    env: childEnvironment(profileRoot, inheritedTemp), windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'],
  });
  completion = new Promise(resolveClosed => child.once('close', resolveClosed));
  child.stderr.on('data', chunk => { stderr += chunk; });
  lines = createInterface({ input: child.stdout, crlfDelay: Infinity });
  const messages = lines[Symbol.asyncIterator]();
  async function receive(id) {
    const events = [];
    for (;;) {
      const line = await messages.next();
      assert.equal(line.done, false, stderr);
      const event = JSON.parse(line.value);
      if (event.id !== id) continue;
      events.push(event);
      if (['ready', 'end', 'error'].includes(event.type)) return events;
    }
  }
  assert.equal((await receive(''))[0].type, 'ready');
  for (const [op, route] of [['request', '/bytes'], ['download', '/attachment']]) {
    child.stdin.write(JSON.stringify({ id: op, account_id: 1, profile_id: 'local-smoke', op, url: origin + route, timeout_ms: 30000 }) + '\n');
    const events = await receive(op);
    assert.equal(events.at(-1).type, 'end', JSON.stringify(events));
    assert.equal(events.find(event => event.type === 'start')?.status, 200);
    const received = Buffer.concat(events.filter(event => event.type === 'chunk').map(event => Buffer.from(event.data_base64, 'base64')));
    assert.deepEqual(received, bytes);
  }
  assert.equal(await access(inheritedTemp).then(() => true, () => false), false);
  assert.equal(await readFile(identityFile, 'utf8'), savedIdentity);
});
