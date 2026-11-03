import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { PassThrough, Writable } from 'node:stream';
import { runInNewContext } from 'node:vm';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { BrowserWorker, writeJSONLine, runIPC, inspectGeminiIdentity, fetchInPage } from '../worker.mjs';

const tick = () => new Promise(resolve => setImmediate(resolve));
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; }
const request = (id, profile = 'one', op = 'request', extra = {}) => ({ id, account_id: profile === 'one' ? 1 : 2, profile_id: profile, op, expected_email: 'actual@example.com', proxy_url: '', timeout_ms: 0, url: 'https://gemini.google.com/api', ...extra });

// 仅浏览器边界使用测试设备；档案、调度、IPC 和流处理均运行生产实现。
function browserDevice() {
  const launches = [];
  let generated = 0;
  const factory = {
    async generate({ os }) { return { executablePath: '/browser', args: [], env: { CAMOU_CONFIG_1: JSON.stringify({ identity: ++generated, os }) }, firefoxUserPrefs: {} }; },
    async launch(options) {
      const pages = [];
      const contextListeners = new Map();
      const context = {
        options, pages, closed: false,
        cookieList: [{ name: 'SAPISID', value: 'auth', domain: '.google.com', path: '/', expires: -1, httpOnly: true, secure: true, sameSite: 'None' }],
        async cookies() { return this.cookieList; },
        on(name, listener) { contextListeners.set(name, listener); },
        async close() { this.closed = true; await Promise.all(pages.map(page => page.close())); contextListeners.get('close')?.(); },
        async newPage() {
          if (this.closed) throw new Error('Browser context closed');
          let abort;
          const stopped = new Promise((_, reject) => { abort = () => reject(new Error('page closed')); });
          stopped.catch(() => {});
          const listeners = new Map();
          const page = {
            closed: false, binding: null, identity: { email: 'actual@example.com', emails: ['actual@example.com'], token: 'verified-token', url: 'https://gemini.google.com/app' },
            async goto(url) {
              this.address = url;
              if (url.endsWith('/attachment') || url.endsWith('/attachment-wait')) {
                listeners.get('response')?.({ status: () => 206, allHeaders: async () => ({ 'content-type': 'video/mp4' }), url: () => url });
                listeners.get('download')?.({ saveAs: async file => { await writeFile(file, Buffer.alloc(196608, 0xaa)); if (url.endsWith('/attachment-wait')) await stopped; }, cancel: async () => abort(), suggestedFilename: () => 'test.mp4', url: () => url, delete: async () => {} });
                throw new Error('Download is starting');
              }
              if (url.endsWith('/missing')) return { status: () => 404, allHeaders: async () => ({ 'content-type': 'text/html' }), url: () => url };
              return null;
            },
            on(name, listener) { listeners.set(name, listener); },
            url() { return this.address; },
            async exposeBinding(name, fn) { this.binding = fn; },
            async evaluate(fn, arg) {
              if (!arg?.url) return this.identity;
              this.fetchArgs = arg;
              const work = (async () => {
                await this.binding({}, { type: 'start', status: arg.url.endsWith('/missing') ? 404 : 200, headers: { 'content-type': arg.url.endsWith('/missing') ? 'text/html' : 'application/octet-stream' }, url: arg.url });
                await this.binding({}, { type: 'chunk', data_base64: 'AP9h4g==' });
                if (arg.url.endsWith('/wait')) await stopped;
                await this.binding({}, { type: 'chunk', data_base64: 'gqzkuK0=' });
              })();
              return Promise.race([work, stopped]);
            },
            isClosed() { return this.closed; },
            async close() { if (this.closed) return; this.closed = true; abort(); listeners.get('close')?.(); },
          };
          pages.push(page);
          return page;
        },
      };
      launches.push(context);
      return context;
    },
  };
  return { factory, launches, get generated() { return generated; } };
}

async function fixture(t, emit, options = {}) {
  const profileRoot = await mkdtemp(join(tmpdir(), 'browser-worker-test-'));
  const device = browserDevice();
  const events = [];
  const worker = new BrowserWorker({ profileRoot, browserFactory: device.factory, maxBrowsers: 2, idleTimeoutMs: 0, ...options, emit: emit || (async event => { events.push(event); }) });
  t.after(async () => { await worker.shutdown(); await rm(profileRoot, { recursive: true, force: true }); });
  return { worker, device, events, profileRoot };
}

// 故障目标：账号切换和迟到启动不得突破实际驻留上限，回收不能删除磁盘身份。
test('浏览器容量单槽切换多账号并恢复原指纹', async t => {
  const f = await fixture(t, undefined, { maxBrowsers: 1 });
  for (const [id, profile] of [['first', 'one'], ['second', 'two'], ['third', 'three'], ['return', 'one']]) {
    await f.worker.dispatch(request(id, profile));
    assert.equal(f.events.find(event => event.id === id && event.type === 'end')?.account_id, profile === 'one' ? 1 : 2);
    assert.equal(f.device.launches.filter(context => !context.closed).length, 1, '账号数不能变成驻留浏览器数');
  }
  assert.equal(f.device.generated, 3);
  assert.deepEqual(f.device.launches.at(-1).options.identity, f.device.launches[0].options.identity);
  assert.equal(JSON.parse(await readFile(join(f.profileRoot, 'one', 'identity.json'), 'utf8')).env.CAMOU_CONFIG_1, f.device.launches[0].options.identity.env.CAMOU_CONFIG_1);
});

