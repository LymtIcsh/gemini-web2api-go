import { mkdir, readFile, writeFile, mkdtemp, rm } from 'node:fs/promises';
import { createReadStream } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createInterface } from 'node:readline';
import { randomUUID, createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { inspectQuotaPanel, parseQuotaPanel, decodeQuotaRPC, fetchQuotaRPCInPage, quotaRPCObservation } from './quota.mjs';
export { inspectQuotaPanel, parseQuotaPanel, decodeQuotaRPC, fetchQuotaRPCInPage } from './quota.mjs';

const GEMINI = 'https://gemini.google.com/app';
const QUOTA_RPC_PAGE = 'https://gemini.google.com/robots.txt';
const QUOTA_TIMEOUT_MS = 45000;
const OPS = new Set(['login', 'confirm-login', 'refresh', 'quota', 'open', 'close', 'request', 'download', 'cancel', 'shutdown']);
const fail = (code, message, session) => Object.assign(new Error(message), { code, session });
const googleService = url => /(^|\.)(google\.com|googleapis\.com|googleusercontent\.com)$/.test(new URL(url).hostname);
// 仅认证身份 Cookie 参与缓存归属；SIDCC 等轮转票据不会改变账号身份缓存。
function authCookieKey(cookies) {
  return JSON.stringify(cookies.filter(cookie => /^(SAPISID|APISID|SID|HSID|SSID|__Secure-[13]P(?:SID|APISID))$/.test(cookie.name)).map(cookie => [cookie.name, cookie.domain, cookie.path, cookie.value]).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b))));
}
function checkCancelled(task) {
  if (task.controller.signal.aborted) throw fail(task.cancelCode, '请求已取消');
}

// 共享用户页面只读操作用 AbortSignal 中断等待，不关闭其他操作拥有的页面。
async function waitTask(task, promise, dispose) {
  // 启动资源可能在取消之后才返回；只关闭这次任务创建的迟到资源。
  if (dispose) Promise.resolve(promise).then(resource => {
    if (task.controller.signal.aborted) void Promise.resolve().then(() => dispose(resource)).catch(() => {});
  }, () => {});
  checkCancelled(task);
  let abort;
  const cancelled = new Promise((_, reject) => { abort = () => reject(fail(task.cancelCode, '请求已取消')); task.controller.signal.addEventListener('abort', abort, { once: true }); });
  try { return await Promise.race([promise, cancelled]); }
  finally { task.controller.signal.removeEventListener('abort', abort); }
}

// 只保存已生成的浏览器身份变量，不把进程环境或代理凭据写入身份文件。
export const camoufoxFactory = {
  async generate({ os }) {
    const { launchOptions } = await import('camoufox');
    const options = await launchOptions({ os, headless: true });
    return { executablePath: options.executablePath, args: options.args || [], firefoxUserPrefs: options.firefoxUserPrefs || {}, env: Object.fromEntries(Object.entries(options.env || {}).filter(([key]) => key.startsWith('CAMOU_CONFIG_'))) };
  },
  async launch({ identity, userDataDir, headless, proxy, timeout }) {
    const { firefox } = await import('playwright-core');
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('CAMOU_CONFIG_')));
    return firefox.launchPersistentContext(userDataDir, { ...identity, env: { ...env, ...identity.env }, headless, proxy, timeout, acceptDownloads: true });
  },
};

export function browserHeaders(headers = {}) {
  return Object.fromEntries(Object.entries(headers).filter(([name]) => !/^(cookie2?|host|content-length|user-agent|origin|referer|accept-encoding|connection|keep-alive|transfer-encoding|upgrade|te|trailer|proxy-.*|sec-.*)$/i.test(name)));
}

