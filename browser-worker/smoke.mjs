import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { once } from 'node:events';
import { BrowserWorker } from './worker.mjs';

if (process.env.BROWSER_REAL_SMOKE !== '1') {
  process.stderr.write('真实 Camoufox smoke 未执行；请安装官方依赖和浏览器后设置 BROWSER_REAL_SMOKE=1。\n');
  process.exitCode = 2;
} else {
  const bytes = Buffer.from([0, 255, 0x61, 0xe2, 0x82, 0xac, 0xe4, 0xb8, 0xad]);
  const server = createServer((request, response) => {
    if (request.url === '/bytes') {
      response.writeHead(200, { 'Content-Type': 'application/octet-stream' });
      response.write(bytes.subarray(0, 4));
      setImmediate(() => response.end(bytes.subarray(4)));
    } else if (request.url === '/wait') {
      response.writeHead(200, { 'Content-Type': 'application/octet-stream' });
      response.write(bytes);
    } else if (request.url === '/attachment') {
      response.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Disposition': 'attachment; filename="sample.bin"' });
      response.end(Buffer.alloc(196608, 0xab));
    } else {
      response.writeHead(200, { 'Content-Type': 'text/html', 'Set-Cookie': 'worker_smoke=persisted; Path=/; SameSite=Lax' });
      response.end('<!doctype html><title>本地浏览器验证</title>');
    }
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const origin = `http://127.0.0.1:${server.address().port}`;
  const profileRoot = await mkdtemp(join(tmpdir(), 'camoufox-real-smoke-'));
  const events = [];
  let chunkResolve;
  const chunkSeen = new Promise(resolve => { chunkResolve = resolve; });
  let worker = new BrowserWorker({ profileRoot, maxBrowsers: 2, idleTimeoutMs: 0, emit: async event => { events.push(event); if (event.id === 'waiting' && event.type === 'chunk') chunkResolve(); } });
  const command = (id, op, extra = {}) => ({ id, account_id: 1, profile_id: 'local-smoke', op, proxy_url: '', timeout_ms: 0, url: origin + '/bytes', ...extra });
  const requireEnd = id => assert.ok(events.some(event => event.id === id && event.type === 'end'), JSON.stringify(events.filter(event => event.id === id)));
  const requireChunk = async id => {
    const deadline = Date.now() + 30000;
    while (!events.some(event => event.id === id && event.type === 'chunk')) {
      assert.ok(!events.some(event => event.id === id && event.type === 'error'), JSON.stringify(events.filter(event => event.id === id)));
      assert.ok(Date.now() < deadline, '真实请求未返回首个分块：' + id);
      await new Promise(resolve => setTimeout(resolve, 20));
    }
  };
  try {
    await worker.dispatch(command('stream', 'request'));
    requireEnd('stream');
    const received = Buffer.concat(events.filter(event => event.id === 'stream' && event.type === 'chunk').map(event => Buffer.from(event.data_base64, 'base64')));
    assert.deepEqual(received, bytes);
    const identityPath = join(profileRoot, 'local-smoke', 'identity.json');
    const identity = await readFile(identityPath, 'utf8');
    const context = worker.profiles.get('local-smoke').context;
    const page = await context.newPage();
    await page.goto(origin);
    await page.evaluate(() => localStorage.setItem('worker_smoke', 'saved'));
    await page.close();
    assert.ok((await context.cookies(origin)).some(cookie => cookie.name === 'worker_smoke'));
    // 两个真实持久档案必须分别保存 Cookie、LocalStorage 和已生成的指纹。
    await context.addCookies([{ name: 'profile_smoke', value: 'one', url: origin, expires: Math.floor(Date.now() / 1000) + 3600 }]);
    const firstFingerprintPage = await context.newPage();
    await firstFingerprintPage.goto(origin);
    const firstFingerprint = await firstFingerprintPage.evaluate(() => ({ ua: navigator.userAgent, width: screen.width, height: screen.height, cores: navigator.hardwareConcurrency }));
    await firstFingerprintPage.close();
    await worker.dispatch(command('second', 'request', { account_id: 2, profile_id: 'local-smoke-two' }));
    requireEnd('second');
    const secondContext = worker.profiles.get('local-smoke-two').context;
    assert.ok(!(await secondContext.cookies(origin)).some(cookie => cookie.name === 'profile_smoke'));
    const secondPage = await secondContext.newPage();
    await secondPage.goto(origin);
    assert.equal(await secondPage.evaluate(() => localStorage.getItem('worker_smoke')), null);
    await secondPage.evaluate(() => localStorage.setItem('worker_smoke', 'second-saved'));
    await secondContext.addCookies([{ name: 'profile_smoke', value: 'two', url: origin, expires: Math.floor(Date.now() / 1000) + 3600 }]);
    await secondPage.close();
    assert.equal((await context.cookies(origin)).find(cookie => cookie.name === 'profile_smoke')?.value, 'one');
    assert.notEqual(await readFile(join(profileRoot, 'local-smoke-two', 'identity.json'), 'utf8'), identity);
    const waiting = worker.dispatch(command('waiting', 'request', { url: origin + '/wait' }));
    await chunkSeen;
    await worker.dispatch(command('other', 'request'));
    await worker.dispatch(command('cancel', 'cancel', { target_id: 'waiting' }));
    await waiting;
    requireEnd('other');
    assert.equal(events.find(event => event.id === 'waiting' && event.type === 'error')?.code, 'cancelled');
    await worker.dispatch(command('attachment', 'download', { url: origin + '/attachment' }));
    requireEnd('attachment');
    assert.equal(Buffer.concat(events.filter(event => event.id === 'attachment' && event.type === 'chunk').map(event => Buffer.from(event.data_base64, 'base64'))).length, 196608);
    await worker.shutdown();
    worker = new BrowserWorker({ profileRoot, maxBrowsers: 2, idleTimeoutMs: 0, emit: async event => events.push(event) });
    await worker.dispatch(command('restart', 'request'));
    requireEnd('restart');
    assert.equal(await readFile(identityPath, 'utf8'), identity);
    const restored = worker.profiles.get('local-smoke').context;
    const restoredPage = await restored.newPage();
    await restoredPage.goto(origin);
    assert.equal(await restoredPage.evaluate(() => localStorage.getItem('worker_smoke')), 'saved');
    assert.equal((await restored.cookies(origin)).find(cookie => cookie.name === 'profile_smoke')?.value, 'one');
    assert.deepEqual(await restoredPage.evaluate(() => ({ ua: navigator.userAgent, width: screen.width, height: screen.height, cores: navigator.hardwareConcurrency })), firstFingerprint);
    await worker.dispatch(command('second-restart', 'request', { account_id: 2, profile_id: 'local-smoke-two' }));
    requireEnd('second-restart');
    const secondRestored = worker.profiles.get('local-smoke-two').context;
    assert.equal((await secondRestored.cookies(origin)).find(cookie => cookie.name === 'profile_smoke')?.value, 'two');
    const secondRestoredPage = await secondRestored.newPage();
    await secondRestoredPage.goto(origin);
    assert.equal(await secondRestoredPage.evaluate(() => localStorage.getItem('worker_smoke')), 'second-saved');
    await secondRestoredPage.close();
    assert.ok((await restored.cookies(origin)).some(cookie => cookie.name === 'worker_smoke' && cookie.value === 'persisted'));
    await restoredPage.close();
    await worker.shutdown();

    // 默认单槽必须真实淘汰后台浏览器；缩短空闲时间只用于本地回收验证。
    worker = new BrowserWorker({ profileRoot, idleTimeoutMs: 2000, emit: async event => events.push(event) });
    await worker.dispatch(command('single-first', 'request'));
    requireEnd('single-first');
    const singleFirst = worker.profiles.get('local-smoke').context;
    const firstClosed = once(singleFirst, 'close');
    const singleSecondWork = worker.dispatch(command('single-second', 'request', { account_id: 2, profile_id: 'local-smoke-two', url: origin + '/wait' }));
    await requireChunk('single-second');
    await firstClosed;
    assert.equal(worker.profiles.get('local-smoke').context, null);
    assert.equal([...worker.profiles.values()].filter(profile => profile.context).length, 1);
    const singleSecond = worker.profiles.get('local-smoke-two').context;
    const idleClosed = once(singleSecond, 'close');
    const singleSecondPage = await singleSecond.newPage();
    await singleSecondPage.goto(origin);
    assert.equal(await singleSecondPage.evaluate(() => localStorage.getItem('worker_smoke')), 'second-saved');
    assert.equal((await singleSecond.cookies(origin)).find(cookie => cookie.name === 'profile_smoke')?.value, 'two');
    await singleSecondPage.close();
    await worker.dispatch(command('single-cancel-second', 'cancel', { account_id: 2, profile_id: 'local-smoke-two', target_id: 'single-second' }));
    await singleSecondWork;
    assert.equal(events.find(event => event.id === 'single-second' && event.type === 'error')?.code, 'cancelled');
    let idleDeadline;
    try {
      await Promise.race([idleClosed, new Promise((_, reject) => { idleDeadline = setTimeout(() => reject(new Error('真实浏览器空闲后未释放')), 10000); })]);
    } finally { clearTimeout(idleDeadline); }
    assert.equal([...worker.profiles.values()].filter(profile => profile.context).length, 0);
    const singleRestoredWork = worker.dispatch(command('single-reopen', 'request', { url: origin + '/wait' }));
    await requireChunk('single-reopen');
    const singleRestored = worker.profiles.get('local-smoke').context;
    assert.equal((await singleRestored.cookies(origin)).find(cookie => cookie.name === 'profile_smoke')?.value, 'one');
    assert.equal(await readFile(identityPath, 'utf8'), identity);
    const singleRestoredPage = await singleRestored.newPage();
    await singleRestoredPage.goto(origin);
    assert.equal(await singleRestoredPage.evaluate(() => localStorage.getItem('worker_smoke')), 'saved');
    assert.deepEqual(await singleRestoredPage.evaluate(() => ({ ua: navigator.userAgent, width: screen.width, height: screen.height, cores: navigator.hardwareConcurrency })), firstFingerprint);
    await singleRestoredPage.close();
    await worker.dispatch(command('single-cancel-restored', 'cancel', { target_id: 'single-reopen' }));
    await singleRestoredWork;
    assert.equal(events.find(event => event.id === 'single-reopen' && event.type === 'error')?.code, 'cancelled');
    process.stderr.write('PASS：真实本地 Camoufox 流式请求、取消隔离、下载、双档案隔离、默认单槽切换、空闲释放和 Cookie/LocalStorage/指纹恢复；未访问 Google。\n');
  } finally {
    await worker.shutdown();
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    await rm(profileRoot, { recursive: true, force: true });
  }
}
