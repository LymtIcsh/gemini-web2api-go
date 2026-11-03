// 只读取已实测的 /usage 用量组件，不在聊天正文或整页数字中搜索额度。
export function inspectQuotaPanel() {
  const visible = element => Boolean(element && element.getClientRects().length && getComputedStyle(element).visibility !== 'hidden');
  const roots = [...document.querySelectorAll('[data-usage-island-root], usage-metrics-window')].filter(visible);
  const checked_at = Math.floor(Date.now() / 1000);
  const time_zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  if (roots.length !== 1) return { found: false, checked_at, time_zone };
  const card = selector => {
    const elements = [...roots[0].querySelectorAll(selector)].filter(visible);
    if (elements.length !== 1) return null;
    const element = elements[0];
    const time = element.querySelector('time[datetime], [data-reset-at]');
    return { text: element.innerText.trim(), reset_timestamp: time?.getAttribute('data-reset-at') || time?.getAttribute('datetime') || null };
  };
  return { found: true, checked_at, time_zone, current: card('[data-testid="current-usage"], [data-test-id="gxu-currently"]'), weekly: card('[data-testid="weekly-usage"], [data-test-id="gxu-weekly"]') };
}

function timestamp(value) {
  if (typeof value === 'string' && /^\d+(?:\.\d+)?$/.test(value)) value = Number(value);
  if (typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T.*(?:Z|[+-]\d{2}:\d{2})$/i.test(value)) value = Date.parse(value) / 1000;
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return null;
  return Math.floor(value > 1e12 ? value / 1000 : value);
}

// 此结构来自实际 jSf9Qc 响应：类型 1 是当前窗口，类型 2 是每周窗口。
export function decodeQuotaRPC(text) {
  let payload;
  for (const line of String(text).split('\n')) {
    if (!line.startsWith('[')) continue;
    try {
      const rows = JSON.parse(line);
      const row = rows.find(item => Array.isArray(item) && item[0] === 'wrb.fr' && item[1] === 'jSf9Qc' && typeof item[2] === 'string');
      if (row) { payload = JSON.parse(row[2]); break; }
    } catch { /* 只接受已确认的完整 RPC 记录。 */ }
  }
  if (!Array.isArray(payload) || !Array.isArray(payload[1])) return null;
  const windows = {};
  for (const row of payload[1]) {
    if (!Array.isArray(row) || row.length < 4 || ![1, 2].includes(row[2])) continue;
    const ratio = row[1];
    if (typeof ratio !== 'number' || !Number.isFinite(ratio) || ratio < 0 || !Number.isFinite(ratio * 100)) continue;
    const key = row[2] === 1 ? 'current' : 'weekly';
    if (windows[key]) return null;
    const reset = Array.isArray(row[3]) && Array.isArray(row[3][0]) && typeof row[3][0][0] === 'number' ? timestamp(row[3][0][0]) : null;
    windows[key] = { used_percent: Number((ratio * 100).toFixed(10)), reset_at: reset };
  }
  return Object.keys(windows).length ? windows : null;
}

// 已确认身份下的只读RPC完整覆盖两个窗口时，直接使用实际比例与服务端恢复时刻。
export function quotaRPCObservation(text) {
  const windows = decodeQuotaRPC(text);
  if (!windows?.current?.reset_at || !windows?.weekly?.reset_at) return null;
  return {
    usage_text: '',
    used_percent: windows.current.used_percent,
    reset_at: windows.current.reset_at,
    five_hour_limited: windows.current.used_percent >= 100,
    weekly_used_percent: windows.weekly.used_percent,
    weekly_reset_at: windows.weekly.reset_at,
    weekly_limited: windows.weekly.used_percent >= 100,
    checked_at: Math.floor(Date.now() / 1000),
    error: '',
  };
}

function percent(text) {
  const values = [];
  const number = '([+-]?\\d+(?:[.,]\\d+)?)\\s*[%％]';
  for (const [pattern, remaining] of [[`(?:已使用|已用|used)\\s*[:：]?\\s*${number}`, false], [`${number}\\s*(?:used|已使用|已用)`, false], [`(?:剩余|剩餘|remaining)\\s*[:：]?\\s*${number}`, true], [`${number}\\s*(?:remaining|剩余|剩餘)`, true]]) {
    for (const match of text.matchAll(new RegExp(pattern, 'gi'))) {
      const value = Number(match[1].replace(',', '.'));
      // 两个窗口记录累计已用；已用超额仍是有效耗尽证据，剩余比例则不能越界。
      if (!Number.isFinite(value) || value < 0 || (remaining && value > 100)) return null;
      values.push(remaining ? 100 - value : value);
    }
  }
  return values.length && values.every(value => value === values[0]) ? values[0] : null;
}