// 此函数在真实页面中执行，网络请求和 Cookie 均由浏览器处理。
export async function fetchInPage({ url, method, headers, body_base64 }) {
  const controller = new AbortController();
  globalThis.__worker_abort = controller;
  const body = body_base64 === undefined ? undefined : Uint8Array.from(atob(body_base64), char => char.charCodeAt(0));
  const response = await fetch(url, { method, headers, body, credentials: 'include', signal: controller.signal });
  // Firefox 隔离脚本中的响应头迭代器可能跨环境，使用原生回调读取。
  const responseHeaders = {};
  response.headers.forEach((value, name) => { responseHeaders[name] = value; });
  await globalThis.__worker_stream({ type: 'start', status: response.status, headers: responseHeaders, url: response.url });
  if (!response.body) return;
  const reader = response.body.getReader();
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      let binary = '';
      // 复制每个流分块到当前脚本环境，避免直接访问 Firefox Xray TypedArray。
      for (const byte of new Uint8Array(value)) binary += String.fromCharCode(byte);
      await globalThis.__worker_stream({ type: 'chunk', data_base64: btoa(binary) });
    }
  } finally {
    reader.releaseLock();
    delete globalThis.__worker_abort;
  }
}

// 邮箱只取 Google 账号控件，页面普通文本和调用方标签不是登录证据。
export function inspectGeminiIdentity() {
  const emails = new Set();
  const controls = document.querySelectorAll('[data-email], [data-profile-identifier], a[aria-label], button[aria-label], [role="button"][aria-label]');
  for (const control of controls) {
    const label = control.getAttribute('aria-label') || '';
    const accountControl = /google\s*(account|账号|帳戶|账户|アカウント|계정|konto|compte|cuenta)/i.test(label);
    const explicit = control.getAttribute('data-email') || control.getAttribute('data-profile-identifier');
    if (!explicit && !accountControl) continue;
    for (const match of (explicit || label).matchAll(/[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Z0-9](?:[A-Z0-9.-]*[A-Z0-9])?\.[A-Z]{2,}/gi)) emails.add(match[0].toLowerCase());
  }
  let token = globalThis.WIZ_global_data?.SNlM0e || '';
  if (!token) {
    for (const script of document.scripts) {
      const match = script.textContent.match(/"SNlM0e"\s*:\s*("(?:[^"\\]|\\.)*")/);
      if (match) { token = JSON.parse(match[1]); break; }
    }
  }
  return { email: emails.size === 1 ? [...emails][0] : '', emails: [...emails], token, url: location.href };
}

function proxyOptions(proxyURL) {
  if (!proxyURL) return undefined;
  const proxy = new URL(proxyURL);
  const options = { server: `${proxy.protocol}//${proxy.host}` };
  if (proxy.username) options.username = decodeURIComponent(proxy.username);
  if (proxy.password) options.password = decodeURIComponent(proxy.password);
  return options;
}

export class BrowserWorker {
  constructor({ profileRoot = process.env.BROWSER_PROFILE_ROOT || join(process.cwd(), 'data', 'browser-profiles'), browserFactory = camoufoxFactory, maxBrowsers = Number(process.env.BROWSER_MAX_BROWSERS || 1), idleTimeoutMs = Number(process.env.BROWSER_IDLE_TIMEOUT_MS || 60000), emit = async event => writeJSONLine(process.stdout, event) } = {}) {
    if (!Number.isSafeInteger(maxBrowsers) || maxBrowsers < 1) throw new RangeError('BROWSER_MAX_BROWSERS 必须是大于0的整数');
    if (!Number.isSafeInteger(idleTimeoutMs) || idleTimeoutMs < 0 || idleTimeoutMs > 2147483647) throw new RangeError('BROWSER_IDLE_TIMEOUT_MS 必须是0到2147483647的整数');
    this.profileRoot = resolve(profileRoot);
    this.factory = browserFactory;
    this.emit = emit;
    this.profiles = new Map();
    this.tasks = new Map();
    this.maxBrowsers = maxBrowsers;
    this.idleTimeoutMs = idleTimeoutMs;
    this.pool = {};
    this.capacityWaiters = new Set();
    this.stopping = false;
  }

  async locked(profile, work) {
    const before = profile.lock || Promise.resolve();
    let release;
    profile.lock = new Promise(resolve => { release = resolve; });
    await before;
    try { return await work(); } finally { release(); }
  }

  profile(id) {
    if (!id || !/^[a-zA-Z0-9_-]+$/.test(id)) throw fail('invalid_request', 'profile_id 必须是独立档案名称');
    if (!this.profiles.has(id)) this.profiles.set(id, { id, directory: join(this.profileRoot, id), active: 0, context: null, page: null, mode: true, proxyURL: '' });
    return this.profiles.get(id);
  }