test('容量排队不结束活动流且取消后下一账号正常发送', async t => {
  const { worker, device, events } = await fixture(t, undefined, { maxBrowsers: 1 });
  const working = worker.dispatch(request('working', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(event => event.id === 'working' && event.type === 'chunk')) await tick();
  const queued = worker.dispatch(request('queued', 'two'));
  await tick();
  await tick();
  assert.equal(device.launches.length, 1);
  assert.equal(device.launches[0].closed, false);
  assert.equal(events.some(event => event.id === 'queued'), false);
  await worker.dispatch(request('release', 'one', 'cancel', { target_id: 'working' }));
  await working;
  await queued;
  assert.equal(events.find(event => event.id === 'working' && event.type === 'error')?.code, 'cancelled');
  assert.equal(events.find(event => event.id === 'queued' && event.type === 'end')?.account_id, 2);
  assert.equal(device.launches.filter(context => !context.closed).length, 1);
});

test('容量排队超时不启动浏览器且不取消别的账号', async t => {
  const { worker, device, events } = await fixture(t, undefined, { maxBrowsers: 1 });
  const working = worker.dispatch(request('working', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(event => event.id === 'working' && event.type === 'chunk')) await tick();
  await worker.dispatch(request('expired', 'two', 'request', { timeout_ms: 20 }));
  assert.equal(events.find(event => event.id === 'expired' && event.type === 'error')?.code, 'timeout');
  assert.equal(device.launches.length, 1);
  assert.equal(device.launches[0].closed, false);
  await worker.dispatch(request('release', 'one', 'cancel', { target_id: 'working' }));
  await working;
  await worker.dispatch(request('next', 'two'));
  assert.ok(events.some(event => event.id === 'next' && event.type === 'end'));
});

async function waitForClosed(context) {
  const deadline = Date.now() + 500;
  while (!context.closed && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
  assert.equal(context.closed, true, '后台空闲浏览器应释放');
}

test('空闲回收释放后台浏览器并保留独立档案', async t => {
  const { worker, device, events, profileRoot } = await fixture(t, undefined, { maxBrowsers: 1, idleTimeoutMs: 20 });
  await worker.dispatch(request('first'));
  const identity = await readFile(join(profileRoot, 'one', 'identity.json'), 'utf8');
  await waitForClosed(device.launches[0]);
  await worker.dispatch(request('restore'));
  assert.ok(events.some(event => event.id === 'restore' && event.type === 'end'));
  assert.equal(device.generated, 1);
  assert.equal(await readFile(join(profileRoot, 'one', 'identity.json'), 'utf8'), identity);
  assert.deepEqual(device.launches.at(-1).options.identity, device.launches[0].options.identity);
});

test('空闲回收等待活动请求清理完成再释放', async t => {
  const { worker, device, events } = await fixture(t, undefined, { maxBrowsers: 1, idleTimeoutMs: 20 });
  const working = worker.dispatch(request('working', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(event => event.id === 'working' && event.type === 'chunk')) await tick();
  await new Promise(resolve => setTimeout(resolve, 40));
  assert.equal(device.launches[0].closed, false);
  assert.equal(events.some(event => event.id === 'working' && event.type === 'error'), false);
  await worker.dispatch(request('release', 'one', 'cancel', { target_id: 'working' }));
  await working;
  await waitForClosed(device.launches[0]);
});

test('浏览器容量和空闲回收保留用户正在使用的登录窗口', async t => {
  const { worker, device, events } = await fixture(t, undefined, { maxBrowsers: 1, idleTimeoutMs: 20 });
  await worker.dispatch(request('login', 'one', 'login'));
  const context = device.launches[0];
  const page = context.pages[0];
  await new Promise(resolve => setTimeout(resolve, 40));
  await worker.dispatch(request('expired', 'two', 'request', { timeout_ms: 20 }));
  assert.equal(events.find(event => event.id === 'expired' && event.type === 'error')?.code, 'timeout');
  assert.equal(device.launches.length, 1);
  assert.equal(context.closed, false);
  assert.equal(page.closed, false);
  await page.close();
  await worker.dispatch(request('next', 'two'));
  assert.ok(events.some(event => event.id === 'next' && event.type === 'end'));
  assert.equal(context.closed, true);
});

test('启动取消及时回应且迟到浏览器关闭后才释放容量', async t => {
  const entered = deferred();
  const release = deferred();
  t.after(() => release.resolve());
  const { worker, device, events } = await fixture(t, undefined, { maxBrowsers: 1 });
  const launch = device.factory.launch;
  let first = true;
  device.factory.launch = async options => {
    if (first) { first = false; entered.resolve(); await release.promise; }
    return launch(options);
  };
  const starting = worker.dispatch(request('starting'));
  await entered.promise;
  await worker.dispatch(request('cancel-start', 'one', 'cancel', { target_id: 'starting' }));
  await tick();
  assert.equal(events.find(event => event.id === 'starting' && event.type === 'error')?.code, 'cancelled');
  const queued = worker.dispatch(request('next', 'two'));
  await tick();
  assert.equal(device.launches.length, 0);
  release.resolve();
  await starting;
  await queued;
  assert.equal(device.launches[0].closed, true);
  assert.equal(device.launches.filter(context => !context.closed).length, 1);
  assert.ok(events.some(event => event.id === 'next' && event.type === 'end'));
});

test('档案隔离且重启沿用已保存身份', async t => {
  const f = await fixture(t);
  await Promise.all([f.worker.dispatch(request('a')), f.worker.dispatch(request('b', 'two'))]);
  assert.notEqual(f.device.launches[0].options.userDataDir, f.device.launches[1].options.userDataDir);
  const identity = JSON.parse(await readFile(join(f.profileRoot, 'one', 'identity.json'), 'utf8'));
  await f.worker.shutdown();
  const restart = new BrowserWorker({ profileRoot: f.profileRoot, browserFactory: f.device.factory, emit: async () => {} });
  t.after(() => restart.shutdown());
  await restart.dispatch(request('c'));
  assert.equal(f.device.generated, 2);
  assert.deepEqual(f.device.launches.at(-1).options.identity, identity);
});

test('并发账号事件按 ID 归属，二进制和拆分 UTF-8 原样保留', async t => {
  const { worker, events } = await fixture(t);
  await Promise.all([worker.dispatch(request('a')), worker.dispatch(request('b', 'two'))]);
  for (const [id, account] of [['a', 1], ['b', 2]]) {
    const own = events.filter(event => event.id === id);
    assert.deepEqual(own.map(event => event.type), ['start', 'chunk', 'chunk', 'end']);
    assert.ok(own.every(event => event.account_id === account));
    assert.equal(Buffer.concat(own.filter(event => event.type === 'chunk').map(event => Buffer.from(event.data_base64, 'base64'))).toString('hex'), '00ff61e282ace4b8ad');
  }
});

test('每个 chunk 等待输出背压后才继续读取', async t => {
  const events = [];
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const { worker } = await fixture(t, async event => { events.push(event); if (event.type === 'chunk' && events.filter(e => e.type === 'chunk').length === 1) await gate; });
  const running = worker.dispatch(request('backpressure'));
  while (events.length < 2) await tick();
  await tick();
  assert.deepEqual(events.map(event => event.type), ['start', 'chunk']);
  release();
  await running;
  assert.deepEqual(events.map(event => event.type), ['start', 'chunk', 'chunk', 'end']);
});

test('取消只关闭目标页面，同档案其他请求可继续完成', async t => {
  const { worker, device, events } = await fixture(t);
  const pending = worker.dispatch(request('waiting', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(e => e.id === 'waiting' && e.type === 'chunk')) await tick();
  await worker.dispatch(request('other'));
  await worker.dispatch(request('cancel', 'one', 'cancel', { target_id: 'waiting' }));
  await pending;
  assert.equal(events.find(e => e.id === 'waiting' && e.type === 'error').code, 'cancelled');
  assert.ok(events.some(e => e.id === 'other' && e.type === 'end'));
  assert.equal(device.launches[0].closed, false);
});

test('其他账号的取消请求不能取消目标账号任务', async t => {
  const { worker, events } = await fixture(t);
  const waiting = worker.dispatch(request('waiting', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(e => e.type === 'chunk')) await tick();
  await worker.dispatch(request('foreign-cancel', 'two', 'cancel', { target_id: 'waiting' }));
  assert.equal(events.some(e => e.id === 'waiting' && e.type === 'error'), false);
  await worker.dispatch(request('own-cancel', 'one', 'cancel', { target_id: 'waiting' }));
  await waiting;
  assert.equal(events.find(e => e.id === 'waiting' && e.type === 'error').code, 'cancelled');
});

test('Go 提供的请求超时只取消该任务', async t => {
  const { worker, events, device } = await fixture(t);
  await worker.dispatch(request('deadline', 'one', 'request', { url: 'https://gemini.google.com/wait', timeout_ms: 30 }));
  assert.equal(events.find(e => e.id === 'deadline' && e.type === 'error').code, 'timeout');
  assert.equal(device.launches[0].closed, false);
});

test('活动请求拒绝关闭及 headed 切换，空闲后切换保持身份', async t => {
  const { worker, device, events } = await fixture(t);
  const pending = worker.dispatch(request('waiting', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(e => e.type === 'chunk')) await tick();
  await worker.dispatch(request('close', 'one', 'close'));
  await worker.dispatch(request('login', 'one', 'login'));
  await worker.dispatch(request('proxy-change', 'one', 'request', { proxy_url: 'http://test:pass@127.0.0.1:8080' }));
  assert.equal(events.find(e => e.id === 'close').code, 'profile_busy');
  assert.equal(events.find(e => e.id === 'login').code, 'profile_busy');
  assert.equal(events.find(e => e.id === 'proxy-change').code, 'profile_busy');
  await worker.dispatch(request('cancel', 'one', 'cancel', { target_id: 'waiting' }));
  await pending;
  await worker.dispatch(request('open', 'one', 'open'));
  assert.equal(device.launches.at(-1).options.headless, false);
  assert.deepEqual(device.launches.at(-1).options.identity, device.launches[0].options.identity);
  assert.equal(device.generated, 1);
  await worker.dispatch(request('new-proxy', 'one', 'request', { proxy_url: 'http://test:pass@127.0.0.1:8080' }));
  assert.deepEqual(device.launches.at(-1).options.proxy, { server: 'http://127.0.0.1:8080', username: 'test', password: 'pass' });
  assert.equal(device.generated, 1);
});

test('登录确认采用页面身份及 token，缺 token 返回 login_required', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('login', 'one', 'login', { email: 'untrusted@example.com' }));
  await worker.dispatch(request('confirm', 'one', 'confirm-login', { email: 'untrusted@example.com' }));
  const session = events.find(e => e.id === 'confirm' && e.type === 'end').session;
  assert.deepEqual(session, { email: 'actual@example.com', cookie: 'SAPISID=auth', profile_id: 'one', login_state: 'ready' });
  const newPage = device.launches[0].newPage.bind(device.launches[0]);
  device.launches[0].newPage = async () => { const page = await newPage(); page.identity.token = ''; return page; };
  await worker.dispatch(request('missing', 'one', 'confirm-login'));
  const failed = events.find(e => e.id === 'missing' && e.type === 'error');
  assert.equal(failed.code, 'login_required');
  assert.equal(failed.session.login_state, 'needs_login');
});

test('后台准备页面不覆盖已打开的用户登录页', async t => {
  const { worker, device } = await fixture(t);
  await worker.dispatch(request('login', 'one', 'login'));
  const loginPage = device.launches[0].pages[0];
  loginPage.address = 'https://accounts.google.com/signin';
  await worker.dispatch(request('confirm', 'one', 'confirm-login'));
  assert.equal(loginPage.address, 'https://accounts.google.com/signin');
  assert.equal(loginPage.closed, false);
});

test('页面创建期间取消登录确认不能返回 ready', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const context = device.launches[0];
  const original = context.newPage.bind(context);
  const entered = deferred();
  const release = deferred();
  context.newPage = async () => { entered.resolve(); await release.promise; return original(); };
  const confirming = worker.dispatch(request('late-confirm', 'one', 'confirm-login'));
  await entered.promise;
  await worker.dispatch(request('cancel-confirm', 'one', 'cancel', { target_id: 'late-confirm' }));
  release.resolve();
  await confirming;
  assert.equal(events.find(e => e.id === 'late-confirm')?.code, 'cancelled');
  assert.equal(events.some(e => e.id === 'late-confirm' && e.type === 'end'), false);
});

test('取消共享登录页操作不关闭先前用户页面', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('initial-login', 'one', 'login'));
  const page = device.launches[0].pages[0];
  const entered = deferred();
  const release = deferred();
  const evaluate = page.evaluate.bind(page);
  page.evaluate = async (...args) => { entered.resolve(); await release.promise; return evaluate(...args); };
  const reopening = worker.dispatch(request('reopen', 'one', 'open'));
  await entered.promise;
  await worker.dispatch(request('concurrent-open', 'one', 'open'));
  assert.equal(events.find(e => e.id === 'concurrent-open')?.code, 'profile_busy');
  await worker.dispatch(request('cancel-open', 'one', 'cancel', { target_id: 'reopen' }));
  release.resolve();
  await reopening;
  assert.equal(page.closed, false);
  assert.equal(events.find(e => e.id === 'reopen')?.code, 'cancelled');
});

test('登录 Cookie 检查期间取消不能发 ready end', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const entered = deferred();
  const release = deferred();
  const context = device.launches[0];
  const cookies = context.cookies.bind(context);
  context.cookies = async () => { entered.resolve(); await release.promise; return cookies(); };
  const confirming = worker.dispatch(request('cookie-confirm', 'one', 'confirm-login'));
  await entered.promise;
  await worker.dispatch(request('cancel-cookie-confirm', 'one', 'cancel', { target_id: 'cookie-confirm' }));
  release.resolve();
  await confirming;
  assert.equal(events.find(e => e.id === 'cookie-confirm')?.code, 'cancelled');
  assert.equal(events.some(e => e.id === 'cookie-confirm' && e.type === 'end'), false);
});

test('Google 请求 end 返回响应后的浏览器 Cookie 与可验证身份', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup'));
  const context = device.launches[0];
  const original = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await original();
    const evaluate = page.evaluate.bind(page);
    page.evaluate = async (fn, arg) => { const result = await evaluate(fn, arg); if (arg?.url) context.cookieList[0].value = 'after-response'; return result; };
    return page;
  };
  await worker.dispatch(request('updated'));
  assert.deepEqual(events.find(e => e.id === 'updated' && e.type === 'end')?.session, { email: 'actual@example.com', cookie: 'SAPISID=after-response', profile_id: 'one', login_state: 'ready' });
});

