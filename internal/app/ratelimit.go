package app

import (
	"sort"
	"sync"
	"time"
)

// IPSlot 标识一个独立 IP（id=0 = 直连主机 IP；id>0 = 代理池里的代理）。
// 每个 slot 独立维护并发数 + 滑动窗口 RPM/RPH 计数。
//
// 限额来自 cfg.PerIP* 字段，默认基于实测 Google 单 IP 容忍度：
//   - 瞬时并发：5（实测 50 并发即时全过，但中长期会触发 sorry）
//   - 每分钟：30（留 2× 余量给突发）
//   - 每小时：80（实测区间 80-180 的下沿，见下）
//
// 单出口能打多少次没有单一数字，主要由**连接策略和出口质量**决定，判据只认
// 302 → /sorry/：并发 10 复用连接池 151/172/177，每次新建连接 106/109（两臂同时
// 起跑、同批出口、全程 80 秒，臂内极差只有 3 和 5）；静态 IP 上 188。
//
// 而**节奏比这些都关键**：10 次/分钟的平缓节奏连打 800 次、跨 110 分钟，一次没被拦。
// 被拦之后是硬拦，106-121 分钟自动恢复。
//
// 所以 hourWin 这个滚动 1 小时窗口是保守假设而不是实测结论——真正的阈值形态更接近
// "突发打满就拦、慢慢打不拦"，而不是"每小时 N 次"。默认取区间下沿，宁可少发。
// 明确按低速率跑的部署可以把 RPH 调高很多。

type ipSlot struct {
	mu        sync.Mutex
	inflight  int
	minuteWin []int64 // 滑动窗口里每个请求的 unix 时间戳（秒）
	hourWin   []int64
}

var (
	slotsMu sync.RWMutex
	slots   = map[int64]*ipSlot{} // proxy_id -> slot；0 = direct
)

func getSlot(proxyID int64) *ipSlot {
	slotsMu.RLock()
	s, ok := slots[proxyID]
	slotsMu.RUnlock()
	if ok {
		return s
	}
	slotsMu.Lock()
	defer slotsMu.Unlock()
	if s, ok := slots[proxyID]; ok {
		return s
	}
	s = &ipSlot{}
	slots[proxyID] = s
	return s
}

// trySlotAcquire 尝试占一个 slot；超额返回 (false, 原因码, 预计需等待秒数)。
// 调用方拿到 true 必须 deferred 调 slotRelease()。
//
// 拒绝类型：
//   - "concurrent" — 已达瞬时并发上限
//   - "rpm"        — 每分钟超限
//   - "rph"        — 每小时超限
func trySlotAcquire(proxyID int64) (bool, string, int64) {
	s := getSlot(proxyID)
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	// 清掉过期窗口（真·滑动时间窗口，不按整点清零）
	cutMin := now - 60
	cutHour := now - 3600
	s.minuteWin = pruneTimestamps(s.minuteWin, cutMin)
	s.hourWin = pruneTimestamps(s.hourWin, cutHour)

	if rtCfg().PerIPConcurrent > 0 && s.inflight >= rtCfg().PerIPConcurrent {
		return false, "concurrent", 1
	}
	if rtCfg().PerIPRPM > 0 && len(s.minuteWin) >= rtCfg().PerIPRPM {
		waitSec := int64(1)
		if len(s.minuteWin) > 0 {
			waitSec = (s.minuteWin[0] + 60) - now
			if waitSec < 1 {
				waitSec = 1
			}
		}
		return false, "rpm", waitSec
	}
	if rtCfg().PerIPRPH > 0 && len(s.hourWin) >= rtCfg().PerIPRPH {
		waitSec := int64(1)
		if len(s.hourWin) > 0 {
			// 滑动窗口最旧的一个请求将在其发生时间 + 3600 秒时滑出窗口，释放 1 个配额
			waitSec = (s.hourWin[0] + 3600) - now
			if waitSec < 1 {
				waitSec = 1
			}
		}
		return false, "rph", waitSec
	}

	// 发送前加锁预扣名额，彻底防止并发漏网突破 80 上限
	s.inflight++
	s.minuteWin = append(s.minuteWin, now)
	s.hourWin = append(s.hourWin, now)
	return true, "", 0
}