  wakeCapacity() {
    for (const resolve of this.capacityWaiters) resolve();
    this.capacityWaiters.clear();
  }

  canRecycle(profile) {
    return profile.context && !profile.active && !profile.acquiring && !profile.recycling && !profile.reserved && (!profile.page || profile.page.isClosed?.());
  }

  // 原生关闭期间仍占槽位；不同档案的原生操作继续使用各自锁。
  async closeProfile(profile) {
    clearTimeout(profile.idleTimer);
    const context = profile.context;
    profile.recycling = true;
    try {
      await context?.close();
      if (profile.context === context) {
        profile.context = null;
        profile.page = null;
        profile.identity = null;
        profile.identityCookieKey = null;
      }
    } finally {
      profile.recycling = false;
      this.wakeCapacity();
    }
  }

  scheduleIdle(profile) {
    clearTimeout(profile.idleTimer);
    if (this.stopping || !this.idleTimeoutMs || !this.canRecycle(profile)) return;
    const context = profile.context;
    profile.idleTimer = setTimeout(() => {
      void this.locked(profile, async () => {
        if (!this.stopping && profile.context === context && this.canRecycle(profile)) await this.closeProfile(profile);
      }).catch(error => { process.stderr.write('空闲浏览器释放失败：' + error.message + '\n'); this.scheduleIdle(profile); });
    }, this.idleTimeoutMs);
    profile.idleTimer.unref();
  }

  capacityWait() {
    const waiting = {};
    waiting.promise = new Promise(resolve => { waiting.resolve = resolve; });
    this.capacityWaiters.add(waiting.resolve);
    return waiting;
  }

  async waitCapacity(task, waiting) {
    try { await waitTask(task, waiting.promise); }
    finally { this.capacityWaiters.delete(waiting.resolve); }
  }

  // 容量锁只分配槽位或认领空闲档案，不能在锁内等待原生启动/关闭。
  async reserve(profile, task) {
    while (true) {
      checkCancelled(task);
      let waiting, victim;
      const granted = await this.locked(this.pool, async () => {
        checkCancelled(task);
        const count = [...this.profiles.values()].filter(item => item.context || item.reserved || item.recycling).length;
        if (count < this.maxBrowsers) { profile.reserved = true; return true; }
        victim = [...this.profiles.values()].filter(item => this.canRecycle(item)).sort((a, b) => (a.lastUsed || 0) - (b.lastUsed || 0))[0];
        if (victim) victim.recycling = true;
        else waiting = this.capacityWait();
        return false;
      });
      if (granted) return;
      if (victim) await this.locked(victim, () => this.closeProfile(victim));
      else await this.waitCapacity(task, waiting);
    }
  }

  async acquire(request, task) {
    const profile = this.profile(request.profile_id);
    // dispatch 可提前回应取消；档案锁仍等待原生生命周期和迟到资源清理。
    while (true) {
      let waiting;
      const acquired = await this.locked(profile, async () => {
        if (task.controller.signal.aborted || this.stopping) throw fail('cancelled', '请求已取消');
        if (profile.recycling) { waiting = this.capacityWait(); return null; }
        profile.acquiring = true;
        try {
          const headless = ['login', 'open'].includes(request.op) ? false : ['confirm-login', 'refresh', 'quota'].includes(request.op) && profile.context ? profile.mode : true;
          const proxyURL = request.proxy_url || '';
          if (profile.context && (profile.mode !== headless || profile.proxyURL !== proxyURL)) {
            if (profile.active) throw fail('profile_busy', '档案有活动请求，无法切换模式或代理');
            await this.closeProfile(profile);
            checkCancelled(task);
          }
          if (!profile.context) {
            await this.reserve(profile, task);
            checkCancelled(task);
            profile.identity = null;
            profile.identityCookieKey = null;
            await mkdir(profile.directory, { recursive: true });
            checkCancelled(task);
            const identityPath = join(profile.directory, 'identity.json');
            let identity;
            try { identity = JSON.parse(await readFile(identityPath, 'utf8')); }
            catch (error) {
              if (error.code !== 'ENOENT') throw error;
              identity = await this.factory.generate({ os: process.platform === 'win32' ? 'windows' : process.platform === 'darwin' ? 'macos' : 'linux' });
              checkCancelled(task);
              await writeFile(identityPath, JSON.stringify(identity), { flag: 'wx' });
            }
            checkCancelled(task);
            const context = await this.factory.launch({ identity, userDataDir: join(profile.directory, 'browser'), headless, proxy: proxyOptions(proxyURL), timeout: request.timeout_ms ?? 0 });
            profile.context = context;
            context.on('close', () => {
              if (profile.context !== context) return;
              profile.context = null;
              profile.page = null;
              profile.identity = null;
              profile.identityCookieKey = null;
              clearTimeout(profile.idleTimer);
              this.wakeCapacity();
            });
            profile.mode = headless;
            profile.proxyURL = proxyURL;
            if (task.controller.signal.aborted || this.stopping) {
              await this.closeProfile(profile);
              checkCancelled(task);
              throw fail('worker_stopped', '工作进程已关闭');
            }
          }
          clearTimeout(profile.idleTimer);
          profile.active++;
          task.profile = profile;
          return profile;
        } finally {
          profile.acquiring = false;
          profile.reserved = false;
          this.wakeCapacity();
        }
      });
      if (acquired) return acquired;
      await this.waitCapacity(task, waiting);
    }
  }