test('Google SAPISIDHASH 采用已更新的当前浏览器 Cookie 重新计算', async t => {
  const { worker, device } = await fixture(t);
  await worker.dispatch(request('warmup'));
  const context = device.launches[0];
  context.cookieList[0].value = 'current-browser-cookie';
  await worker.dispatch(request('authorized', 'one', 'request', { headers: { Authorization: 'SAPISIDHASH 123_stale' } }));
  const value = context.pages.at(-1).fetchArgs.headers.Authorization;
  const match = /^SAPISIDHASH (\d+)_([a-f0-9]{40})$/.exec(value);
  assert.ok(match, value);
  assert.equal(match[2], createHash('sha1').update(`${match[1]} current-browser-cookie https://gemini.google.com`).digest('hex'));
});

test('Google 请求缺认证 Cookie 返回 login_required 并带 needs_login session', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup'));
  device.launches[0].cookieList = [];
  await worker.dispatch(request('auth-missing', 'one', 'request', { headers: { Authorization: 'SAPISIDHASH 123_stale' } }));
  const failed = events.find(e => e.id === 'auth-missing' && e.type === 'error');
  assert.equal(failed.code, 'login_required');
  assert.equal(failed.session.login_state, 'needs_login');
  assert.equal(failed.session.profile_id, 'one');
});

test('Google 下载 end 使用缓存已验证身份和最新 Cookie，不新开身份页', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('verified', 'one', 'refresh'));
  const context = device.launches[0];
  const count = context.pages.length;
  context.cookieList.push({ ...context.cookieList[0], name: '__Secure-1PSIDCC', value: 'rotated-ticket' });
  await worker.dispatch(request('google-media', 'one', 'download', { url: 'https://lh3.googleusercontent.com/attachment' }));
  assert.equal(context.pages.length, count + 1);
  assert.deepEqual(events.find(e => e.id === 'google-media' && e.type === 'end')?.session, { email: 'actual@example.com', cookie: 'SAPISID=auth; __Secure-1PSIDCC=rotated-ticket', profile_id: 'one', login_state: 'ready' });
});

test('账号认证 Cookie 改变后下载必须重新确认邮箱', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('verified-old', 'one', 'refresh'));
  const context = device.launches[0];
  context.cookieList[0].value = 'new-account-cookie';
  const original = context.newPage.bind(context);
  context.newPage = async () => { const page = await original(); page.identity.email = 'new@example.com'; page.identity.emails = ['new@example.com']; return page; };
  await worker.dispatch(request('switched-media', 'one', 'download', { url: 'https://lh3.googleusercontent.com/attachment', expected_email: 'new@example.com' }));
  const session = events.find(e => e.id === 'switched-media' && e.type === 'end')?.session;
  assert.equal(session?.email, 'new@example.com');
  assert.equal(session?.cookie, 'SAPISID=new-account-cookie');
  assert.equal(session?.login_state, 'ready');
});

test('headed 切换账号后模式及代理重开不能复用旧邮箱', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('old-login', 'one', 'login'));
  const launch = device.factory.launch.bind(device.factory);
  device.factory.launch = async options => {
    const context = await launch(options);
    context.cookieList[0].value = 'new-account-cookie';
    const original = context.newPage.bind(context);
    context.newPage = async () => { const page = await original(); page.identity.email = 'new@example.com'; page.identity.emails = ['new@example.com']; return page; };
    return context;
  };
  await worker.dispatch(request('reopened-media', 'one', 'download', { url: 'https://lh3.googleusercontent.com/attachment', proxy_url: 'http://127.0.0.1:8080', expected_email: 'new@example.com' }));
  const session = events.find(e => e.id === 'reopened-media' && e.type === 'end')?.session;
  assert.equal(session?.email, 'new@example.com');
  assert.equal(session?.cookie, 'SAPISID=new-account-cookie');
  assert.equal(device.launches.at(-1).options.headless, true);
});

test('下载身份检查期间再次切换 Cookie 拒绝配成 ready', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('old-verified', 'one', 'refresh'));
  const context = device.launches[0];
  context.cookieList[0].value = 'intermediate-account';
  const original = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await original();
    const evaluate = page.evaluate.bind(page);
    page.evaluate = async (fn, arg) => { const value = await evaluate(fn, arg); if (!arg?.url) context.cookieList[0].value = 'third-account'; return value; };
    return page;
  };
  await worker.dispatch(request('identity-race', 'one', 'download', { url: 'https://lh3.googleusercontent.com/attachment' }));
  const session = events.find(e => e.id === 'identity-race' && e.type === 'error')?.session;
  assert.equal(session?.login_state, 'needs_login');
  assert.equal(session?.email, '');
  assert.equal(session?.cookie, 'SAPISID=third-account');
});

test('登录确认读取邮箱期间切换 Cookie 不能绑定旧身份', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const context = device.launches[0];
  const original = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await original();
    const evaluate = page.evaluate.bind(page);
    page.evaluate = async (fn, arg) => { const value = await evaluate(fn, arg); if (!arg?.url) context.cookieList[0].value = 'new-account-cookie'; return value; };
    return page;
  };
  await worker.dispatch(request('confirm-race', 'one', 'confirm-login'));
  const failed = events.find(e => e.id === 'confirm-race' && e.type === 'error');
  assert.equal(failed?.code, 'login_required');
  assert.equal(failed?.session.email, '');
  assert.equal(failed?.session.cookie, 'SAPISID=new-account-cookie');
  assert.equal(failed?.session.login_state, 'needs_login');
  assert.equal(worker.profiles.get('one').identity, null);
});

test('请求前读取邮箱期间切换 Cookie 拒绝发送旧账号请求', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const context = device.launches[0];
  const original = context.newPage.bind(context);
  let fetched = false;
  context.newPage = async () => {
    const page = await original();
    const evaluate = page.evaluate.bind(page);
    page.evaluate = async (fn, arg) => { const value = await evaluate(fn, arg); if (arg?.url) fetched = true; else context.cookieList[0].value = 'new-account-cookie'; return value; };
    return page;
  };
  await worker.dispatch(request('request-identity-race'));
  const failed = events.find(e => e.id === 'request-identity-race' && e.type === 'error');
  assert.equal(failed?.code, 'login_required');
  assert.equal(failed?.session.email, '');
  assert.equal(failed?.session.login_state, 'needs_login');
  assert.equal(fetched, false);
  assert.equal(worker.profiles.get('one').identity, null);
});