function zonedParts(seconds, zone) {
  const formatter = new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' });
  return Object.fromEntries(formatter.formatToParts(new Date(seconds * 1000)).filter(part => part.type !== 'literal').map(part => [part.type, Number(part.value)]));
}

function zonedEpoch(parts, zone) {
  const target = Date.UTC(parts.year, parts.month - 1, parts.day, parts.hour, parts.minute, 0) / 1000;
  const offsets = new Set();
  for (const delta of [-86400, 0, 86400]) {
    const local = zonedParts(target + delta, zone);
    offsets.add(Date.UTC(local.year, local.month - 1, local.day, local.hour, local.minute, local.second) / 1000 - (target + delta));
  }
  const candidates = [...offsets].map(offset => target - offset).filter(seconds => {
    const local = zonedParts(seconds, zone);
    return ['year', 'month', 'day', 'hour', 'minute'].every(key => local[key] === parts[key]);
  });
  // 夏令时重复或不存在的本地时刻无法唯一确认，保留未知。
  return candidates.length === 1 ? candidates[0] : null;
}

function resetAt(card, checked, zone) {
  const machine = timestamp(card?.reset_timestamp);
  if (machine !== null) return machine;
  const text = card?.text || '';
  const hint = text.split('\n').find(line => /重置|重設|reset/i.test(line));
  if (!hint || !zone) return null;
  const relative = /(?:resets?\s+in|将在|將於|还有|還有|后重置|後重設)/i.test(hint);
  if (relative || /(?:后|後).*?(?:重置|重設)/.test(hint)) {
    let duration = 0;
    const units = /(\d+(?:\.\d+)?)\s*(hours?|hrs?|小时|小時|minutes?|mins?|分钟|分鐘|days?|天|seconds?|secs?|秒)/gi;
    const matches = [...hint.matchAll(units)];
    for (const match of matches) {
      const unit = match[2].toLowerCase();
      duration += Number(match[1]) * (/^(hour|hr|小时|小時)/.test(unit) ? 3600 : /^(minute|min|分钟|分鐘)/.test(unit) ? 60 : /^(day|天)/.test(unit) ? 86400 : 1);
    }
    const remainder = hint.replace(units, '').replace(/^resets?\s+in\s*/i, '').replace(/^(?:将在|將於|还有|還有)\s*/, '').replace(/(?:后|後)?(?:重置|重設)$/, '').replace(/\band\b/gi, '').replace(/[\s,:：]/g, '');
    return matches.length && remainder === '' && Number.isFinite(duration) ? Math.floor(checked + duration) : null;
  }
  try {
    const now = zonedParts(checked, zone);
    const clock = hint.match(/(\d{1,2}):(\d{2})\s*(AM|PM)?/i);
    if (!clock) return null;
    let hour = Number(clock[1]);
    if (clock[3]) { if (hour < 1 || hour > 12) return null; hour = hour % 12 + (clock[3].toUpperCase() === 'PM' ? 12 : 0); }
    if (hour > 23 || Number(clock[2]) > 59) return null;
    const parts = { year: now.year, month: now.month, day: now.day, hour, minute: Number(clock[2]) };
    const chinese = hint.match(/(?:(\d{4})年)?(\d{1,2})月(\d{1,2})日/);
    const english = hint.match(/\b(Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:tember)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\s+(\d{1,2})(?:,?\s+(\d{4}))?/i);
    const relativeDays = [...hint.matchAll(/\b(?:day after tomorrow|tomorrow|today)\b|(?:后天|後天|明天|明日|今天|今日)/gi)];
    if (relativeDays.length > 1 || (relativeDays.length && (chinese || english))) return null;
    // 必须完整理解日期提示；未知日期限定词不能丢弃后只取时钟并补造今天。
    let remainder = hint.replace(clock[0], '');
    for (const recognized of [chinese, english, relativeDays[0]]) {
      if (recognized) remainder = remainder.replace(recognized[0], '');
    }
    remainder = remainder.replace(/^\s*(?:resets?\b|重置(?:时间|時間)?|重設(?:时间|時間)?)/i, '').replace(/\b(?:at|on)\b|于|於/gi, '').replace(/[\s,:：]/g, '');
    if (remainder !== '') return null;
    if (chinese) { parts.year = Number(chinese[1] || now.year); parts.month = Number(chinese[2]); parts.day = Number(chinese[3]); }
    if (english) { parts.month = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'].indexOf(english[1].slice(0, 3).toLowerCase()) + 1; parts.day = Number(english[2]); parts.year = Number(english[3] || now.year); }
    if (relativeDays.length) {
      const label = relativeDays[0][0].toLowerCase();
      const days = /^(?:day after tomorrow|后天|後天)$/.test(label) ? 2 : /^(?:tomorrow|明天|明日)$/.test(label) ? 1 : 0;
      const date = new Date(Date.UTC(now.year, now.month - 1, now.day + days));
      parts.year = date.getUTCFullYear(); parts.month = date.getUTCMonth() + 1; parts.day = date.getUTCDate();
    }
    let result = zonedEpoch(parts, zone);
    if (result !== null && result < checked) {
      if ((chinese && !chinese[1]) || (english && !english[3])) { parts.year++; result = zonedEpoch(parts, zone); }
      else if (!chinese && !english && !relativeDays.length) { const next = new Date(Date.UTC(parts.year, parts.month - 1, parts.day + 1)); parts.year = next.getUTCFullYear(); parts.month = next.getUTCMonth() + 1; parts.day = next.getUTCDate(); result = zonedEpoch(parts, zone); }
    }
    return result;
  } catch { return null; }
}

export function parseQuotaPanel(observation = {}) {
  const checked = Number.isFinite(observation.checked_at) ? Math.floor(observation.checked_at) : Math.floor(Date.now() / 1000);
  const result = { usage_text: '', used_percent: null, reset_at: null, five_hour_limited: null, weekly_used_percent: null, weekly_reset_at: null, weekly_limited: null, checked_at: checked, error: '' };
  if (!observation.found) { result.error = '未找到可识别的用量限额面板'; return result; }
  result.usage_text = observation.current?.text || '';
  const errors = [];
  for (const [key, usedKey, resetKey, limitedKey, name] of [['current', 'used_percent', 'reset_at', 'five_hour_limited', '五小时'], ['weekly', 'weekly_used_percent', 'weekly_reset_at', 'weekly_limited', '每周']]) {
    const card = observation[key];
    const remote = observation.rpc?.[key];
    // 面板确实可见时才应用对应 RPC；隐藏、缺失面板不能因 RPC 推断可用。
    if (card) {
      result[usedKey] = remote?.used_percent ?? percent(card.text || '');
      result[resetKey] = remote?.reset_at ?? resetAt(card, checked, observation.time_zone);
      if (result[usedKey] !== null) result[limitedKey] = result[usedKey] >= 100;
      else if (/(?:^|\n)(?:[^\n]*quota\s+exhausted|(?:weekly\s+)?limit\s+reached|用量已耗尽|用量已用尽|额度已耗尽|已达到(?:每周)?限额)\s*(?:$|\n)/i.test(card.text || '')) result[limitedKey] = true;
    }
    if (result[limitedKey] === null || result[resetKey] === null) errors.push(`${name}用量或恢复时间未完整识别`);
  }
  result.error = errors.join('；');
  return result;
}

// 请求参数已在真实 /usage 的 jSf9Qc 调用中观察并验证；不调用生成或设置接口。
export async function fetchQuotaRPCInPage({ token }) {
  if (!token) return null;
  const controller = new AbortController();
  globalThis.__worker_abort = controller;
  const timer = setTimeout(() => controller.abort(), 10000);
  try {
    const response = await fetch('/_/BardChatUi/data/batchexecute?rpcids=jSf9Qc&source-path=%2Fusage', { method: 'POST', credentials: 'include', signal: controller.signal, headers: { 'content-type': 'application/x-www-form-urlencoded;charset=UTF-8' }, body: new URLSearchParams({ 'f.req': JSON.stringify([[['jSf9Qc', '[]', null, 'generic']]]), at: token }) });
    return response.ok ? await response.text() : null;
  } finally { clearTimeout(timer); delete globalThis.__worker_abort; }
}