  async dispatch(request) {
    const event = payload => this.emit({ ...payload, id: typeof request?.id === 'string' ? request.id : '', account_id: request?.account_id ?? 0 });
    let task;
    try {
      if (!request || typeof request.id !== 'string' || !request.id || !Number.isInteger(request.account_id) || !OPS.has(request.op)) throw fail('invalid_request', '请求 id、account_id 或 op 无效');
      // quota 的总上限从 acquire 前开始，覆盖指纹生成、启动、建页与首次身份导航。
      if (request.op === 'quota') request = { ...request, timeout_ms: Number.isFinite(request.timeout_ms) && request.timeout_ms > 0 ? request.timeout_ms : QUOTA_TIMEOUT_MS };
      if (this.tasks.has(request.id)) throw fail('duplicate_id', '请求 id 正在使用');
      if (request.op === 'shutdown') { await this.shutdown(); await event({ type: 'end' }); return; }
      if (this.stopping) throw fail('worker_stopped', '工作进程已关闭');
      if (request.op === 'cancel') {
        const target = this.tasks.get(request.target_id);
        if (target && target.request.account_id === request.account_id) await this.cancel(target, 'cancelled');
        await event({ type: 'end' });
        return;
      }
      if (request.op === 'close') {
        const profile = this.profile(request.profile_id);
        await this.locked(profile, async () => {
          if (profile.active) throw fail('profile_busy', '档案有活动请求，无法关闭');
          await this.closeProfile(profile);
        });
        await event({ type: 'end' });
        return;
      }
      let resolveDone;
      task = { request, controller: new AbortController(), done: new Promise(resolve => { resolveDone = resolve; }), resolveDone, page: null, keepPage: false, cancelCode: 'cancelled' };
      this.tasks.set(request.id, task);
      if (request.timeout_ms > 0) task.timer = setTimeout(() => { void this.cancel(task, 'timeout'); }, request.timeout_ms);
      const acquiring = this.acquire(request, task);
      const profile = await waitTask(task, acquiring);
      if (task.controller.signal.aborted) throw fail(task.cancelCode, '请求已取消');
      if (['login', 'open', 'confirm-login', 'refresh', 'quota'].includes(request.op)) {
        const session = await this.session(profile, task);
        checkCancelled(task);
        this.expectedSession(task, session, false);
        if (['confirm-login', 'refresh', 'quota'].includes(request.op) && session.login_state !== 'ready') throw fail('login_required', '页面账号身份或登录 token 不完整，需要登录确认', session);
        if (request.op === 'quota') {
          const quota = await this.quota(task);
          const verified = task.quotaRPCIdentity
            ? await this.snapshot(profile, task, task.quotaRPCIdentity, task.sessionCookieKey)
            : await this.freshSnapshot(profile, task, task.page);
          this.expectedSession(task, verified, false);
          if (verified.login_state !== 'ready') throw fail('login_required', '用量读取后账号身份失效，需要登录确认', verified);
          checkCancelled(task);
          await event({ type: 'end', session: verified, quota });
        } else await event({ type: 'end', session });
      } else {
        task.page = await profile.context.newPage();
        if (task.controller.signal.aborted) throw fail(task.cancelCode, '请求已取消');
        if (request.op === 'request') await this.fetch(task, event);
        else await this.download(task, event);
        if (task.controller.signal.aborted) throw fail(task.cancelCode, '请求已取消');
        const session = googleService(request.url) ? await this.responseSession(task) : undefined;
        if (session) this.expectedSession(task, session, true);
        checkCancelled(task);
        await event({ type: 'end', ...(session ? { session } : {}) });
      }
    } catch (error) {
      const session = error.session || (!task?.controller.signal.aborted && task?.session);
      await event({ type: 'error', code: task?.controller.signal.aborted ? task.cancelCode : error.code || 'browser_error', error: error.message, ...(session ? { session } : {}) });
    } finally {
      if (task) {
        clearTimeout(task.timer);
        if (task.page && !task.keepPage) await task.page.close().catch(() => {});
        await task.identityPage?.close().catch(() => {});
        if (task.profile?.userTask === task) task.profile.userTask = null;
        if (task.profile) {
          task.profile.active--;
          task.profile.lastUsed = Date.now();
          this.scheduleIdle(task.profile);
          this.wakeCapacity();
        }
        this.tasks.delete(request.id);
        task.resolveDone();
      }
    }
  }