test('Google 请求账号与 expected_email 不符时不发送生成或上传', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('headed', 'one', 'login'));
  const launch = device.factory.launch.bind(device.factory);
  device.factory.launch = async options => { const context = await launch(options); const newPage = context.newPage.bind(context); context.newPage = async () => { const page = await newPage(); page.identity.email = 'other@example.com'; page.identity.emails = ['other@example.com']; return page; }; return context; };
  await worker.dispatch(request('wrong-owner', 'one', 'request', { url: 'https://content-push.googleapis.com/upload' }));
  const own = events.filter(e => e.id === 'wrong-owner');
  assert.deepEqual(own.map(e => e.type), ['error']);
  assert.equal(own[0].code, 'account_mismatch');
  assert.equal(own[0].session.email, 'other@example.com');
  assert.equal(own[0].session.login_state, 'needs_login');
  assert.equal(device.launches.at(-1).pages.some(page => page.fetchArgs), false);
});

test('Google 下载账号与 expected_email 不符时不导航媒体或输出 chunk', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('headed', 'one', 'login'));
  await worker.dispatch(request('wrong-download', 'one', 'download', { url: 'https://lh3.googleusercontent.com/attachment', expected_email: 'owner@example.com' }));
  const own = events.filter(e => e.id === 'wrong-download');
  assert.deepEqual(own.map(e => e.type), ['error']);
  assert.equal(own[0].code, 'account_mismatch');
  assert.equal(device.launches.at(-1).pages.some(page => page.address?.endsWith('/attachment')), false);
});

test('Google 请求缺 expected_email 不发送，初次未绑定登录仍返回真实邮箱', async t => {
  const { worker, events } = await fixture(t);
  await worker.dispatch(request('unbound-request', 'one', 'request', { expected_email: '' }));
  assert.equal(events.find(e => e.id === 'unbound-request')?.code, 'login_required');
  assert.equal(events.some(e => e.id === 'unbound-request' && e.type === 'chunk'), false);
  await worker.dispatch(request('initial-login', 'one', 'login', { expected_email: '' }));
  assert.equal(events.find(e => e.id === 'initial-login' && e.type === 'end')?.session.email, 'actual@example.com');
});

test('绑定后的登录及确认邮箱不匹配返回 account_mismatch', async t => {
  const { worker, events } = await fixture(t);
  for (const op of ['login', 'open', 'confirm-login', 'refresh']) {
    await worker.dispatch(request('wrong-' + op, 'one', op, { expected_email: 'owner@example.com' }));
    const failed = events.find(e => e.id === 'wrong-' + op);
    assert.equal(failed.code, 'account_mismatch');
    assert.equal(failed.session.login_state, 'needs_login');
  }
});

test('既有登录页 Cookie 改变后 open 不能继续信任旧邮箱 DOM', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('first-login', 'one', 'login'));
  const context = device.launches[0];
  const userPage = context.pages[0];
  context.cookieList[0].value = 'other-account';
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); page.identity.email = 'other@example.com'; page.identity.emails = ['other@example.com']; return page; };
  await worker.dispatch(request('reopen-stale', 'one', 'open'));
  const failed = events.find(e => e.id === 'reopen-stale');
  assert.equal(failed.code, 'account_mismatch');
  assert.equal(failed.session.email, 'other@example.com');
  assert.equal(userPage.closed, false);
});

test('请求准备导航期间认证 Cookie 改变不得发送 owner DOM 配新 Cookie 请求', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const context = device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); const goto = page.goto.bind(page); page.goto = async (...args) => { const result = await goto(...args); context.cookieList[0].value = 'other-auth'; return result; }; return page; };
  await worker.dispatch(request('navigation-race'));
  const final = events.find(e => e.id === 'navigation-race');
  assert.equal(final.code, 'login_required');
  assert.equal(final.session.email, '');
  assert.equal(final.session.login_state, 'needs_login');
  assert.equal(context.pages.at(-1).fetchArgs, undefined);
});

test('登录确认导航期间认证 Cookie 改变不能确认旧 DOM 邮箱', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const context = device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); const goto = page.goto.bind(page); page.goto = async (...args) => { const result = await goto(...args); context.cookieList[0].value = 'other-auth'; return result; }; return page; };
  await worker.dispatch(request('confirm-nav-race', 'one', 'confirm-login'));
  const final = events.find(e => e.id === 'confirm-nav-race');
  assert.equal(final.code, 'login_required');
  assert.equal(final.session.email, '');
  assert.equal(final.session.login_state, 'needs_login');
});

test('下载临时确认页导航期间认证改变不导航媒体', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup', 'one', 'refresh'));
  const context = device.launches[0];
  worker.profiles.get('one').identity = null;
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); const goto = page.goto.bind(page); page.goto = async (...args) => { const result = await goto(...args); if (args[0] === 'https://gemini.google.com/app') context.cookieList[0].value = 'other-auth'; return result; }; return page; };
  await worker.dispatch(request('download-nav-race', 'one', 'download', { url: 'https://lh3.googleusercontent.com/attachment' }));
  const final = events.find(e => e.id === 'download-nav-race');
  assert.equal(final.code, 'login_required');
  assert.equal(final.session.email, '');
  assert.equal(context.pages.some(page => page.address?.endsWith('/attachment')), false);
});

test('身份校验后发送前认证 Cookie 改变时停止 Google fetch', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('verified', 'one', 'refresh'));
  const context = device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); const binding = page.exposeBinding.bind(page); page.exposeBinding = async (...args) => { await binding(...args); context.cookieList[0].value = 'switched-after-check'; }; return page; };
  await worker.dispatch(request('time-of-send'));
  const own = events.filter(e => e.id === 'time-of-send');
  assert.deepEqual(own.map(e => e.type), ['error']);
  assert.equal(own[0].code, 'login_required');
  assert.equal(context.pages.at(-1).fetchArgs, undefined);
});

test('请求响应后认证变化不能信任旧页面邮箱，需重新导航检查', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('verified', 'one', 'refresh'));
  const context = device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await newPage();
    const goto = page.goto.bind(page);
    page.goto = async (...args) => { if (context.cookieList[0].value === 'other-account') { page.identity.email = 'other@example.com'; page.identity.emails = ['other@example.com']; } return goto(...args); };
    const evaluate = page.evaluate.bind(page);
    page.evaluate = async (fn, arg) => { const result = await evaluate(fn, arg); if (arg?.url) context.cookieList[0].value = 'other-account'; return result; };
    return page;
  };
  await worker.dispatch(request('stale-dom'));
  const final = events.filter(e => e.id === 'stale-dom').at(-1);
  assert.equal(final.type, 'error');
  assert.equal(final.code, 'account_mismatch');
  assert.equal(final.session.email, 'other@example.com');
  assert.equal(final.session.login_state, 'needs_login');
});

test('用户关闭或崩溃的 context 下次同模式操作重启固定档案', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('first', 'one', 'refresh'));
  const identity = device.launches[0].options.identity;
  await device.launches[0].close();
  await worker.dispatch(request('after-close', 'one', 'refresh'));
  assert.equal(events.find(e => e.id === 'after-close')?.type, 'end');
  assert.equal(device.launches.length, 2);
  assert.equal(device.generated, 1);
  assert.deepEqual(device.launches[1].options.identity, identity);
});

