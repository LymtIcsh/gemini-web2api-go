import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runInNewContext } from 'node:vm';
import * as quota from '../worker.mjs';

const sample = (current, weekly = null, extra = {}) => ({ found: true, current: current === null ? null : { text: current }, weekly: weekly === null ? null : { text: weekly }, checked_at: 1792807840, time_zone: 'Asia/Shanghai', ...extra });
const parse = data => quota.parseQuotaPanel?.(data);

// 故障目标：把两个窗口混为一谈，或丢失真实页面比例及浏览器时区日期。
test('真实面板窗口文本解析为独立比例及浏览器时区恢复时间', () => {
  const actual = parse(sample('当前用量\n已使用 0%\n重置时间：12:51', '每周限额\n重置时间：10月30日09:51\n已使用 4%'));
  assert.deepEqual(actual, { usage_text: '当前用量\n已使用 0%\n重置时间：12:51', used_percent: 0, reset_at: 1792817460, five_hour_limited: false, weekly_used_percent: 4, weekly_reset_at: 1793325060, weekly_limited: false, checked_at: 1792807840, error: '' });
});

test('五小时用尽与每周用尽分别判断，未识别内容保留未知', () => {
  for (const [current, weekly, five, week] of [
    ['Used 100%', 'Used 17%', true, false],
    ['Used 12%', 'Remaining 0%', false, true],
    ['当前用量\n加载失败', '每周限额\n已使用 100%', null, true],
    ['当前用量\n已使用 0%', null, false, null],
    ['用量已耗尽', 'Weekly quota exhausted', true, true],
  ]) {
    const actual = parse(sample(current, weekly));
    assert.equal(actual?.five_hour_limited, five);
    assert.equal(actual?.weekly_limited, week);
  }
});

test('缺面板、无比例和无恢复提示均不制造可用状态或五小时倒计时', () => {
  for (const data of [{ found: false, checked_at: 1792807840 }, sample('当前用量\n5 小时窗口'), sample('已使用 -125%'), sample('剩余 -1%'), sample('剩余 125%')]) {
    const actual = parse(data);
    assert.equal(actual?.used_percent, null);
    assert.equal(actual?.reset_at, null);
    assert.equal(actual?.five_hour_limited, null);
    assert.ok(actual?.error);
  }
});

test('真实相对恢复提示、英文日期与跨年时钟按面板时区计算', () => {
  const rows = [
    [sample('Used 20%\nResets in 2 hours 15 minutes'), 1792815940],
    [sample('Used 20%\nResets at 12:51 PM'), 1792817460],
    [sample('Used 20%\nResets on Oct 30, 9:51 AM'), 1793325060],
    [sample('已使用 20%\n重置时间：1月1日00:10', null, { checked_at: 1798732200 }), 1798733400],
    [sample('Used 20%\nResets at 02:00', null, { checked_at: 1792807840, time_zone: 'America/Los_Angeles' }), 1792832400],
  ];
  for (const [data, reset] of rows) assert.equal(parse(data)?.reset_at, reset);
});

test('机器时间戳优先于显示提示，部分已确认字段不因另一窗口异常丢失', () => {
  const data = sample('Used 8%\nResets at 12:51', '每周限额\n无法读取');
  data.current.reset_timestamp = 1792821818;
  const actual = parse(data);
  assert.equal(actual?.reset_at, 1792821818);
  assert.equal(actual?.used_percent, 8);
  assert.equal(actual?.weekly_used_percent, null);
  assert.equal(actual?.weekly_limited, null);
  assert.ok(actual?.error);
});

test('真实 jSf9Qc 数据按窗口类型解析精确比例及时间戳，不暴露内部用量计数', () => {
  const rpc = JSON.stringify([['wrb.fr', 'jSf9Qc', JSON.stringify([2, [[47787, 0.01233471, 2, [[1793372618, 283710000]]], [2400, 0, 1, [[1792821818, 283647000]]]], false]), null, null, null, 'generic']]);
  const decoded = quota.decodeQuotaRPC?.(")]}'\n\n" + rpc + '\n');
  assert.deepEqual(decoded, { current: { used_percent: 0, reset_at: 1792821818 }, weekly: { used_percent: 1.233471, reset_at: 1793372618 } });
  const actual = parse({ ...sample('已使用 0%\n重置时间：14:03', '已使用 1%\n重置时间：10月30日23:03'), rpc: decoded });
  assert.equal(actual?.weekly_used_percent, 1.233471);
  assert.equal(actual?.reset_at, 1792821818);
  assert.equal(actual?.weekly_reset_at, 1793372618);
});