  async session(profile, task) {
    const op = task.request.op;
    const userPage = ['login', 'open'].includes(op);
    let navigate = true;
    if (userPage) {
      await this.locked(profile, async () => {
        checkCancelled(task);
        if (profile.userTask) throw fail('profile_busy', '登录页面有活动操作');
        profile.userTask = task;
        if (profile.page?.isClosed?.()) profile.page = null;
        navigate = !profile.page;
        task.page = profile.page || await profile.context.newPage();
        if (navigate) task.page.on('close', () => {
          if (profile.page === task.page) profile.page = null;
          this.scheduleIdle(profile);
          this.wakeCapacity();
        });
        task.ownsUserPage = navigate;
        checkCancelled(task);
        profile.page = task.page;
        task.keepPage = true;
      });
      await waitTask(task, Promise.resolve(task.page.bringToFront?.()));
    } else {
      const creating = profile.context.newPage();
      task.page = op === 'quota' ? await waitTask(task, creating, page => page.close()) : await creating;
    }
    checkCancelled(task);
    if (op === 'quota' && profile.identity && profile.identityCookieKey === authCookieKey(await waitTask(task, profile.context.cookies(GEMINI)))) {
      // 认证Cookie一致时复用已验证身份；临时同源轻页面不下载整个app或扰动用户页。
      const identity = profile.identity;
      const cookieKey = profile.identityCookieKey;
      try {
        await waitTask(task, task.page.goto(QUOTA_RPC_PAGE, { waitUntil: 'domcontentloaded', timeout: task.request.timeout_ms }));
      } catch {
        // 普通导航失败回退app；取消或总超时仍立即透传，不能在关闭后继续请求。
        checkCancelled(task);
        return this.navigateSnapshot(profile, task, task.page);
      }
      checkCancelled(task);
      if (new URL(task.page.url()).origin === new URL(GEMINI).origin) {
        task.quotaCachedIdentity = true;
        task.page.identityCookieKey = cookieKey;
        return this.snapshot(profile, task, identity, cookieKey);
      }
    }
    if (navigate) return this.navigateSnapshot(profile, task, task.page);
    checkCancelled(task);
    if (!navigate && task.page.identityCookieKey !== authCookieKey(await waitTask(task, profile.context.cookies(GEMINI)))) {
      task.identityPage = await profile.context.newPage();
      checkCancelled(task);
      return this.navigateSnapshot(profile, task, task.identityPage);
    }
    return this.freshSnapshot(profile, task, task.page);
  }