test('活动请求 context 关闭后清理计数，其他档案继续工作', async t => {
  const { worker, device, events } = await fixture(t);
  const running = worker.dispatch(request('crashed', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  while (!events.some(e => e.id === 'crashed' && e.type === 'chunk')) await tick();
  await worker.dispatch(request('other-first', 'two'));
  await device.launches[0].close();
  await running;
  assert.equal(worker.profiles.get('one').active, 0);
  await worker.dispatch(request('recovered'));
  await worker.dispatch(request('other-still', 'two'));
  assert.equal(events.find(e => e.id === 'recovered' && e.type === 'end')?.type, 'end');
  assert.equal(events.find(e => e.id === 'other-still' && e.type === 'end')?.type, 'end');
  assert.equal(device.launches[1].closed, false);
});

test('下载保留真实状态和 MIME，并分块读取自有临时文件', async t => {
  const { worker, events } = await fixture(t);
  await worker.dispatch(request('media', 'one', 'download', { url: 'https://example.com/attachment' }));
  const start = events.find(e => e.type === 'start');
  assert.equal(start.status, 206);
  assert.equal(start.headers['content-type'], 'video/mp4');
  const chunks = events.filter(e => e.type === 'chunk');
  assert.ok(chunks.length > 1);
  assert.equal(Buffer.concat(chunks.map(e => Buffer.from(e.data_base64, 'base64'))).length, 196608);
});

test('下载取消清理本次临时文件并保留账号档案', async t => {
  const { worker, events, profileRoot } = await fixture(t);
  const before = new Set((await readdir(tmpdir())).filter(name => name.startsWith('gemini-download-')));
  const downloading = worker.dispatch(request('media-wait', 'one', 'download', { url: 'https://example.com/attachment-wait' }));
  while ((await readdir(tmpdir())).filter(name => name.startsWith('gemini-download-') && !before.has(name)).length === 0) await tick();
  await worker.dispatch(request('cancel-download', 'one', 'cancel', { target_id: 'media-wait' }));
  await downloading;
  assert.equal(events.find(e => e.id === 'media-wait' && e.type === 'error').code, 'cancelled');
  assert.deepEqual((await readdir(tmpdir())).filter(name => name.startsWith('gemini-download-') && !before.has(name)), []);
  assert.ok((await readFile(join(profileRoot, 'one', 'identity.json'), 'utf8')).length > 0);
});

test('取消附件时调用原生 Download.cancel，saveAs 不依赖关闭页面', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup'));
  const context = device.launches[0];
  const original = context.newPage.bind(context);
  const entered = deferred();
  const cancelled = deferred();
  const rescue = deferred();
  let actualCancelled = false;
  context.newPage = async () => {
    const page = await original();
    const listeners = new Map();
    page.on = (name, listener) => listeners.set(name, listener);
    page.goto = async url => {
      listeners.get('response')({ status: () => 200, allHeaders: async () => ({ 'content-type': 'video/mp4' }), url: () => url });
      listeners.get('download')({
        saveAs: async () => { entered.resolve(); await Promise.race([cancelled.promise, rescue.promise]); throw new Error('download cancelled'); },
        cancel: async () => { actualCancelled = true; cancelled.resolve(); },
        delete: async () => { await Promise.race([cancelled.promise, rescue.promise]); },
        url: () => url,
      });
      throw new Error('Download is starting');
    };
    return page;
  };
  const downloading = worker.dispatch(request('native-download', 'one', 'download', { url: 'https://example.com/download' }));
  await entered.promise;
  await worker.dispatch(request('cancel-native', 'one', 'cancel', { target_id: 'native-download' }));
  rescue.resolve();
  await downloading;
  assert.equal(actualCancelled, true);
  assert.equal(events.find(e => e.id === 'native-download')?.code, 'cancelled');
});

test('跨源重定向的普通媒体流使用最终浏览器响应 URL', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warmup'));
  const context = device.launches[0];
  const original = context.newPage.bind(context);
  const finalURL = 'https://cdn.example.net/final-media';
  context.newPage = async () => {
    const page = await original();
    page.goto = async () => { page.address = finalURL; return { status: () => 200, allHeaders: async () => ({ 'content-type': 'video/mp4' }), url: () => finalURL }; };
    const evaluate = page.evaluate.bind(page);
    page.evaluate = async (fn, arg) => { if (arg?.url && new URL(arg.url).origin !== new URL(page.address).origin) throw new Error('cross-origin fetch blocked'); return evaluate(fn, arg); };
    return page;
  };
  await worker.dispatch(request('redirected', 'one', 'download', { url: 'https://original.example.com/media' }));
  const start = events.find(e => e.id === 'redirected' && e.type === 'start');
  assert.equal(start?.url, finalURL);
  assert.equal(events.find(e => e.id === 'redirected' && e.type === 'end')?.type, 'end');
});

test('普通媒体导航的 HTML 404 仍流式返回状态和 MIME', async t => {
  const { worker, events } = await fixture(t);
  await worker.dispatch(request('missing-media', 'one', 'download', { url: 'https://example.com/missing' }));
  assert.equal(events[0].status, 404);
  assert.equal(events[0].headers['content-type'], 'text/html');
  assert.equal(events.at(-1).type, 'end');
});

test('真实页面函数只提取账号控件邮箱，多个账号拒绝认定身份', () => {
  const document = {
    querySelectorAll: () => [{ getAttribute: name => name === 'aria-label' ? 'Google Account: Actual (ACTUAL@example.com)' : null }, { getAttribute: name => name === 'aria-label' ? 'Email someone else other@example.com' : null }],
    scripts: [{ textContent: 'window.WIZ_global_data={"SNlM0e":"token:123"}' }],
  };
  const identity = runInNewContext(`(${inspectGeminiIdentity.toString()})()`, { document, location: { href: 'https://gemini.google.com/app' } });
  assert.equal(identity.email, 'actual@example.com');
  assert.equal(identity.token, 'token:123');
  document.querySelectorAll = () => ['one@example.com', 'two@example.com'].map(email => ({ getAttribute: name => name === 'data-email' ? email : null }));
  assert.equal(runInNewContext(`(${inspectGeminiIdentity.toString()})()`, { document, location: { href: 'https://gemini.google.com/app' } }).email, '');
});

test('页面 fetch 使用 credentials include 并按 ReadableStream 字节等待绑定', async () => {
  const events = [];
  let options;
  const response = new Response(new ReadableStream({ start(controller) { controller.enqueue(new Uint8Array([0, 255, 0xe2])); controller.enqueue(new Uint8Array([0x82, 0xac])); controller.close(); } }), { status: 201, headers: { 'content-type': 'application/octet-stream' } });
  const context = { AbortController, Uint8Array, atob, btoa, fetch: async (_, init) => { options = init; return response; }, __worker_stream: async event => { events.push(event); await tick(); } };
  await runInNewContext(`(${fetchInPage.toString()})({url:'https://example.com',method:'POST',headers:{},body_base64:'AP8='})`, context);
  assert.equal(options.credentials, 'include');
  assert.equal(Buffer.from(options.body).toString('hex'), '00ff');
  assert.deepEqual(events.map(e => e.type), ['start', 'chunk', 'chunk']);
  assert.equal(Buffer.concat(events.filter(e => e.type === 'chunk').map(e => Buffer.from(e.data_base64, 'base64'))).toString('hex'), '00ffe282ac');
});

test('请求剔除浏览器管理头并保留业务头，从 Gemini 发跨域上传', async t => {
  const { worker, device } = await fixture(t);
  await worker.dispatch(request('upload', 'one', 'request', { url: 'https://content-push.googleapis.com/upload', headers: { Cookie: 'bad', Host: 'bad', 'Content-Length': '5', 'User-Agent': 'bad', Origin: 'bad', Referer: 'bad', 'Sec-Fetch-Site': 'bad', 'X-Goog-Upload-Protocol': 'resumable', 'Content-Type': 'application/json' }, body_base64: 'AAE=' }));
  const page = device.launches[0].pages[0];
  assert.equal(page.address, 'https://gemini.google.com/app');
  assert.deepEqual(page.fetchArgs.headers, { 'X-Goog-Upload-Protocol': 'resumable', 'Content-Type': 'application/json' });
  assert.equal(page.fetchArgs.body_base64, 'AAE=');
});

test('shutdown 取消活动页面并关闭全部自有 context', async t => {
  const { worker, device, events } = await fixture(t);
  const pending = worker.dispatch(request('waiting', 'one', 'request', { url: 'https://gemini.google.com/wait' }));
  await worker.dispatch(request('other', 'two'));
  while (!events.some(e => e.id === 'waiting' && e.type === 'chunk')) await tick();
  await worker.dispatch(request('stop', 'one', 'shutdown'));
  await pending;
  assert.ok(device.launches.every(context => context.closed));
  assert.ok(events.some(e => e.id === 'stop' && e.type === 'end'));
});

test('JSON Lines 输出等待 Writable 完成且 IPC 分片保留 id/account_id', async () => {
  let finish;
  let wire = '';
  const output = new Writable({ write(chunk, _, callback) { wire += chunk; finish = callback; } });
  let done = false;
  const writing = writeJSONLine(output, { id: '中', account_id: 7, type: 'end' }).then(() => { done = true; });
  await tick();
  assert.equal(done, false);
  finish();
  await writing;
  assert.deepEqual(JSON.parse(wire), { id: '中', account_id: 7, type: 'end' });
  const input = new PassThrough();
  const events = [];
  const ipcOutput = new Writable({ write(chunk, _, callback) { events.push(JSON.parse(chunk.toString())); callback(); } });
  const running = runIPC({ input, output: ipcOutput, workerOptions: { browserFactory: browserDevice().factory } });
  const payload = Buffer.from(JSON.stringify(request('分片', 'one', 'unknown')) + '\n');
  input.write(payload.subarray(0, 20));
  input.end(payload.subarray(20));
  await running;
  assert.equal(events[0].type, 'ready');
  assert.equal(events[0].id, '');
  assert.equal(events[1].id, '分片');
  assert.equal(events[1].account_id, 1);
  assert.equal(events[1].type, 'error');
});

test('IPC 空对象或 null 请求返回协议错误且继续读取后续 shutdown', async () => {
  const input = new PassThrough();
  const events = [];
  const output = new Writable({ write(chunk, _, callback) { events.push(JSON.parse(chunk.toString())); callback(); } });
  const running = runIPC({ input, output });
  input.end('null\n{}\n{"id":"done","account_id":9,"op":"shutdown"}\n');
  await running;
  assert.deepEqual(events.map(e => e.type), ['ready', 'error', 'error', 'end']);
  assert.equal(events.at(-1).id, 'done');
});

