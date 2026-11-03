import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { camoufoxFactory } from '../worker.mjs';

const root = fileURLToPath(new URL('../../', import.meta.url));
const artifacts = join(root, '.codex');
const normal = { state: 'available', used_percent: 0, weekly_used_percent: 4, reset_at: 1792817460, weekly_reset_at: 1793325060, blocked_until: null, limited_by: '', checked_at: 1792807840, error: '', usage_text: '', eligible: true };
const limited = { state: 'limited', used_percent: 100, weekly_used_percent: 17, reset_at: 1792817460, weekly_reset_at: 1793325060, blocked_until: 1792817460, limited_by: 'five_hour', checked_at: 1792807840, error: '', usage_text: '', eligible: false };
const unknown = { state: 'unknown', used_percent: null, weekly_used_percent: null, reset_at: null, weekly_reset_at: null, blocked_until: null, limited_by: '', checked_at: 0, error: '', usage_text: '', eligible: true };
const account = (id, quota, overrides = {}) => ({ id, label: '额度样本 ' + id, email: '', note: '', profile_id: 'fixture-profile-' + id, login_state: 'ready', status: 'enabled', selected: false, health: 'ok', cookie_count: 1, key_cookies: [], has_1psidts: true, fail_count: 0, proxy_name: '直连', last_ok_at: 0, last_error: '', quota: { ...quota }, ...overrides });