  async quota(task) {
    const { page, profile } = task;
    const identity = profile.identity;
    let rpcText;
    try {
      rpcText = await waitTask(task, page.evaluate(fetchQuotaRPCInPage, { token: identity?.token }));
    } catch {
      checkCancelled(task);
    }
    const fast = quotaRPCObservation(rpcText);
    if (fast) {
      // 读取期间身份变化时丢弃观测，不把原账号比例交给新的Cookie身份。
      const cookieKey = authCookieKey(await waitTask(task, profile.context.cookies(GEMINI)));
      if (cookieKey !== task.sessionCookieKey) {
        const session = await this.snapshot(profile, task, { email: '', emails: [], token: '', url: GEMINI }, task.sessionCookieKey);
        throw fail('login_required', '用量读取期间认证身份变化，需要重新确认', session);
      }
      task.quotaRPCIdentity = identity;
      checkCancelled(task);
      return fast;
    }
    if (task.quotaCachedIdentity) {
      // 轻量请求失效才重新载入app核实身份，再回退真实usage面板。
      const session = await this.navigateSnapshot(profile, task, page);
      this.expectedSession(task, session, false);
      if (session.login_state !== 'ready') throw fail('login_required', '用量页面账号身份未确认', session);
    }
    let observation;
    try {
      await waitTask(task, page.goto('https://gemini.google.com/usage', { waitUntil: 'domcontentloaded', timeout: task.request.timeout_ms || 15000 }));
      await waitTask(task, page.waitForSelector('[data-usage-island-root] [data-testid="current-usage"], [data-usage-island-root] [data-testid="weekly-usage"], usage-metrics-window [data-test-id="gxu-currently"], usage-metrics-window [data-test-id="gxu-weekly"]', { state: 'visible', timeout: 8000 }));
      observation = await waitTask(task, page.evaluate(inspectQuotaPanel));
      if (observation.found) {
        // 精确比率和恢复时间优先采用网页自身已确认的只读 RPC。
        const session = await this.freshSnapshot(profile, task, page);
        this.expectedSession(task, session, false);
        if (session.login_state !== 'ready') throw fail('login_required', '用量页面账号身份未确认', session);
        const text = await waitTask(task, page.evaluate(fetchQuotaRPCInPage, { token: profile.identity.token }));
        observation.rpc = decodeQuotaRPC(text);
      }
    } catch (error) {
      checkCancelled(task);
      if (error.code === 'login_required' || error.code === 'account_mismatch') throw error;
      const unknown = parseQuotaPanel(observation);
      unknown.error ||= '用量限额页面读取失败';
      return unknown;
    }
    return parseQuotaPanel(observation);
  }

  async navigateSnapshot(profile, task, page) {
    const cookieKey = authCookieKey(await waitTask(task, profile.context.cookies(GEMINI)));
    page.identityCookieKey = null;
    const navigation = page.goto(GEMINI, { waitUntil: 'domcontentloaded', timeout: task.request.timeout_ms ?? 0 });
    if (task.request.op === 'quota') await waitTask(task, navigation);
    else await navigation;
    checkCancelled(task);
    return this.freshSnapshot(profile, task, page, cookieKey);
  }

  async freshSnapshot(profile, task, page, cookieKey = page.identityCookieKey) {
    const before = authCookieKey(await waitTask(task, profile.context.cookies(GEMINI)));
    if (cookieKey === null || cookieKey === undefined || before !== cookieKey) return this.snapshot(profile, task, { email: '', emails: [], token: '', url: GEMINI }, cookieKey ?? 'unverified-page');
    const identity = await waitTask(task, page.evaluate(inspectGeminiIdentity));
    const session = await this.snapshot(profile, task, identity, cookieKey);
    page.identityCookieKey = session.login_state === 'ready' ? cookieKey : null;
    return session;
  }

  expectedSession(task, session, required) {
    const expected = task.request.expected_email?.trim().toLowerCase();
    const mismatch = expected && session.email && expected !== session.email.toLowerCase();
    if (mismatch || (required && (!expected || session.login_state !== 'ready'))) {
      task.session = { ...session, login_state: 'needs_login' };
      throw fail(mismatch ? 'account_mismatch' : 'login_required', mismatch ? '浏览器账号与 expected_email 不匹配' : '需要已绑定并验证的浏览器账号', task.session);
    }
  }