test('实际 Node 子进程无需浏览器依赖即可 ready/shutdown，stdout 仅协议', async () => {
  const child = spawn(process.execPath, [fileURLToPath(new URL('../worker.mjs', import.meta.url))], { stdio: ['pipe', 'pipe', 'pipe'] });
  let stdout = '';
  let stderr = '';
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', chunk => { stdout += chunk; if (stdout.includes('"ready"') && !stdout.includes('"end"')) child.stdin.write('{"id":"shutdown-process","account_id":42,"op":"shutdown"}\n'); });
  child.stderr.on('data', chunk => { stderr += chunk; });
  const code = await new Promise(resolve => child.on('exit', resolve));
  assert.equal(code, 0, stderr);
  assert.equal(stderr, '');
  const events = stdout.trim().split('\n').map(line => JSON.parse(line));
  assert.deepEqual(events.map(e => e.type), ['ready', 'end']);
  assert.equal(events[1].id, 'shutdown-process');
  assert.equal(events[1].account_id, 42);
});

test('页面 fetch 在跨环境响应头迭代器不可迭代时保留真实状态和头', async () => {
  const events = [];
  const response = new Response(null, { status: 206, headers: { 'content-type': 'video/mp4', 'x-goog-upload-status': 'final' } });
  const iterator = response.headers.entries();
  // 真实 Camoufox 隔离环境返回的迭代器不提供可用的跨环境 Symbol.iterator。
  Object.defineProperty(response.headers, 'entries', { value: () => ({ next: () => iterator.next() }) });
  const context = { AbortController, Uint8Array, atob, btoa, fetch: async () => response, __worker_stream: async event => events.push(event) };
  await runInNewContext(`(${fetchInPage.toString()})({url:'https://example.com',method:'GET',headers:{}})`, context);
  assert.equal(events[0].status, 206);
  assert.deepEqual(JSON.parse(JSON.stringify(events[0].headers)), { 'content-type': 'video/mp4', 'x-goog-upload-status': 'final' });
});

// 故障目标：quota 被当作未知操作、切换 headed context，或覆盖用户登录页面。
test('quota 返回本账号 session，沿用 headed context 并关闭独立用量页', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('user-login', 'one', 'login'));
  const profile = worker.profiles.get('one');
  const userPage = profile.page;
  const context = device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await newPage();
    const evaluate = page.evaluate.bind(page);
    page.waitForSelector = async () => null;
    page.evaluate = async (fn, arg) => fn.name === 'inspectQuotaPanel' ? { found: true, current: { text: '当前用量\n已使用 0%\n重置时间：12:51' }, weekly: { text: '每周限额\n已使用 4%\n重置时间：10月30日09:51' }, checked_at: 1792807840, time_zone: 'Asia/Shanghai' } : fn.name === 'fetchQuotaRPCInPage' ? null : evaluate(fn, arg);
    return page;
  };
  await worker.dispatch(request('quota-own', 'one', 'quota'));
  const final = events.find(event => event.id === 'quota-own');
  assert.equal(final.type, 'end');
  assert.equal(final.account_id, 1);
  assert.equal(final.session.email, 'actual@example.com');
  assert.equal(final.session.login_state, 'ready');
  assert.equal(final.quota.used_percent, 0);
  assert.equal(final.quota.weekly_used_percent, 4);
  assert.equal(final.quota.five_hour_limited, false);
  assert.equal(device.launches.length, 1);
  assert.equal(profile.page, userPage);
  assert.equal(userPage.closed, false);
  assert.ok(context.pages.filter(page => page !== userPage).every(page => page.closed));
});

test('quota 等实际卡片加载后读取，不把先出现的 Angular 空容器当成最终样本', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('warm-quota', 'one', 'refresh'));
  const context = device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await newPage();
    const evaluate = page.evaluate.bind(page);
    let cardsVisible = false;
    page.waitForSelector = async selector => { if (/current-usage|gxu-currently/.test(selector)) cardsVisible = true; };
    page.evaluate = async (fn, arg) => fn.name === 'inspectQuotaPanel' ? { found: true, current: cardsVisible ? { text: '已使用 0%\n重置时间：14:03' } : null, weekly: null, checked_at: 1792807840, time_zone: 'Asia/Shanghai' } : fn.name === 'fetchQuotaRPCInPage' ? null : evaluate(fn, arg);
    return page;
  };
  await worker.dispatch(request('loading-quota', 'one', 'quota'));
  const final = events.find(event => event.id === 'loading-quota');
  assert.equal(final.type, 'end');
  assert.equal(final.quota.used_percent, 0);
  assert.equal(final.quota.five_hour_limited, false);
});

async function quotaFixture(t, observation = { found: false, checked_at: 1792807840 }) {
  const f = await fixture(t);
  await f.worker.dispatch(request('keep-login', 'one', 'login'));
  const context = f.device.launches[0];
  const newPage = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await newPage();
    const evaluate = page.evaluate.bind(page);
    page.waitForSelector = async () => null;
    page.evaluate = async (fn, arg) => fn.name === 'inspectQuotaPanel' ? observation : fn.name === 'fetchQuotaRPCInPage' ? null : evaluate(fn, arg);
    return page;
  };
  return { ...f, context };
}

// 精确响应采用独立字面值；故障目标是完整RPC仍被迫加载usage或热请求重新加载app。
const fastQuotaWire = `)]}'\n\n[["wrb.fr","jSf9Qc","[2,[[2400,0,1,[[1792821818,0]]],[47787,0.01233471,2,[[1793372618,0]]]],false]",null,null,null,"generic"]]\n`;
async function fastQuotaFixture(t) {
  const f = await fixture(t);
  const launch = f.device.factory.launch.bind(f.device.factory);
  const navigations = [];
  let rpcCalls = 0;
  const state = { wire: fastQuotaWire, afterRPC: null, identityOverride: null, failLightNavigation: false };
  f.device.factory.launch = async options => {
    const context = await launch(options);
    const newPage = context.newPage.bind(context);
    context.newPage = async () => {
      const page = await newPage();
      const goto = page.goto.bind(page);
      const evaluate = page.evaluate.bind(page);
      page.goto = async url => {
        navigations.push(url);
        if (url.endsWith('/robots.txt') && state.failLightNavigation) throw new Error('Light page unavailable');
        return goto(url);
      };
      page.waitForSelector = async () => { throw new Error('Fast quota must not require usage UI'); };
      page.evaluate = async (fn, arg) => {
        if (fn.name === 'fetchQuotaRPCInPage') { rpcCalls++; await state.afterRPC?.(context); return state.wire; }
        if (fn.name === 'inspectGeminiIdentity' && state.identityOverride) return state.identityOverride;
        return evaluate(fn, arg);
      };
      return page;
    };
    return context;
  };
  return { ...f, navigations, state, get rpcCalls() { return rpcCalls; } };
}

test('完整真实用量RPC无需usage面板就返回精确两窗口与已验证身份', async t => {
  const f = await fastQuotaFixture(t);
  await f.worker.dispatch(request('fast-cold', 'one', 'quota'));
  const result = f.events.find(event => event.id === 'fast-cold');
  assert.equal(result.type, 'end');
  assert.equal(result.session.login_state, 'ready');
  assert.equal(result.quota.used_percent, 0);
  assert.equal(result.quota.weekly_used_percent, 1.233471);
  assert.equal(result.quota.reset_at, 1792821818);
  assert.equal(result.quota.weekly_reset_at, 1793372618);
  assert.equal(result.quota.error, '');
  assert.equal(f.rpcCalls, 1);
  assert.ok(!f.navigations.some(url => url.endsWith('/usage')));
  assert.equal(f.worker.profiles.get('one').active, 0);
  assert.ok(f.device.launches[0].pages.every(page => page.closed));
});

test('认证Cookie一致的热查询新读RPC且不重新下载app或切换用户窗口', async t => {
  const f = await fastQuotaFixture(t);
  await f.worker.dispatch(request('fast-login', 'one', 'login'));
  const userPage = f.worker.profiles.get('one').page;
  const priorAppLoads = f.navigations.filter(url => url.endsWith('/app')).length;
  await f.worker.dispatch(request('fast-warm-1', 'one', 'quota'));
  f.state.wire = fastQuotaWire.replace('0.01233471', '1');
  await f.worker.dispatch(request('fast-warm-2', 'one', 'quota'));
  const result = f.events.find(event => event.id === 'fast-warm-2');
  assert.equal(result.type, 'end');
  assert.equal(result.quota.weekly_used_percent, 100);
  assert.equal(result.quota.weekly_limited, true);
  assert.equal(f.rpcCalls, 2);
  assert.equal(f.navigations.filter(url => url.endsWith('/app')).length, priorAppLoads);
  assert.ok(!f.navigations.some(url => url.endsWith('/usage')));
  assert.equal(userPage.closed, false);
  assert.equal(f.device.launches.length, 1);
});