test('未知 RPC、变更结构和负数比例不当作真实额度', () => {
  for (const text of ['{}', 'not JSON', JSON.stringify([['wrb.fr', 'sJBwce', '[]']]), JSON.stringify([['wrb.fr', 'jSf9Qc', '[2,[[1,-1,1,[[999,0]]]]]']])]) {
    assert.equal(quota.decodeQuotaRPC?.(text), null);
  }
});

test('累计 USED 超过 100% 保留真实超额并按两个窗口分别判为耗尽', () => {
  for (const [text, used] of [['Used125%', 125], ['已使用 100.5%', 100.5], ['125% used', 125], ['Used 100%', 100], ['Used 0%', 0]]) {
    const actual = parse(sample(text, text));
    assert.equal(actual.used_percent, used);
    assert.equal(actual.weekly_used_percent, used);
    assert.equal(actual.five_hour_limited, used >= 100);
    assert.equal(actual.weekly_limited, used >= 100);
  }
  for (const text of ['Used -1%', 'Used NaN%', 'Used Infinity%', 'Used abc%', 'Used 125%\nUsed 130%']) {
    assert.equal(parse(sample(text)).used_percent, null);
    assert.equal(parse(sample(text)).five_hour_limited, null);
  }
});

test('RPC 累计 USED 比率 1.25 是已用 125% 和耗尽，0 比率仍有全部额度', () => {
  const raw = JSON.stringify([['wrb.fr', 'jSf9Qc', JSON.stringify([2, [[2400, 1.25, 1, [[1792821818, 0]]], [100, 0, 2, [[1793372618, 0]]]], false])]]);
  const decoded = quota.decodeQuotaRPC(raw);
  assert.deepEqual(decoded, { current: { used_percent: 125, reset_at: 1792821818 }, weekly: { used_percent: 0, reset_at: 1793372618 } });
  const actual = parse({ ...sample('已使用 125%', '已使用 0%'), rpc: decoded });
  assert.equal(actual.used_percent, 125);
  assert.equal(actual.five_hour_limited, true);
  assert.equal(actual.weekly_limited, false);
});

test('恢复提示明确 tomorrow 或明天按浏览器明日日期计算，today 保留今天', () => {
  for (const [hint, expected] of [['Resets tomorrow at 12:51', 1792903860], ['重置时间：明天12:51', 1792903860], ['Resets today at 12:51', 1792817460], ['Resets day after tomorrow at 12:51', 1792990260]]) {
    assert.equal(parse(sample(`Used 30%\n${hint}`)).reset_at, expected);
  }
});

test('恢复日期限定词未理解或冲突时不默认为今天，并保留已知 USED', () => {
  for (const hint of ['Resets next Friday at 12:51', 'Resets next week at 12:51', 'Resets next month at 12:51', 'Resets today or tomorrow at 12:51', 'Resets tomorrow on Oct 30 at 12:51', '重置时间：下周五12:51', 'Resets in 2 hours or 3 hours']) {
    const actual = parse(sample(`Used 30%\n${hint}`));
    assert.equal(actual.reset_at, null, hint);
    assert.equal(actual.used_percent, 30);
    assert.equal(actual.five_hour_limited, false);
    assert.ok(actual.error);
  }
});

test('RPC 恢复结构变更不把字符串首位误认为 Unix 时间，保留有效比例', () => {
  const raw = JSON.stringify([['wrb.fr', 'jSf9Qc', JSON.stringify([2, [[2400, 0, 1, '1792821818']], false])]]);
  assert.deepEqual(quota.decodeQuotaRPC(raw), { current: { used_percent: 0, reset_at: null } });
});

test('只读 RPC 使用已观察参数、浏览器 Cookie 和页面 token，结束后清理中断句柄', async () => {
  const seen = [];
  const sandbox = { AbortController, URLSearchParams, setTimeout, clearTimeout, fetch: async (url, options) => {
    seen.push({ url, method: options.method, credentials: options.credentials, rpc: JSON.parse(options.body.get('f.req')), token: options.body.get('at') });
    return { ok: true, text: async () => 'quota-response' };
  } };
  const invoke = runInNewContext(`(${quota.fetchQuotaRPCInPage})`, sandbox);
  assert.equal(await invoke({ token: 'fake-token' }), 'quota-response');
  assert.deepEqual(JSON.parse(JSON.stringify(seen)), [{ url: '/_/BardChatUi/data/batchexecute?rpcids=jSf9Qc&source-path=%2Fusage', method: 'POST', credentials: 'include', rpc: [[['jSf9Qc', '[]', null, 'generic']]], token: 'fake-token' }]);
  assert.equal(sandbox.__worker_abort, undefined);
});