  async sendCookies(task) {
    const cookies = await waitTask(task, task.profile.context.cookies(GEMINI));
    if (!task.verifiedCookieKey || authCookieKey(cookies) !== task.verifiedCookieKey) {
      task.profile.identity = null;
      task.profile.identityCookieKey = null;
      task.session = { email: '', cookie: cookies.map(cookie => `${cookie.name}=${cookie.value}`).join('; '), profile_id: task.profile.id, login_state: 'needs_login' };
      throw fail('login_required', '发送前浏览器认证 Cookie 已变化，需要重新确认账号', task.session);
    }
    this.expectedSession(task, task.session, true);
    return cookies;
  }

  async snapshot(profile, task, identity, expectedCookieKey) {
    checkCancelled(task);
    const cookies = await waitTask(task, profile.context.cookies(GEMINI));
    const cookieKey = authCookieKey(cookies);
    if (expectedCookieKey !== undefined && cookieKey !== expectedCookieKey) identity = { email: '', emails: [], token: '', url: GEMINI };
    const verified = new URL(identity.url).origin === new URL(GEMINI).origin && identity.email && identity.emails.length === 1 && identity.token && cookies.some(cookie => ['SAPISID', '__Secure-1PAPISID', '__Secure-3PAPISID'].includes(cookie.name) && cookie.value);
    profile.identity = verified ? identity : null;
    profile.identityCookieKey = verified ? cookieKey : null;
    task.sessionCookieKey = verified ? cookieKey : null;
    task.session = { email: identity.email || '', cookie: cookies.map(cookie => `${cookie.name}=${cookie.value}`).join('; '), profile_id: profile.id, login_state: verified ? 'ready' : ['login', 'open'].includes(task.request.op) ? 'logging_in' : 'needs_login' };
    checkCancelled(task);
    return task.session;
  }

  async responseSession(task) {
    const profile = task.profile;
    let cookieKey = authCookieKey(await waitTask(task, profile.context.cookies(GEMINI)));
    let identity = cookieKey === profile.identityCookieKey ? profile.identity : null;
    if (task.request.op === 'request' && new URL(task.page.url()).origin === new URL(GEMINI).origin && task.page.identityCookieKey === cookieKey) return this.freshSnapshot(profile, task, task.page);
    if (!identity) {
      await task.identityPage?.close().catch(() => {});
      task.identityPage = await profile.context.newPage();
      checkCancelled(task);
      return this.navigateSnapshot(profile, task, task.identityPage);
    }
    return this.snapshot(profile, task, identity, cookieKey);
  }

  async fetch(task, event) {
    const { request, page } = task;
    const origin = new URL(request.url).origin;
    if (googleService(request.url)) {
      const session = await this.navigateSnapshot(task.profile, task, page);
      this.expectedSession(task, session, true);
      task.verifiedCookieKey = page.identityCookieKey;
    } else await page.goto(origin, { waitUntil: 'domcontentloaded', timeout: request.timeout_ms ?? 0 });
    checkCancelled(task);
    await this.streamFetch(task, event);
  }

  async streamFetch(task, event, url = task.request.url) {
    const { request, page } = task;
    await page.exposeBinding('__worker_stream', async (_, payload) => {
      if (task.controller.signal.aborted) throw fail(task.cancelCode, '请求已取消');
      await event(payload);
    });
    const headers = browserHeaders(request.headers);
    if (googleService(request.url)) {
      const cookies = await this.sendCookies(task);
      const authorization = Object.keys(headers).find(name => name.toLowerCase() === 'authorization' && /^SAPISIDHASH\s/.test(headers[name]));
      if (authorization) {
        const sapisid = ['SAPISID', '__Secure-3PAPISID', '__Secure-1PAPISID'].map(name => cookies.find(cookie => cookie.name === name)?.value).find(Boolean);
        if (!sapisid) throw fail('login_required', '浏览器缺少 SAPISID 认证 Cookie', task.session);
        const timestamp = Math.floor(Date.now() / 1000);
        headers[authorization] = `SAPISIDHASH ${timestamp}_${createHash('sha1').update(`${timestamp} ${sapisid} https://gemini.google.com`).digest('hex')}`;
      }
    }
    checkCancelled(task);
    await page.evaluate(fetchInPage, { url, method: request.method || 'GET', headers, body_base64: request.body_base64 });
  }