// slotRetryAfter 查询某个出口最快还需多少秒可释放下一个配额。
func slotRetryAfter(proxyID int64) int64 {
	s := getSlot(proxyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	s.minuteWin = pruneTimestamps(s.minuteWin, now-60)
	s.hourWin = pruneTimestamps(s.hourWin, now-3600)

	waitSec := int64(1)
	if rtCfg().PerIPRPM > 0 && len(s.minuteWin) >= rtCfg().PerIPRPM && len(s.minuteWin) > 0 {
		w := (s.minuteWin[0] + 60) - now
		if w > waitSec {
			waitSec = w
		}
	}
	if rtCfg().PerIPRPH > 0 && len(s.hourWin) >= rtCfg().PerIPRPH && len(s.hourWin) > 0 {
		w := (s.hourWin[0] + 3600) - now
		if w > waitSec {
			waitSec = w
		}
	}
	if waitSec < 1 {
		return 1
	}
	return waitSec
}

// minProxyRetryAfter 遍历所有启用的代理出口，计算最快解冻的秒数。
func minProxyRetryAfter() int64 {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	minWait := int64(60)
	first := true
	for _, p := range proxyCache {
		if p.Enabled {
			w := slotRetryAfter(p.ID)
			if first || w < minWait {
				minWait = w
				first = false
			}
		}
	}
	if first {
		return slotRetryAfter(0)
	}
	if minWait < 1 {
		return 1
	}
	return minWait
}

// slotRelease 释放并发计数（窗口计数自然过期，不在这里清）。
func slotRelease(proxyID int64) {
	s := getSlot(proxyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight > 0 {
		s.inflight--
	}
}

// SlotUsage admin UI 看用量用。
type SlotUsage struct {
	ProxyID   int64 `json:"proxy_id"`
	Inflight  int   `json:"inflight"`
	RPM       int   `json:"rpm"`
	RPH       int   `json:"rph"`
	LimitConc int   `json:"limit_concurrent"`
	LimitRPM  int   `json:"limit_rpm"`
	LimitRPH  int   `json:"limit_rph"`
}

func slotUsage(proxyID int64) SlotUsage {
	s := getSlot(proxyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	s.minuteWin = pruneTimestamps(s.minuteWin, now-60)
	s.hourWin = pruneTimestamps(s.hourWin, now-3600)
	return SlotUsage{
		ProxyID:   proxyID,
		Inflight:  s.inflight,
		RPM:       len(s.minuteWin),
		RPH:       len(s.hourWin),
		LimitConc: rtCfg().PerIPConcurrent,
		LimitRPM:  rtCfg().PerIPRPM,
		LimitRPH:  rtCfg().PerIPRPH,
	}
}

// allSlotUsage 返回所有 slot 的用量快照（直连 + 全部代理）。
// allSlotUsage 列出当前**可被调度到**的所有 IP slot，包括一次都还没用过的。
//
// 只返回 slots map 里已存在的是不够的：那个 map 是首次用到时才懒创建的，
// 服务刚重启时是空的，面板上「距离封禁红线」会整块空白——而那恰恰是部署时
// 最该盯的一屏。这里按 acquireSlot 的同一套规则枚举：配了代理就只列代理
// （代理存在时不会退回直连），否则列直连。
func allSlotUsage() []SlotUsage {
	seen := map[int64]bool{}
	var ids []int64

	proxyMu.RLock()
	for _, p := range proxyCache {
		if p.Enabled {
			ids = append(ids, p.ID)
			seen[p.ID] = true
		}
	}
	hasProxies := len(ids) > 0
	proxyMu.RUnlock()

	if !hasProxies {
		ids = append(ids, 0)
		seen[0] = true
	}

	// 已有计数但已从池中移除/禁用的 slot 也带上，否则它的用量会凭空消失
	slotsMu.RLock()
	for id := range slots {
		if !seen[id] && (hasProxies == (id != 0)) {
			ids = append(ids, id)
		}
	}
	slotsMu.RUnlock()

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]SlotUsage, 0, len(ids))
	for _, id := range ids {
		out = append(out, slotUsage(id))
	}
	return out
}

func pruneTimestamps(ts []int64, cutoff int64) []int64 {
	// ts 是按时间递增追加的，找第一个 >= cutoff 的位置切掉前面
	i := 0
	for i < len(ts) && ts[i] < cutoff {
		i++
	}
	if i == 0 {
		return ts
	}
	return ts[i:]
}