// 故障目标：真实页面把 0 当缺失、窗口混用、取整冒充边界、到期解锁或刷新丢失已确认状态。
test('管理页真实 Firefox 额度、操作与生命周期', { timeout: 120000 }, async t => {
  const html = await readFile(join(root, 'internal/app/admin_ui/index.html'));
  // 透明旁观真实计时器；经典脚本在 Camoufox 主页面执行，所有调用和时间仍转交浏览器。
  const timerObserver = `<script>(() => {
    const handles = new Set();
    const start = window.setInterval.bind(window), stop = window.clearInterval.bind(window);
    const record = () => document.documentElement.dataset.fixtureIntervals = handles.size;
    window.setInterval = (...args) => { const handle = start(...args); handles.add(handle); record(); return handle; };
    window.clearInterval = handle => { handles.delete(handle); record(); return stop(handle); };
    record();
  })();</script>`;
  const observedHTML = html.toString().replace('<head>', '<head>' + timerObserver);
  const chart = await readFile(join(root, 'internal/app/admin_ui/chart.umd.min.js'));
  const directory = await mkdtemp(join(tmpdir(), 'gw2a-admin-quota-'));
  const calls = [];
  let items = [], selectedID = 0, stats = {}, quotaResponse = null, authenticated = true, listFailure = null;
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    const path = url.pathname;
    let body = '';
    for await (const chunk of req) body += chunk;
    const reply = (data, status = 200) => { res.writeHead(status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(data)); };
    if (path === '/admin/' || path === '/admin/chart.umd.min.js') {
      res.writeHead(200, { 'Content-Type': path.endsWith('.js') ? 'text/javascript' : 'text/html; charset=utf-8' });
      res.end(path.endsWith('.js') ? chart : observedHTML); return;
    }
    calls.push({ path, method: req.method, body });
    if (path === '/') return reply({ version: 'fixture', models: [] });
    if (path === '/admin/api/login') { authenticated = true; return reply({ ok: true }); }
    if (path === '/admin/api/logout') { authenticated = false; return reply({ ok: true }); }
    if (!authenticated) return reply({ error: 'unauthorized' }, 401);
    if (path === '/admin/api/accounts/selection') { selectedID = JSON.parse(body).account_id; return reply({ ok: true }); }
    if (path === '/admin/api/accounts') {
      if (req.method === 'POST') { items.push(account(12, unknown, { login_state: 'needs_login' })); return reply({ id: 12 }, 201); }
      if (listFailure) {
        const failure = listFailure;
        if (failure.delay) await new Promise(resolve => setTimeout(resolve, failure.delay));
        if (failure.disconnect) { res.destroy(); return; }
        if (failure.status === 401) authenticated = false;
        return reply({ error: 'fixture list read failed' }, failure.status);
      }
      return reply({ items: items.map(a => ({ ...a, selected: a.id === selectedID })), account_id: selectedID, total: items.length, enabled: items.length, ready: 1, pool_ready: 1, quota_limited: 0, ...stats });
    }
    const item = path.match(/^\/admin\/api\/accounts\/(\d+)(?:\/(.+))?$/);
    if (item) {
      const id = Number(item[1]);
      const a = items.find(a => a.id === id);
      if (item[2] === 'quota') {
        const response = quotaResponse;
        response?.captured?.();
        if (response?.wait) await response.wait;
        if (response?.account) items = items.map(a => a.id === id ? response.account : a);
        if (response?.stats) stats = response.stats;
        if (response?.listFailure) listFailure = response.listFailure;
        if (response?.status === 401) authenticated = false;
        if (response?.disconnect) { res.destroy(); return; }
        return reply({ ok: response?.status !== 409, error: response?.error || '', account: response?.account || a }, response?.status || 200);
      }
      if (req.method === 'DELETE') { items = items.filter(a => a.id !== id); return reply({ ok: true }); }
      if (req.method === 'PATCH') { Object.assign(a, JSON.parse(body)); return reply({ ok: true }); }
      if (item[2] === 'login') a.login_state = 'logging_in';
      if (item[2] === 'confirm-login') a.login_state = 'ready';
      return reply(item[2] ? { ok: true, account: a } : a);
    }
    if (path.endsWith('/stats')) return reply({ total_requests: 0, success_rate: 0, success: 0, failures: 0, avg_ms: 0 });
    if (path.endsWith('/proxies') || path.endsWith('/usage')) return reply({ items: [] });
    if (path.endsWith('/timeseries')) return reply({ series: [] });
    reply({ error: 'fixture unhandled path: ' + path }, 404);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let context;
  try {
    const identity = await camoufoxFactory.generate({ os: process.platform === 'win32' ? 'windows' : 'linux' });
    context = await camoufoxFactory.launch({ identity, userDataDir: join(directory, 'browser'), headless: true, timeout: 30000 });
    const page = await context.newPage();
    await page.addInitScript(() => localStorage.setItem('lang', 'zh'));
    await page.setViewportSize({ width: 2400, height: 1200 });
    page.setDefaultTimeout(2000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const base = `http://127.0.0.1:${server.address().port}/admin/`;
    const open = async (rows, options = {}) => {
      items = rows; selectedID = options.selectedID || 0; stats = options.stats || {}; quotaResponse = null; authenticated = true; listFailure = null;
      // 初始化等待页面和账号数据实际就绪，交互断言仍沿用原来的 2 秒期限。
      await page.goto(base, { waitUntil: 'domcontentloaded', timeout: 15000 });
      await page.locator('nav [data-page="cookies"]').click();
      await page.locator('tbody tr').first().waitFor({ timeout: 15000 });
    };
    // 使用账号复选框的业务值定位，避免新增表格列后把第一列误认为账号编号。
    const row = id => page.locator('tbody tr').filter({ has: page.locator(`input.acct-cb[value="${id}"]`) });
    const cellText = async (id, selector) => row(id).locator(selector).innerText();
    const expectText = async (id, selector, expected) => assert.equal(await cellText(id, selector), expected);

    await t.test('0 已用显示 100 剩余，两窗口独立且未知不冒充 0', async () => {
      await open([account(1, normal), account(2, unknown), account(3, { ...limited, used_percent: 0, weekly_used_percent: 100, limited_by: 'weekly', blocked_until: 1793325060 })], { stats: { ready: 3, pool_ready: 2, quota_limited: 1 } });
      await expectText(1, '[data-window="five-hour"] .quota-used', '0%');
      await expectText(1, '[data-window="five-hour"] .quota-remaining', '100%');
      await expectText(1, '[data-window="weekly"] .quota-used', '4%');
      await expectText(1, '[data-window="weekly"] .quota-remaining', '96%');
      await expectText(2, '[data-window="five-hour"] .quota-used', '未知');
      await expectText(2, '[data-window="weekly"] .quota-remaining', '未知');
      assert.equal(await row(2).getByRole('button', { name: '使用此账号', exact: true }).isEnabled(), true);
      await expectText(3, '[data-window="five-hour"] .quota-remaining', '100%');
      await expectText(3, '[data-window="weekly"] .quota-remaining', '0%');
      assert.match(await cellText(3, '.quota-status'), /周额度已耗尽/);
      assert.match(await page.locator('.account-summary').innerText(), /3 登录就绪\s+2 可选号\s+1 已限额/);
    });
    await t.test('100 和 125 耗尽，极近 0/100 显示真实近似边界', async () => {
      await open([account(1, limited), account(2, { ...limited, used_percent: 125 }), account(3, { ...normal, used_percent: 0.000001, weekly_used_percent: 99.999999 })]);
      await expectText(1, '[data-window="five-hour"] .quota-used', '100%');
      await expectText(1, '[data-window="five-hour"] .quota-remaining', '0%');
      await expectText(2, '[data-window="five-hour"] .quota-used', '125%');
      await expectText(2, '[data-window="five-hour"] .quota-remaining', '0%');
      await expectText(3, '[data-window="five-hour"] .quota-used', '<0.01%');
      await expectText(3, '[data-window="five-hour"] .quota-remaining', '>99.99%');
      await expectText(3, '[data-window="weekly"] .quota-used', '>99.99%');
      await expectText(3, '[data-window="weekly"] .quota-remaining', '<0.01%');
      assert.equal(await row(2).locator('progress').first().getAttribute('value'), '100');
      assert.equal(await row(3).getByRole('button', { name: '使用此账号', exact: true }).isEnabled(), true);
    });
    await t.test('到期仍等网页确认；固定限额和人工禁用保留原状态', async () => {
      await open([account(1, { ...limited, blocked_until: 1 }, { selected: true }), account(2, { ...normal, eligible: false }, { status: 'disabled' }), account(3, { ...unknown, eligible: false }, { login_state: 'needs_login' })], { selectedID: 1 });
      assert.match(await cellText(1, '.quota-countdown'), /已到期，等待网页确认/);
      assert.match(await page.locator('.fixed-quota-warning').innerText(), /固定账号已限额/);
      assert.equal(await row(1).getByRole('button', { name: '当前使用', exact: true }).isDisabled(), true);
      assert.equal(await row(2).getByRole('button', { name: '使用此账号', exact: true }).count(), 0);
      assert.match(await row(2).innerText(), /禁用/);
      assert.match(await row(3).innerText(), /需登录/);
      assert.equal(calls.filter(c => c.path.endsWith('/selection')).length, 0);
      await page.waitForTimeout(1100);
      assert.match(await cellText(1, '.quota-status'), /5小时额度已耗尽/);
    });
    await t.test('网页时间用 Unix 秒和本机时区；倒计时实际减少', async () => {
      const future = Math.floor(Date.now() / 1000) + 120;
      await open([account(1, { ...limited, blocked_until: future })]);
      const first = await cellText(1, '.quota-countdown');
      assert.match(first, /00:0[12]:\d{2}/);
      await page.waitForTimeout(1200);
      assert.notEqual(await cellText(1, '.quota-countdown'), first);
      assert.equal(await row(1).locator('time[data-time="five-reset"]').getAttribute('datetime'), '2026-10-24T04:51:00.000Z');
      assert.equal(await row(1).locator('time[data-time="week-reset"]').getAttribute('datetime'), '2026-10-30T01:51:00.000Z');
      assert.match(await cellText(1, '.quota-cell'), /本机时间/);
    });
    await t.test('POST 无 body，进行中防重；成功更新额度、统计和固定提示', async () => {
      await open([account(1, limited)], { selectedID: 1, stats: { pool_ready: 0, quota_limited: 1 } });
      let release;
      const wait = new Promise(resolve => { release = resolve; });
      quotaResponse = { wait, account: account(1, normal, { selected: true }), stats: { pool_ready: 1, quota_limited: 0 } };
      const button = row(1).getByRole('button', { name: '刷新额度', exact: true });
      const request = page.waitForRequest(r => r.url().endsWith('/accounts/1/quota'));
      await button.click();
      const actual = await request;
      assert.equal(actual.method(), 'POST'); assert.equal(actual.postData(), null);
      const pending = row(1).getByRole('button', { name: '额度读取中…', exact: true });
      assert.equal(await pending.isDisabled(), true);
      await pending.evaluate(el => el.click());
      release();
      await page.waitForFunction(() => document.querySelector('[data-window="five-hour"] .quota-used')?.textContent === '0%');
      await page.waitForFunction(() => /1 可选号\s+0 已限额/.test(document.querySelector('.account-summary')?.innerText));
      assert.equal(calls.filter(c => c.path.endsWith('/accounts/1/quota')).length, 1);
      assert.match(await page.locator('.account-summary').innerText(), /1 可选号\s+0 已限额/);
      assert.equal(await page.locator('.fixed-quota-warning').count(), 0);
      assert.match(await page.locator('.toast').last().innerText(), /额度已更新/);
    });
    await t.test('409 最新局部比例可显示，读取错误和上次数据明确，限额仍保留', async () => {
      await open([account(1, limited)], { selectedID: 1, stats: { pool_ready: 0, quota_limited: 1 } });
      quotaResponse = { status: 409, error: '无法确认网页用量，请稍后重试', account: account(1, { ...limited, used_percent: 0, weekly_used_percent: 100, reset_at: null, limited_by: 'weekly', blocked_until: null, error: '无法确认网页用量，请稍后重试' }) };
      await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('[data-window="five-hour"] .quota-used')?.textContent === '0%');
      await expectText(1, '[data-window="weekly"] .quota-used', '100%');
      assert.match(await cellText(1, '.quota-cell'), /上次或局部数据/);
      assert.match(await cellText(1, '.quota-error'), /无法确认网页用量/);
      assert.match(await cellText(1, '.quota-countdown'), /未知/);
      assert.match(await page.locator('.fixed-quota-warning').innerText(), /固定账号已限额/);
    });
    await t.test('网络失败保留已限额和原数值；unknown 允许真实局部值，Cookie-only 资格保留', async () => {
      await open([account(1, limited), account(2, { ...unknown, used_percent: 8, checked_at: 1792807840, error: '无法确认网页用量，请稍后重试' }), account(3, unknown, { profile_id: '', login_state: 'cookie_only' })]);
      quotaResponse = { disconnect: true };
      await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
      await page.locator('.toast').waitFor();
      await expectText(1, '[data-window="five-hour"] .quota-used', '100%');
      assert.match(await cellText(1, '.quota-cell'), /上次数据；本次读取失败/);
      assert.match(await cellText(1, '.quota-status'), /5小时额度已耗尽/);
      await expectText(2, '[data-window="five-hour"] .quota-remaining', '92%');
      assert.equal(await row(2).getByRole('button', { name: '使用此账号', exact: true }).isEnabled(), true);
      assert.equal(await row(3).getByRole('button', { name: '使用此账号', exact: true }).isEnabled(), true);
      assert.match(await cellText(3, '.quota-cell'), /网页额度未知/);
    });
    // 故障目标：额度 POST 已返回后，后续列表失败或慢加载占位清空最新账号、资格及重试入口。
    for (const [postStatus, failureName, failure] of [
      [200, '网络断线', { disconnect: true, delay: 300 }],
      [409, '网络断线', { disconnect: true, delay: 300 }],
      [200, 'HTTP503', { status: 503, delay: 300 }],
      [409, 'HTTP503', { status: 503, delay: 300 }],
    ]) {
      await t.test(`后续列表失败：POST${postStatus}→GET${failureName}保留最新固定账号并可重试`, async () => {
        await open([account(1, normal)], { selectedID: 1 });
        const selectionCalls = calls.filter(c => c.path.endsWith('/selection')).length;
        quotaResponse = { status: postStatus, error: postStatus === 409 ? '无法确认网页用量，请稍后重试' : '', listFailure: failure, account: account(1, { ...limited, used_percent: 0, weekly_used_percent: 100, limited_by: 'weekly', blocked_until: Math.floor(Date.now() / 1000) + 120, error: postStatus === 409 ? '无法确认网页用量，请稍后重试' : '' }, { selected: true }) };
        await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
        await page.locator('.account-sync-error').waitFor({ timeout: 6000 });
        await expectText(1, '[data-window="five-hour"] .quota-used', '0%');
        await expectText(1, '[data-window="five-hour"] .quota-remaining', '100%');
        await expectText(1, '[data-window="weekly"] .quota-used', '100%');
        await expectText(1, '[data-window="weekly"] .quota-remaining', '0%');
        assert.match(await cellText(1, '.quota-status'), /周额度已耗尽/);
        assert.match(await page.locator('.fixed-quota-warning').innerText(), /固定账号已限额/);
        assert.match(await page.locator('.account-sync-error').innerText(), /列表.*统计.*未同步/);
        assert.equal(await row(1).getByRole('button', { name: '当前使用', exact: true }).isDisabled(), true);
        assert.equal(await row(1).getByRole('button', { name: '刷新额度', exact: true }).isEnabled(), true);
        assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '1');
        const first = await cellText(1, '.quota-countdown');
        await page.waitForFunction(initial => document.querySelector('.quota-countdown')?.textContent !== initial, first, { timeout: 3500 });
        assert.notEqual(await cellText(1, '.quota-countdown'), first);
        listFailure = null;
        quotaResponse = { account: account(1, normal, { selected: true }), stats: { pool_ready: 1, quota_limited: 0 } };
        await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
        await page.waitForFunction(() => /1 可选号\s+0 已限额/.test(document.querySelector('.account-summary')?.innerText) && !document.querySelector('.account-sync-error'));
        assert.equal(await page.locator('.fixed-quota-warning').count(), 0);
        assert.equal(await row(1).getByRole('button', { name: '当前使用', exact: true }).isDisabled(), true);
        assert.equal(calls.filter(c => c.path.endsWith('/selection')).length, selectionCalls);
      });
    }
    await t.test('后续列表失败：非固定账号按最新eligible禁用，恢复仍保留原固定选择', async () => {
      await open([account(1, normal), account(2, normal)], { selectedID: 1 });
      quotaResponse = { account: account(2, limited), listFailure: { status: 503 } };
      await row(2).getByRole('button', { name: '刷新额度', exact: true }).click();
      await page.locator('.account-sync-error').waitFor();
      assert.equal(await row(2).getByRole('button', { name: '使用此账号', exact: true }).isDisabled(), true);
      assert.equal(await row(1).getByRole('button', { name: '当前使用', exact: true }).isDisabled(), true);
      listFailure = null;
      quotaResponse = { account: account(2, normal) };
      await row(2).getByRole('button', { name: '刷新额度', exact: true }).click();
      await page.waitForFunction(() => !document.querySelector('.account-sync-error'));
      assert.equal(await row(2).getByRole('button', { name: '使用此账号', exact: true }).isEnabled(), true);
      assert.equal(await row(1).getByRole('button', { name: '当前使用', exact: true }).isDisabled(), true);
    });
    // 故障目标：另一账号完成刷新重绘表格后，迟到的 POST 仍须更新当前有效行。
    for (const [postStatus, failureName, failure, fixedID] of [
      [200, 'HTTP503', { status: 503 }, 1],
      [409, 'HTTP503', { status: 503 }, 1],
      [200, '网络断线', { disconnect: true }, 2],
      [409, '网络断线', { disconnect: true }, 2],
    ]) {
      await t.test(`并发刷新重绘：POST${postStatus}→GET${failureName}更新当前账号行`, async () => {
        await open([account(1, normal), account(2, normal)], { selectedID: fixedID });
        const selectionCalls = calls.filter(c => c.path.endsWith('/selection')).length;
        const quotaCalls = calls.filter(c => c.path.endsWith('/accounts/1/quota')).length;
        let release;
        const wait = new Promise(resolve => { release = resolve; });
        let captureResponse;
        const captured = new Promise(resolve => { captureResponse = resolve; });
        quotaResponse = { wait, captured: captureResponse, status: postStatus, error: postStatus === 409 ? '无法确认网页用量，请稍后重试' : '', listFailure: failure, account: account(1, { ...limited, weekly_used_percent: 100, limited_by: 'both', blocked_until: Math.floor(Date.now() / 1000) + 120 }, { selected: fixedID === 1 }) };
        const original = await row(1).elementHandle();
        const firstRequest = page.waitForRequest(r => r.url().endsWith('/accounts/1/quota'));
        await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
        await firstRequest;
        // 浏览器发起事件早于服务端捕获响应，确认取到第一号fixture再切换第二号。
        await captured;
        quotaResponse = { account: account(2, { ...normal, used_percent: 12 }, { selected: fixedID === 2 }) };
        await row(2).getByRole('button', { name: '刷新额度', exact: true }).click();
        await page.waitForFunction(() => document.querySelector('[data-quota-refresh="2"]')?.disabled === false && document.querySelectorAll('tbody tr')[1]?.querySelector('.quota-used')?.textContent === '12%');
        assert.equal(await original.evaluate(el => el.isConnected), false, '第一账号的请求发起行必须已被实际重绘替换');
        const pending = row(1).getByRole('button', { name: '额度读取中…', exact: true });
        assert.equal(await pending.isDisabled(), true);
        await pending.evaluate(el => el.click());
        release();
        await page.locator('.account-sync-error').waitFor({ timeout: 6000 });
        await expectText(1, '[data-window="five-hour"] .quota-used', '100%');
        await expectText(1, '[data-window="five-hour"] .quota-remaining', '0%');
        await expectText(1, '[data-window="weekly"] .quota-used', '100%');
        await expectText(1, '[data-window="weekly"] .quota-remaining', '0%');
        assert.match(await cellText(1, '.quota-status'), /两窗口额度已耗尽/);
        assert.match(await row(1).locator('.quota-detail').innerText(), /当前不可选号/);
        assert.equal(await row(1).getByRole('button', { name: fixedID === 1 ? '当前使用' : '使用此账号', exact: true }).isDisabled(), true);
        assert.equal(await row(fixedID).getByRole('button', { name: '当前使用', exact: true }).isDisabled(), true);
        assert.equal(await page.locator('.fixed-quota-warning').count(), fixedID === 1 ? 1 : 0);
        assert.equal(await row(1).getByRole('button', { name: '刷新额度', exact: true }).isEnabled(), true);
        assert.equal(calls.filter(c => c.path.endsWith('/accounts/1/quota')).length, quotaCalls + 1);
        assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '1');
        const first = await cellText(1, '.quota-countdown');
        await page.waitForFunction(initial => document.querySelector('.quota-countdown')?.textContent !== initial, first, { timeout: 3500 });
        listFailure = null;
        quotaResponse = { account: account(1, normal, { selected: fixedID === 1 }), stats: { pool_ready: 2, quota_limited: 0 } };
        await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
        await page.waitForFunction(() => !document.querySelector('.account-sync-error') && document.querySelector('.quota-used')?.textContent === '0%');
        assert.equal(await row(1).getByRole('button', { name: fixedID === 1 ? '当前使用' : '使用此账号', exact: true }).isEnabled(), fixedID !== 1);
        assert.equal(await page.locator('.fixed-quota-warning').count(), 0);
        assert.equal(calls.filter(c => c.path.endsWith('/selection')).length, selectionCalls);
        await original.dispose();
      });
    }
    // 边界保护：迟到的成功响应不能在导航或登录失效后重新插入旧账号表。
    for (const departure of ['导航离开', '其他请求401']) {
      await t.test(`刷新途中${departure}后POST200不恢复旧表`, async () => {
        await open([account(1, normal), account(2, normal)], { selectedID: 1 });
        let release;
        const wait = new Promise(resolve => { release = resolve; });
        quotaResponse = { wait, account: account(1, limited, { selected: true }), listFailure: { status: 503 } };
        const request = page.waitForRequest(r => r.url().endsWith('/accounts/1/quota'));
        await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
        await request;
        if (departure === '导航离开') {
          await page.locator('nav [data-page="proxies"]').click();
          await page.locator('nav [data-page="proxies"].active').waitFor();
          await page.waitForFunction(() => !document.querySelector('.account-table'));
        } else {
          quotaResponse = { status: 401, error: 'unauthorized' };
          await row(2).getByRole('button', { name: '刷新额度', exact: true }).click();
          await page.locator('#tokenInput').waitFor();
        }
        release();
        await page.locator('.toast').filter({ hasText: '额度已更新' }).last().waitFor();
        assert.equal(await page.locator('.account-table').count(), 0);
        assert.equal(await page.locator('.fixed-quota-warning').count(), 0);
        assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '0');
        if (departure === '其他请求401') assert.equal(await page.locator('#tokenInput').isVisible(), true);
      });
    }
    for (const stage of ['POST', '后续GET']) {
      await t.test(`后续列表失败：${stage}401保留登录流程且不恢复旧表格`, async () => {
        await open([account(1, limited)], { selectedID: 1 });
        quotaResponse = stage === 'POST' ? { status: 401, error: 'unauthorized' } : { account: account(1, normal, { selected: true }), listFailure: { status: 401, delay: 300 } };
        await row(1).getByRole('button', { name: '刷新额度', exact: true }).click();
        await page.locator('#tokenInput').waitFor();
        await page.waitForTimeout(350);
        assert.equal(await page.locator('.account-table').count(), 0);
        assert.equal(await page.locator('.account-sync-error').count(), 0);
        assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '0');
      });
    }
    await t.test('原登录、确认、打开、刷新、选择、禁用和删除仍发送原请求', async () => {
      await open([account(1, normal, { login_state: 'needs_login' }), account(2, normal)]);
      for (const [label, suffix] of [['登录', 'login'], ['完成登录', 'confirm-login'], ['打开窗口', 'open'], ['刷新', 'refresh']]) {
        const request = page.waitForRequest(r => r.url().endsWith('/accounts/1/' + suffix));
        await row(1).getByRole('button', { name: label, exact: true }).click();
        assert.equal((await request).method(), 'POST');
        await page.waitForTimeout(70);
      }
      const selection = page.waitForRequest(r => r.url().endsWith('/selection'));
      await row(2).getByRole('button', { name: '使用此账号', exact: true }).click();
      assert.deepEqual((await selection).postDataJSON(), { account_id: 2 });
      await row(2).getByRole('button', { name: '当前使用', exact: true }).waitFor();
      await row(2).getByRole('button', { name: '禁用', exact: true }).click();
      await row(2).getByRole('button', { name: '启用', exact: true }).waitFor();
      await row(2).getByRole('button', { name: '删除', exact: true }).click();
      await page.locator('.modal').getByRole('button', { name: '删除', exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll('tbody tr').length === 1);
    });
    await t.test('英文切换、宽屏和手机滚动布局；截图遮账号/会话标识', async () => {
      await open([account(1, normal), account(2, limited), account(3, { ...limited, used_percent: 125, weekly_used_percent: 100, limited_by: 'both', blocked_until: null }), account(4, unknown)], { selectedID: 2, stats: { ready: 4, pool_ready: 2, quota_limited: 2 } });
      await mkdir(artifacts, { recursive: true });
      const mask = [page.locator('tbody td:nth-child(1)'), page.locator('tbody td:nth-child(2)'), page.locator('tbody td:nth-child(4)')];
      await page.locator('.table-scroll').screenshot({ path: join(artifacts, 'quota-task-3-wide.png'), mask });
      await page.locator('#langBtn').click();
      await page.waitForFunction(() => document.documentElement.lang === 'en');
      assert.match(await cellText(1, '.quota-cell'), /5-hour used/);
      assert.match(await cellText(1, '.quota-cell'), /Weekly used/);
      assert.equal(await page.locator('.quota-values').evaluateAll(elements => elements.every(el => el.scrollWidth <= el.clientWidth)), true, '英文数值不得溢入下一列');
      assert.equal(await page.locator('.quota-status .badge').evaluateAll(elements => elements.every(el => el.getBoundingClientRect().right <= el.closest('td').getBoundingClientRect().right)), true, '英文状态不得溢入下一列');
      assert.equal(await row(1).getByRole('button', { name: 'Refresh quota', exact: true }).count(), 1);
      assert.match(await page.locator('.fixed-quota-warning').innerText(), /Fixed account is quota limited/);
      await page.locator('.table-scroll').screenshot({ path: join(artifacts, 'quota-task-3-en.png'), mask });
      await page.setViewportSize({ width: 390, height: 844 });
      assert.equal(await page.locator('.table-scroll').evaluate(el => el.scrollWidth > el.clientWidth), true);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await row(1).locator('.quota-cell').scrollIntoViewIfNeeded();
      await page.locator('.table-scroll').screenshot({ path: join(artifacts, 'quota-task-3-mobile.png'), mask });
      await page.setViewportSize({ width: 2400, height: 1200 });
      await page.locator('#langBtn').click();
    });
    await t.test('离开账号页与退出登录会清理倒计时，登录入口仍可用', async () => {
      await open([account(1, limited)]);
      assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '1');
      await page.locator('nav [data-page="proxies"]').click();
      assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '0');
      await page.locator('nav [data-page="cookies"]').click();
      await row(1).locator('.quota-cell').waitFor();
      await page.locator('.logout-btn').click();
      await page.locator('.modal').getByRole('button', { name: '退出', exact: true }).click();
      await page.locator('#tokenInput').waitFor();
      assert.equal(await page.locator('html').getAttribute('data-fixture-intervals'), '0');
      await page.locator('#tokenInput').fill('fixture-local-token');
      const login = page.waitForRequest(r => r.url().endsWith('/api/login'));
      await page.locator('.login-box').getByRole('button', { name: '登录', exact: true }).click();
      assert.deepEqual((await login).postDataJSON(), { token: 'fixture-local-token' });
      await page.locator('nav [data-page="cookies"]').waitFor();
    });
    assert.deepEqual(errors, []);
  } finally {
    await context?.close();
    await new Promise(resolve => server.close(resolve));
    await rm(directory, { recursive: true, force: true });
  }
});