test('热查询认证Cookie变化必须重新核对身份，不沿用旧账号额度', async t => {
  const f = await fastQuotaFixture(t);
  await f.worker.dispatch(request('fast-initial', 'one', 'quota'));
  const priorCalls = f.rpcCalls;
  f.device.launches[0].cookieList[0].value = 'switched-account-cookie';
  f.state.identityOverride = { email: 'other@example.com', emails: ['other@example.com'], token: 'other-token', url: 'https://gemini.google.com/app' };
  await f.worker.dispatch(request('fast-wrong-owner', 'one', 'quota'));
  const result = f.events.find(event => event.id === 'fast-wrong-owner');
  assert.equal(result.type, 'error');
  assert.equal(result.code, 'account_mismatch');
  assert.equal(result.quota, undefined);
  assert.equal(f.rpcCalls, priorCalls);
});

test('精确RPC读取途中认证Cookie变化不发布旧身份的用量', async t => {
  const f = await fastQuotaFixture(t);
  f.state.afterRPC = async context => { context.cookieList[0].value = 'changed-during-rpc'; };
  await f.worker.dispatch(request('fast-rpc-cookie-change', 'one', 'quota'));
  const result = f.events.find(event => event.id === 'fast-rpc-cookie-change');
  assert.equal(result.type, 'error');
  assert.equal(result.code, 'login_required');
  assert.equal(result.quota, undefined);
  assert.equal(f.worker.profiles.get('one').active, 0);
});

test('热查询轻页面失败回退重新确认app，不丢失额度或保留活动任务', async t => {
  const f = await fastQuotaFixture(t);
  await f.worker.dispatch(request('fast-before-navigation-failure', 'one', 'quota'));
  const appLoads = f.navigations.filter(url => url.endsWith('/app')).length;
  f.state.failLightNavigation = true;
  await f.worker.dispatch(request('fast-navigation-fallback', 'one', 'quota'));
  const result = f.events.find(event => event.id === 'fast-navigation-fallback');
  assert.equal(result.type, 'end');
  assert.equal(result.session.login_state, 'ready');
  assert.equal(result.quota.used_percent, 0);
  assert.equal(result.quota.weekly_used_percent, 1.233471);
  assert.equal(f.navigations.filter(url => url.endsWith('/app')).length, appLoads + 1);
  assert.equal(f.worker.profiles.get('one').active, 0);
  assert.equal(f.worker.tasks.size, 0);
});

test('quota 未找到面板返回未知额度与 ready session，保留登录页面', async t => {
  const { worker, context, events } = await quotaFixture(t);
  const loginPage = worker.profiles.get('one').page;
  await worker.dispatch(request('missing-quota', 'one', 'quota'));
  const final = events.find(event => event.id === 'missing-quota');
  assert.equal(final.type, 'end');
  assert.equal(final.session.login_state, 'ready');
  assert.equal(final.quota.used_percent, null);
  assert.equal(final.quota.five_hour_limited, null);
  assert.equal(final.quota.weekly_limited, null);
  assert.ok(final.quota.error);
  assert.equal(loginPage.closed, false);
  assert.equal(context.closed, false);
});

test('quota 面板读取异常保持未知，不伪装成登录失效', async t => {
  const { worker, context, events } = await quotaFixture(t);
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); page.waitForSelector = async () => { throw new Error('locator timeout'); }; return page; };
  await worker.dispatch(request('bad-panel', 'one', 'quota'));
  const final = events.find(event => event.id === 'bad-panel');
  assert.equal(final.type, 'end');
  assert.equal(final.session.login_state, 'ready');
  assert.equal(final.quota.five_hour_limited, null);
  assert.ok(final.quota.error);
});

test('quota 绑定账号不匹配及真实 token 缺失仍返回身份错误', async t => {
  const { worker, context, events } = await quotaFixture(t);
  await worker.dispatch(request('quota-mismatch', 'one', 'quota', { expected_email: 'other@example.com' }));
  assert.equal(events.find(event => event.id === 'quota-mismatch').code, 'account_mismatch');
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); page.identity.token = ''; return page; };
  await worker.dispatch(request('quota-no-token', 'one', 'quota'));
  const final = events.find(event => event.id === 'quota-no-token');
  assert.equal(final.type, 'error');
  assert.equal(final.code, 'login_required');
  assert.equal(final.session.login_state, 'needs_login');
  assert.equal(worker.profiles.get('one').page.closed, false);
});

test('quota 读取期间身份 Cookie 变化不把原账号额度发给新身份', async t => {
  const { worker, context, events } = await quotaFixture(t);
  const newPage = context.newPage.bind(context);
  context.newPage = async () => {
    const page = await newPage();
    const goto = page.goto.bind(page);
    page.goto = async url => { const result = await goto(url); if (url.endsWith('/usage')) context.cookieList[0].value = 'changed-during-quota'; return result; };
    return page;
  };
  await worker.dispatch(request('quota-switched', 'one', 'quota'));
  const final = events.find(event => event.id === 'quota-switched');
  assert.equal(final.type, 'error');
  assert.equal(final.code, 'login_required');
  assert.equal(final.session.login_state, 'needs_login');
  assert.equal(final.quota, undefined);
});

test('quota 取消按 account_id 归属，中断用量等待且不关闭用户登录页', async t => {
  const { worker, context, events } = await quotaFixture(t);
  const entered = deferred();
  const release = deferred();
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); page.waitForSelector = async () => { entered.resolve(); await release.promise; }; return page; };
  const running = worker.dispatch(request('quota-wait', 'one', 'quota'));
  await entered.promise;
  await worker.dispatch(request('wrong-cancel', 'two', 'cancel', { target_id: 'quota-wait' }));
  assert.ok(worker.tasks.has('quota-wait'));
  await worker.dispatch(request('right-cancel', 'one', 'cancel', { target_id: 'quota-wait' }));
  await running;
  release.resolve();
  assert.equal(events.find(event => event.id === 'quota-wait').code, 'cancelled');
  assert.equal(events.filter(event => event.id === 'quota-wait').length, 1);
  assert.equal(worker.profiles.get('one').page.closed, false);
  assert.ok(context.pages.filter(page => page !== worker.profiles.get('one').page).every(page => page.closed));
  assert.equal(worker.profiles.get('one').active, 0);
});

test('quota 超时释放临时页面、任务和计数', async t => {
  const { worker, context, events } = await quotaFixture(t);
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { const page = await newPage(); page.waitForSelector = async () => new Promise(() => {}); return page; };
  await worker.dispatch(request('quota-timeout', 'one', 'quota', { timeout_ms: 30 }));
  assert.equal(events.find(event => event.id === 'quota-timeout').code, 'timeout');
  assert.equal(worker.tasks.size, 0);
  assert.equal(worker.profiles.get('one').active, 0);
  assert.equal(worker.profiles.get('one').page.closed, false);
  assert.ok(context.pages.filter(page => page !== worker.profiles.get('one').page).every(page => page.closed));
});

// 仅压缩外部时间边界；断言真实调度结果和资源归属，不靠 SDK 自身超时行为。
function shortQuotaDeadline(t) {
  const original = globalThis.setTimeout;
  const delays = [];
  t.mock.method(globalThis, 'setTimeout', (callback, delay, ...args) => {
    delays.push(delay);
    return original(callback, Math.min(delay, 30), ...args);
  });
  return {
    delays,
    async observe(pending) {
      let timer;
      try { return await Promise.race([pending.then(() => 'finished'), new Promise(resolve => { timer = original(() => resolve('still-running'), 100); })]); }
      finally { clearTimeout(timer); }
    },
  };
}

test('quota 默认总上限覆盖冷启动 launch，省略或零超时均退出并关闭迟到 context', async t => {
  const deadline = shortQuotaDeadline(t);
  for (const timeout of [undefined, 0]) {
    const { worker, device, events } = await fixture(t);
    const release = deferred();
    const launch = device.factory.launch.bind(device.factory);
    let sdkTimeout;
    device.factory.launch = async options => { sdkTimeout = options.timeout; await release.promise; return launch(options); };
    const req = request(`cold-bound-${timeout}`, 'one', 'quota');
    if (timeout === undefined) delete req.timeout_ms;
    const running = worker.dispatch(req);
    const outcome = await deadline.observe(running);
    release.resolve();
    await running;
    await tick();
    assert.equal(outcome, 'finished');
    assert.equal(events.find(event => event.id === req.id).code, 'timeout');
    assert.ok(Number.isFinite(sdkTimeout) && sdkTimeout > 0);
    assert.ok(deadline.delays.some(delay => Number.isFinite(delay) && delay > 0));
    assert.equal(worker.tasks.size, 0);
    assert.equal(worker.profiles.get('one').active, 0);
    assert.equal(worker.profiles.get('one').context, null);
    assert.equal(device.launches[0].closed, true);
  }
});

test('quota 默认总上限覆盖指纹生成等待，取消后不会继续启动浏览器', async t => {
  const { worker, device, events } = await fixture(t);
  const deadline = shortQuotaDeadline(t);
  const release = deferred();
  const generate = device.factory.generate.bind(device.factory);
  device.factory.generate = async options => { await release.promise; return generate(options); };
  const running = worker.dispatch(request('identity-bound', 'one', 'quota'));
  const outcome = await deadline.observe(running);
  release.resolve();
  await running;
  await tick();
  assert.equal(outcome, 'finished');
  assert.equal(events.find(event => event.id === 'identity-bound').code, 'timeout');
  assert.equal(device.launches.length, 0);
  assert.equal(worker.tasks.size, 0);
  assert.equal(worker.profiles.get('one').active, 0);
});