test('RPC 非成功状态、网络失败和取消清理中断句柄', async () => {
  const make = fetch => {
    const sandbox = { AbortController, URLSearchParams, setTimeout, clearTimeout, fetch };
    return { sandbox, invoke: runInNewContext(`(${quota.fetchQuotaRPCInPage})`, sandbox) };
  };
  const badStatus = make(async () => ({ ok: false }));
  assert.equal(await badStatus.invoke({ token: 'fake' }), null);
  assert.equal(badStatus.sandbox.__worker_abort, undefined);
  const network = make(async () => { throw new Error('network offline'); });
  await assert.rejects(network.invoke({ token: 'fake' }), /network offline/);
  assert.equal(network.sandbox.__worker_abort, undefined);
  const cancelled = make(async (_, options) => new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted')))));
  const running = cancelled.invoke({ token: 'fake' });
  cancelled.sandbox.__worker_abort.abort();
  await assert.rejects(running, /aborted/);
  assert.equal(cancelled.sandbox.__worker_abort, undefined);
});

test('百分比冲突、未知语言和不唯一的夏令时恢复时刻保持未知', () => {
  const conflict = parse(sample('Used 100%\nRemaining 25%'));
  assert.equal(conflict.used_percent, null);
  assert.equal(conflict.five_hour_limited, null);
  const unknown = parse(sample('Utilisation de 12%'));
  assert.equal(unknown.used_percent, null);
  assert.equal(unknown.five_hour_limited, null);
  const dst = parse(sample('Used 30%\nResets at 01:30', null, { checked_at: 1793512800, time_zone: 'America/Los_Angeles' }));
  assert.equal(dst.reset_at, null);
  const explicit = sample('Used 30%\nResets at 01:30');
  explicit.current.reset_timestamp = '2026-11-01T01:30:00-08:00';
  assert.equal(parse(explicit).reset_at, 1793525400);
});

test('只读 RPC 无调用方超时时也会有界中断网络等待', async () => {
  const sandbox = { AbortController, URLSearchParams, setTimeout: fn => setTimeout(fn, 1), clearTimeout, fetch: async (_, options) => new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted')))) };
  const invoke = runInNewContext(`(${quota.fetchQuotaRPCInPage})`, sandbox);
  const pending = invoke({ token: 'fake' });
  const outcome = await Promise.race([pending.then(() => 'completed', () => 'aborted'), new Promise(resolve => setTimeout(() => resolve('still-running'), 30))]);
  sandbox.__worker_abort?.abort();
  await pending.catch(() => {});
  assert.equal(outcome, 'aborted');
  assert.equal(sandbox.__worker_abort, undefined);
});

// 浏览器是外部边界；这里使用真正的 Firefox DOM，两个固定夹具独立于生产解析逻辑。
test('真实 React 与 Angular DOM 仅提取用量卡片，聊天和隐藏伪面板不误判', async t => {
  assert.equal(typeof quota.inspectQuotaPanel, 'function');
  const directory = await mkdtemp(join(tmpdir(), 'quota-dom-test-'));
  const identity = await quota.camoufoxFactory.generate({ os: process.platform === 'win32' ? 'windows' : 'linux' });
  const context = await quota.camoufoxFactory.launch({ identity, userDataDir: directory, headless: true, timeout: 30000 });
  t.after(async () => { await context.close(); await rm(directory, { recursive: true, force: true }); });
  const page = await context.newPage();
  for (const [file, percentage] of [['react-panel.json', 4], ['angular-panel.json', 1]]) {
    const [fixture] = JSON.parse(await readFile(new URL(`./fixtures/quota/${file}`, import.meta.url), 'utf8'));
    await page.setContent('<article>聊天：已使用 100% 每周限额 100%</article>' + fixture.html);
    const observed = await page.evaluate(quota.inspectQuotaPanel);
    assert.equal(observed.found, true);
    assert.equal(parse(observed)?.used_percent, 0);
    assert.equal(parse(observed)?.weekly_used_percent, percentage);
    assert.ok(!parse(observed)?.usage_text.includes('聊天'));
  }
  await page.setContent('<article>当前用量 已使用 100% 重置时间：12:51 每周限额 已使用 100%</article>');
  assert.equal((await page.evaluate(quota.inspectQuotaPanel)).found, false);
  await page.setContent('<div data-usage-island-root hidden><div data-testid="current-usage">已使用 100%</div></div>');
  assert.equal((await page.evaluate(quota.inspectQuotaPanel)).found, false);
});