  async download(task, event) {
    const { page, request } = task;
    if (googleService(request.url)) {
      const session = await this.responseSession(task);
      this.expectedSession(task, session, true);
      task.verifiedCookieKey = task.sessionCookieKey;
    }
    let download;
    let observedResponse;
    page.on('download', item => {
      download = item;
      task.download = item;
      if (task.controller.signal.aborted) void item.cancel().catch(() => {});
    });
    page.on('response', item => { if (item.url() === request.url || item.request?.().isNavigationRequest?.()) observedResponse = item; });
    let response;
    if (googleService(request.url)) await this.sendCookies(task);
    try { response = await page.goto(request.url, { waitUntil: 'load', timeout: request.timeout_ms ?? 0 }); }
    catch (error) { if (!download) throw error; }
    if (!download) {
      if (!response) throw fail('download_error', '浏览器未返回媒体响应');
      // 原生媒体文档已位于媒体源；同源 fetch 逐块读取，避免 response.body 累积视频。
      await this.streamFetch(task, event, response.url());
      return;
    }
    const temporary = await mkdtemp(join(tmpdir(), 'gemini-download-'));
    try {
      const file = join(temporary, randomUUID());
      await download.saveAs(file);
      if (task.controller.signal.aborted) throw fail(task.cancelCode, '下载已取消');
      const metadata = response || observedResponse;
      if (!metadata) throw fail('download_metadata_unavailable', '浏览器下载缺少原生 HTTP 状态和响应头');
      await event({ type: 'start', status: metadata.status(), headers: await metadata.allHeaders(), url: download.url() });
      for await (const chunk of createReadStream(file, { signal: task.controller.signal })) await event({ type: 'chunk', data_base64: chunk.toString('base64') });
    } finally {
      await download.delete().catch(() => {});
      await rm(temporary, { recursive: true, force: true });
      task.download = null;
    }
  }

  async cancel(task, code) {
    task.cancelCode = code;
    task.controller.abort();
    await task.download?.cancel().catch(() => {});
    if (task.page && (!task.keepPage || task.ownsUserPage)) await task.page.close().catch(() => {});
    await task.identityPage?.close().catch(() => {});
    if (task.keepPage && task.ownsUserPage && task.profile) task.profile.page = null;
  }

  async shutdown() {
    this.stopping = true;
    this.wakeCapacity();
    const tasks = [...this.tasks.values()];
    await Promise.all(tasks.map(task => this.cancel(task, 'cancelled')));
    await Promise.all(tasks.map(task => task.done));
    await Promise.all([...this.profiles.values()].map(profile => this.locked(profile, () => this.closeProfile(profile))));
  }
}

export function writeJSONLine(output, event) {
  return new Promise((resolve, reject) => output.write(JSON.stringify(event) + '\n', error => error ? reject(error) : resolve()));
}

export async function runIPC({ input = process.stdin, output = process.stdout, diagnostics = process.stderr, workerOptions = {} } = {}) {
  const worker = new BrowserWorker({ ...workerOptions, emit: event => writeJSONLine(output, event) });
  // 浏览器进程使用随档案保留的临时目录，避免继承会话结束后被清理的 TEMP。
  const temporaryRoot = join(worker.profileRoot, '.tmp');
  await mkdir(temporaryRoot, { recursive: true });
  process.env.TMPDIR = temporaryRoot;
  process.env.TEMP = temporaryRoot;
  process.env.TMP = temporaryRoot;
  const pending = new Set();
  await writeJSONLine(output, { id: '', account_id: 0, type: 'ready' });
  const lines = createInterface({ input, crlfDelay: Infinity });
  try {
    for await (const line of lines) {
      if (!line.trim()) continue;
      let request;
      try { request = JSON.parse(line); }
      catch (error) { await writeJSONLine(output, { id: '', account_id: 0, type: 'error', code: 'invalid_json', error: error.message }); continue; }
      const running = worker.dispatch(request).catch(error => { diagnostics.write(error.message + '\n'); }).finally(() => pending.delete(running));
      pending.add(running);
      if (request?.op === 'shutdown') { await running; break; }
    }
  } finally {
    await worker.shutdown();
    await Promise.all(pending);
    lines.close();
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  runIPC().catch(error => { process.stderr.write(error.stack + '\n'); process.exitCode = 1; });
}