test('quota 默认总上限中断初始 app 导航，省略或零超时均保留原 headed 用户页', async t => {
  const deadline = shortQuotaDeadline(t);
  for (const timeout of [undefined, 0]) {
    const { worker, context, events } = await quotaFixture(t);
    const profile = worker.profiles.get('one');
    const userPage = profile.page;
    const newPage = context.newPage.bind(context);
    const release = deferred();
    let sdkTimeout;
    context.newPage = async () => {
      const page = await newPage();
      const goto = page.goto.bind(page);
      page.goto = async (url, options) => { if (url.endsWith('/app')) { sdkTimeout = options.timeout; await release.promise; } return goto(url, options); };
      return page;
    };
    const req = request(`app-bound-${timeout}`, 'one', 'quota');
    if (timeout === undefined) delete req.timeout_ms;
    const running = worker.dispatch(req);
    const outcome = await deadline.observe(running);
    release.resolve();
    await running;
    assert.equal(outcome, 'finished');
    assert.equal(events.find(event => event.id === req.id).code, 'timeout');
    assert.ok(Number.isFinite(sdkTimeout) && sdkTimeout > 0);
    assert.equal(profile.page, userPage);
    assert.equal(userPage.closed, false);
    assert.equal(context.closed, false);
    assert.equal(context.options.headless, false);
    assert.equal(worker.tasks.size, 0);
    assert.equal(profile.active, 0);
    assert.ok(context.pages.filter(page => page !== userPage).every(page => page.closed));
  }
});

test('quota 默认总上限中断新建页面，迟到临时页面被关闭且用户页保留', async t => {
  const { worker, context, events } = await quotaFixture(t);
  const deadline = shortQuotaDeadline(t);
  const userPage = worker.profiles.get('one').page;
  const release = deferred();
  const newPage = context.newPage.bind(context);
  context.newPage = async () => { await release.promise; return newPage(); };
  const running = worker.dispatch(request('page-bound', 'one', 'quota'));
  const outcome = await deadline.observe(running);
  release.resolve();
  await running;
  await tick();
  assert.equal(outcome, 'finished');
  assert.equal(events.find(event => event.id === 'page-bound').code, 'timeout');
  assert.equal(worker.tasks.size, 0);
  assert.equal(worker.profiles.get('one').active, 0);
  assert.equal(userPage.closed, false);
  assert.ok(context.pages.filter(page => page !== userPage).every(page => page.closed));
});

test('取消持锁 quota close 后同档案后继不能复用仍在关闭的 context，其他档案继续', async t => {
  const { worker, device, events } = await fixture(t);
  await worker.dispatch(request('close-owner-warm', 'one', 'refresh'));
  const closing = device.launches[0];
  const entered = deferred();
  const release = deferred();
  const close = closing.close.bind(closing);
  const newPage = closing.newPage.bind(closing);
  let closeSettled = false;
  let overlap = false;
  closing.close = async () => { entered.resolve(); await release.promise; await close(); closeSettled = true; };
  closing.newPage = async () => { if (!closeSettled) overlap = true; return newPage(); };
  const running = worker.dispatch(request('close-owner-quota', 'one', 'quota', { proxy_url: 'http://127.0.0.1:8080', timeout_ms: 1000 }));
  await entered.promise;
  await worker.dispatch(request('close-owner-cancel', 'one', 'cancel', { target_id: 'close-owner-quota' }));
  await running;
  const successor = worker.dispatch(request('close-owner-successor', 'one', 'refresh'));
  await worker.dispatch(request('close-other-profile', 'two', 'refresh'));
  await tick();
  const finishedBeforeClose = events.some(event => event.id === 'close-owner-successor');
  const overlappedBeforeClose = overlap;
  release.resolve();
  await successor;
  await worker.profiles.get('one').lock;
  assert.equal(events.find(event => event.id === 'close-owner-quota').code, 'cancelled');
  assert.equal(events.find(event => event.id === 'close-other-profile').type, 'end');
  assert.equal(overlappedBeforeClose, false);
  assert.equal(finishedBeforeClose, false);
  assert.equal(events.find(event => event.id === 'close-owner-successor').type, 'end');
  assert.ok(worker.profiles.get('one').context && worker.profiles.get('one').context !== closing);
  assert.equal(closing.closed, true);
  assert.equal(worker.profiles.get('one').active, 0);
});

test('取消持锁 quota launch 后同目录后继等待原启动及迟到 context 清理都结束', async t => {
  const { worker, device, events, profileRoot } = await fixture(t);
  const entered = deferred();
  const releaseLaunch = deferred();
  const cleanupEntered = deferred();
  const releaseCleanup = deferred();
  const launch = device.factory.launch.bind(device.factory);
  let firstStarted = false;
  let nativeSettled = false;
  let cleanupSettled = false;
  let overlappingLaunch = false;
  let lateContext;
  device.factory.launch = async options => {
    if (options.userDataDir !== join(profileRoot, 'one', 'browser')) return launch(options);
    if (!firstStarted) {
      firstStarted = true; entered.resolve();
      await releaseLaunch.promise;
      lateContext = await launch(options);
      nativeSettled = true;
      const close = lateContext.close.bind(lateContext);
      lateContext.close = async () => { cleanupEntered.resolve(); await releaseCleanup.promise; await close(); cleanupSettled = true; };
      return lateContext;
    }
    if (!nativeSettled || !cleanupSettled) overlappingLaunch = true;
    return launch(options);
  };
  const running = worker.dispatch(request('launch-owner-quota', 'one', 'quota', { timeout_ms: 1000 }));
  await entered.promise;
  await worker.dispatch(request('launch-owner-cancel', 'one', 'cancel', { target_id: 'launch-owner-quota' }));
  await running;
  const successor = worker.dispatch(request('launch-owner-successor', 'one', 'refresh'));
  await worker.dispatch(request('launch-other-profile', 'two', 'refresh'));
  await tick();
  const finishedBeforeLaunch = events.some(event => event.id === 'launch-owner-successor');
  const overlappedBeforeLaunch = overlappingLaunch;
  releaseLaunch.resolve();
  await cleanupEntered.promise;
  await tick();
  const finishedBeforeCleanup = events.some(event => event.id === 'launch-owner-successor');
  const overlappedBeforeCleanup = overlappingLaunch;
  releaseCleanup.resolve();
  await successor;
  await worker.profiles.get('one').lock;
  assert.equal(events.find(event => event.id === 'launch-owner-quota').code, 'cancelled');
  assert.equal(events.find(event => event.id === 'launch-other-profile').type, 'end');
  assert.equal(overlappedBeforeLaunch, false);
  assert.equal(finishedBeforeLaunch, false);
  assert.equal(overlappedBeforeCleanup, false);
  assert.equal(finishedBeforeCleanup, false);
  assert.equal(events.find(event => event.id === 'launch-owner-successor').type, 'end');
  assert.equal(lateContext.closed, true);
  assert.ok(worker.profiles.get('one').context !== lateContext);
  assert.equal(worker.profiles.get('one').active, 0);
});

test('quota 排队时取消保持先前 owner 与后继顺序，不影响其他档案', async t => {
  const { worker, device, events } = await fixture(t);
  const profile = worker.profile('one');
  const entered = deferred();
  const release = deferred();
  let ownerSettled = false;
  let successorEnteredEarly = false;
  const owner = worker.locked(profile, async () => { entered.resolve(); await release.promise; ownerSettled = true; });
  await entered.promise;
  const launch = device.factory.launch.bind(device.factory);
  device.factory.launch = async options => { if (options.userDataDir === join(profile.directory, 'browser') && !ownerSettled) successorEnteredEarly = true; return launch(options); };
  const waiting = worker.dispatch(request('queued-owner-quota', 'one', 'quota', { timeout_ms: 1000 }));
  const successor = worker.dispatch(request('queued-owner-successor', 'one', 'refresh'));
  await worker.dispatch(request('queued-owner-cancel', 'one', 'cancel', { target_id: 'queued-owner-quota' }));
  await waiting;
  await worker.dispatch(request('queued-other-profile', 'two', 'refresh'));
  await tick();
  const finishedBeforeOwner = events.some(event => event.id === 'queued-owner-successor');
  release.resolve();
  await owner;
  await successor;
  assert.equal(events.find(event => event.id === 'queued-owner-quota').code, 'cancelled');
  assert.equal(events.find(event => event.id === 'queued-other-profile').type, 'end');
  assert.equal(successorEnteredEarly, false);
  assert.equal(finishedBeforeOwner, false);
  assert.equal(events.find(event => event.id === 'queued-owner-successor').type, 'end');
  assert.equal(profile.active, 0);
});
