package app

import (
	"crypto/sha256"
	"errors"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// convState 是一路 Gemini 原生会话的状态。整条会话绑定同一个 cookie / 出口，中途不换
// —— 换了 Gemini 就当成另一个会话，续接的 cid 失效。
type convState struct {
	cid, rid, rcid, tok26 string
	turn                  int    // 会话内轮次索引，填 inner[17]，逐轮 +1
	cookie                string // 会话绑定 cookie：登录号的 cookie，或匿名 /app 拿的 session
	sapisid               string // 登录态算 SAPISIDHASH 用；匿名为空
	isLogin               bool
	accountID             int64
	proxyID               int64
	proxyURL              string
	updated               time.Time
}

const (
	// convTTL 跟导出 cookie ~2 小时寿命对齐，过期的会话状态没有复用价值。
	convTTL = 90 * time.Minute
	convMax = 4000
)

var (
	convMu    sync.Mutex
	convStore = map[string]*convState{}
)

// convDelete 作废一路会话（续接失败时用，避免客户端重试还撞同一路死会话）。
func convDelete(key string) {
	if key == "" {
		return
	}
	convMu.Lock()
	delete(convStore, key)
	convMu.Unlock()
}

// convGet 取一路会话，顺带清掉过期的。
func convGet(key string) *convState {
	if key == "" {
		return nil
	}
	convMu.Lock()
	defer convMu.Unlock()
	st, ok := convStore[key]
	if !ok {
		return nil
	}
	if time.Since(st.updated) > convTTL {
		delete(convStore, key)
		return nil
	}
	return st
}

// convPut 存一路会话，超量就按最旧的淘汰一批。
func convPut(key string, st *convState) {
	if key == "" {
		return
	}
	convMu.Lock()
	defer convMu.Unlock()
	st.updated = time.Now()
	convStore[key] = st
	if len(convStore) > convMax {
		// 简单淘汰：删掉过期的；还超就删最旧的一小批。
		var oldestK string
		var oldestT time.Time
		first := true
		for k, v := range convStore {
			if time.Since(v.updated) > convTTL {
				delete(convStore, k)
				continue
			}
			if first || v.updated.Before(oldestT) {
				oldestK, oldestT, first = k, v.updated, false
			}
		}
		if len(convStore) > convMax && oldestK != "" {
			delete(convStore, oldestK)
		}
	}
}

// canonicalMessages 把消息列表压成 role+text 的稳定串，用来算会话指纹。
// 只取 role 和文本内容 —— 客户端每轮重发同样的历史，压出来的串一致才能识别续接。
func canonicalMessages(messages []map[string]interface{}) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(getStr(m, "role"))
		b.WriteByte(0x1f)
		b.WriteString(contentToString(m["content"]))
		b.WriteByte(0x1e)
	}
	return b.String()
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// convParentKey 是"这轮之前的历史"的指纹：除最后一条消息外的全部。
// 命中 store 说明这是某路已知会话的延续，只要发最后一条新消息即可。
func convParentKey(messages []map[string]interface{}) string {
	if len(messages) < 2 {
		return ""
	}
	return hashStr(canonicalMessages(messages[:len(messages)-1]))
}

// convChildKey 是"这轮之后的历史"的指纹：历史 + 本轮模型回复。
// 下一轮请求的 parentKey 会正好等于它（客户端把我们的回复原样带回来），从而续上。
func convChildKey(messages []map[string]interface{}, responseText string) string {
	full := append(append([]map[string]interface{}{}, messages...),
		map[string]interface{}{"role": "assistant", "content": responseText})
	return hashStr(canonicalMessages(full))
}

var rcidRe = regexp.MustCompile(`"(rc_[A-Za-z0-9_-]{6,})"`)

// parseConvIDs 从 StreamGenerate 响应里取续接要用的四样：cid=[1][0]、rid=[1][1]、
// tok26=帧[26]、rcid=正文里的 rc_xxx（登录态续接第 3 位要用它，匿名为空）。
func parseConvIDs(raw string) (cid, rid, rcid, tok26 string) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
			row, ok := it.([]interface{})
			if !ok || len(row) < 3 || row[0] != "wrb.fr" {
				continue
			}
			payload, ok := row[2].(string)
			if !ok {
				continue
			}
			var f []interface{}
			if json.Unmarshal([]byte(payload), &f) != nil {
				continue
			}
			if len(f) > 1 {
				if meta, ok := f[1].([]interface{}); ok && len(meta) >= 2 {
					if s, ok := meta[0].(string); ok && s != "" {
						cid = s
					}
					if s, ok := meta[1].(string); ok && s != "" {
						rid = s
					}
				}
			}
			if len(f) > 26 {
				if s, ok := f[26].(string); ok && s != "" {
					tok26 = s
				}
			}
			// 候选 ID 在已解码的内容帧中，外层 JSON 的转义文本不能直接匹配。
			if len(f) > 4 {
				if candidates, ok := f[4].([]interface{}); ok && len(candidates) > 0 {
					if candidate, ok := candidates[0].([]interface{}); ok && len(candidate) > 0 {
						if id, ok := candidate[0].(string); ok && strings.HasPrefix(id, "rc_") {
							rcid = id
						}
					}
				}
			}
		}
	}
	if m := rcidRe.FindStringSubmatch(raw); m != nil {
		rcid = m[1]
	}
	return
}

// getAnonSession 匿名 GET /app 拿一份 session cookie（NID/COMPASS 等），给匿名多轮当会话
// 载体。续接请求必须每轮带上它，否则被服务端当新会话（实测不带 0/4 通、带 4/4 通）。
// 必须走跟正式请求同一出口。
func getAnonSession(proxyURL string) (string, error) {
	req := func() (*http.Response, io.ReadCloser, []string, error) {
		if proxyURL != "" {
			r, err := http.NewRequest("GET", "https://gemini.google.com/app", nil)
			if err != nil {
				return nil, nil, nil, err
			}
			applyChromeHeaders(r)
			resp, err := getStdlibClient(proxyURL).Do(r)
			if err != nil {
				return nil, nil, nil, err
			}
			return resp, resp.Body, resp.Header.Values("Set-Cookie"), nil
		}
		r, err := fhttp.NewRequest("GET", "https://gemini.google.com/app", nil)
		if err != nil {
			return nil, nil, nil, err
		}
		r.Header.Set("User-Agent", currentBrowserUA())
		resp, err := getTLSClient().Do(r)
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, resp.Body, resp.Header.Values("Set-Cookie"), nil
	}
	_, body, setCookies, err := req()
	if err != nil {
		return "", err
	}
	if body != nil {
		io.Copy(io.Discard, body)
		body.Close()
	}
	var pairs []string
	seen := map[string]bool{}
	for _, sc := range setCookies {
		nv := strings.SplitN(sc, ";", 2)[0]
		name := strings.TrimSpace(strings.SplitN(nv, "=", 2)[0])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		pairs = append(pairs, strings.TrimSpace(nv))
	}
	if len(pairs) == 0 {
		return "", fmt.Errorf("匿名 /app 没返回 Set-Cookie")
	}
	return strings.Join(pairs, "; "), nil
}

// webUA 是匿名 GET /app 的 User-Agent（tls-client 路径要手动带）。
const webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// streamGenerateConv 发一路会话的一轮。turn==0 是首轮（建会话），>0 是续接（只发新消息）。
// 整条会话绑定 conv.cookie / conv.proxyID，中途不换号不换出口。成功后回填 conv 的
// cid/rid/rcid/tok26 和 turn+1，供下一轮续接。
func streamGenerateConv(prompt string, mc ModelConfig, conv *convState,
	onDelta, onReasoning func(string)) (*StreamResult, error) {
	if conv.turn == 0 && conv.accountID == 0 && browserAccountsExist() && !anonFirstEligible(mc, false) {
		return streamBrowserConversation(prompt, mc, conv, onDelta, onReasoning)
	}
	_, ready, b, err := accountBrowserState(conv.accountID)
	if err != nil {
		return &StreamResult{AccountID: conv.accountID}, err
	}
	if conv.isLogin && conv.accountID > 0 && !ready {
		return &StreamResult{AccountID: conv.accountID}, fmt.Errorf("会话账号已删除、禁用或登录失效，无法继续")
	}
	if conv.isLogin && conv.accountID > 0 && !quotaEligible(conv.accountID) {
		return &StreamResult{AccountID: conv.accountID}, quotaAccountError(conv.accountID)
	}
	if b != nil {
		return streamBrowserConversation(prompt, mc, conv, onDelta, onReasoning)
	}
	var p Proxy
	var slotOK bool
	var slotErr error
	if conv.turn > 0 && conv.proxyID > 0 {
		// 续接已有会话：严格沿用原出口，不随意乱跳代理，避免上下文在 Google 端失效
		p, slotOK, slotErr = acquireSlotStrict(conv.proxyID)
	} else {
		p, slotOK, slotErr = acquireSlot(conv.proxyID)
	}
	if !slotOK {
		return &StreamResult{ProxyID: conv.proxyID, AccountID: conv.accountID}, slotErr
	}
	defer releaseSlot(p.ID)
	proxyURL := p.URL
	pickedOK := p.ID > 0

	// 首轮定出口；续接沿用会话原出口（acquireSlot 已优先，但拿不到原出口时只能换，
	// 换了续接大概率失败，靠调用方回退到全量重发兜底）。
	if conv.turn == 0 {
		conv.proxyID = p.ID
		conv.proxyURL = proxyURL
		// 匿名首轮：没有登录 cookie，就地拿一份 session cookie 当会话载体。
		if conv.cookie == "" && !conv.isLogin {
			c, err := getAnonSession(proxyURL)
			if err != nil {
				return &StreamResult{ProxyID: p.ID, ProxyName: p.Name}, fmt.Errorf("建匿名会话失败: %w", err)
			}
			conv.cookie = c
		}
	}

	// 登录态每轮要带 at（XSRF）；匿名不要。
	xsrf := ""
	if conv.isLogin && conv.cookie != "" {
		if tok, err := getXSRF(conv.cookie, proxyURL); err == nil {
			xsrf = tok
		} else {
			return &StreamResult{ProxyID: p.ID, ProxyName: p.Name, AccountID: conv.accountID},
				fmt.Errorf("取 XSRF 失败: %w", err)
		}
	}

	inner := make([]interface{}, innerSlots)
	inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []interface{}{"en"}
	if conv.turn == 0 {
		inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	} else {
		// 续接：[cid, rid, rcid(登录)/""(匿名), null×6, tok26]
		inner[2] = []interface{}{conv.cid, conv.rid, conv.rcid,
			nil, nil, nil, nil, nil, nil, conv.tok26}
	}
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{conv.turn}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	inner[41] = []interface{}{1}
	inner[53] = 0
	reqUUID := uuid.NewString()
	inner[59] = reqUUID
	inner[61] = []interface{}{}
	inner[68] = 1
	inner[79] = mc.Mode
	tw := mc.thinkWire()
	inner[80] = tw
	inner[91] = 0
	inner[96] = 0
	if tw != thinkingNormal {
		inner[96] = 1
	}

	innerJSON, _ := json.Marshal(inner)
	outerJSON, _ := json.Marshal([]interface{}{nil, string(innerJSON)})

	buildBody := func(at string) string {
		form := url.Values{}
		form.Set("f.req", string(outerJSON))
		if at != "" {
			form.Set("at", at)
		}
		return form.Encode()
	}
	body := buildBody(xsrf)

	geminiHeaders := buildGeminiHeaders(conv.cookie, conv.sapisid, mc.HexID)
	geminiHeaders["x-goog-ext-525001261-jspb"] = buildModelHeader(mc.HexID, mc.Mode, mc.thinkWire(), uuid.NewString())
	geminiHeaders["x-goog-ext-525005358-jspb"] = fmt.Sprintf(`["%s",1]`, reqUUID)

	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid)

	tracker := &deltaTracker{}
	rtracker := &deltaTracker{}
	var lineCB func(string)
	if onDelta != nil || onReasoning != nil {
		lineCB = func(line string) {
			if onReasoning != nil {
				if r := reasoningInLine(line); r != "" {
					if d := rtracker.Push(r); d != "" {
						onReasoning(d)
					}
				}
			}
			if onDelta == nil {
				return
			}
			for _, t := range textsInLine(line) {
				if d := tracker.Push(t); d != "" {
					onDelta(d)
				}
			}
		}
	}

	t0 := time.Now()
	var lastErr error
	for attempt := 0; attempt < rtCfg().RetryAttempts; attempt++ {
		statusCode, raw, ttfb, setCookie, err := doGeminiRequest(endpoint, body, geminiHeaders, proxyURL, lineCB)
		if len(setCookie) > 0 && conv.cookie != "" {
			if merged := mergeSetCookie(conv.cookie, setCookie); merged != conv.cookie {
				conv.cookie = merged
			}
		}
		if err != nil || statusCode != 200 || !hasContentFrame(string(raw)) {
			if err == nil && statusCode == 200 {
				lastErr = fmt.Errorf("续接无内容帧（会话可能已失效，raw %d 字节）", len(raw))
			} else if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("upstream HTTP %d: %s", statusCode, truncate(string(raw), 160))
			}
			if pickedOK {
				recordProxyResult(p.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < rtCfg().RetryAttempts-1 {
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		// 成功：回填续接状态。
		cid, rid, rcid, tok26 := parseConvIDs(string(raw))
		if cid != "" {
			conv.cid = cid
		}
		if rid != "" {
			conv.rid = rid
		}
		if rcid != "" {
			conv.rcid = rcid
		}
		conv.tok26 = tok26 // 每轮更新（可能为空）
		conv.turn++
		if pickedOK {
			recordProxyResult(p.ID, true, "")
		}
		return &StreamResult{
			Emitted:          tracker.emitted,
			EmittedReasoning: rtracker.emitted,
			Raw:              string(raw),
			Reasoning:        extractReasoning(string(raw)),
			UpstreamModel:    extractUpstreamModel(string(raw)),
			ProxyID:          p.ID,
			ProxyName:        p.Name,
			AccountID:        conv.accountID,
			TTFBMs:           ttfb,
			TotalMs:          time.Since(t0).Milliseconds(),
		}, nil
	}
	return &StreamResult{ProxyID: p.ID, ProxyName: p.Name, AccountID: conv.accountID}, lastErr
}

type deltaEmitTracker struct {
	emitted bool
	cb      func(string)
}

func (t *deltaEmitTracker) push(s string) {
	if s != "" {
		t.emitted = true
	}
	if t.cb != nil {
		t.cb(s)
	}
}

// callGeminiConv 是多轮开启时的入口：按历史前缀识别续接。
//   - 命中已知会话 → 只发最后一条新消息（user 或 tool 结果），历史留服务端（绕开字节墙）。
//   - 没命中 → 新建会话，首轮发全量拼接的历史（含 tools 指令，把当前上下文交给服务端）。
//   - 会话故障自愈：若当前绑定的账号额度用完或请求异常，且未向客户端吐字，自动切换新账号重构会话。
//
// 支持 tools（续接里也解析 ```tool_call``` 围栏）；图片走原单轮路径（callGemini）。
func callGeminiConv(messages []map[string]interface{}, mc ModelConfig,
	tools []map[string]interface{}, toolChoice interface{},
	onDelta, onReasoning func(string)) (string, []ToolCall, *StreamResult, error) {
	parentKey := convParentKey(messages)
	conv := convGet(parentKey)
	fresh := conv == nil

	dt := &deltaEmitTracker{cb: onDelta}
	rt := &deltaEmitTracker{cb: onReasoning}

	var prompt string
	if fresh {
		conv = &convState{}
		if id := selectedAccountID(); id > 0 {
			mc.PinAccountID = id
		}
		// 有 cookie 池就用登录号（能力更全），否则匿名（首轮就地拿 session cookie）。
		// #20 匿名优先：模型不需要登录态时不占号（多轮已被 gate 限成无图无工具，附件
		// 恒无，所以第二个参数传 false）。必须在调用前短路：pickCookieAccount 会更新
		// last_used_at，调了不用等于白占一轮。
		if mc.PinAccountID > 0 {
			a := accountByID(mc.PinAccountID)
			if !accountReady(a) {
				return "", nil, &StreamResult{AccountID: mc.PinAccountID}, fmt.Errorf("固定账号需要重新登录")
			}
			conv.cookie, conv.sapisid, conv.isLogin, conv.accountID, conv.proxyID = a.Cookie, extractSAPISID(a.Cookie), true, a.ID, a.ProxyID
		} else if !anonFirstEligible(mc, false) && !browserAccountsExist() {
			if a, ok := pickCookieAccount(); ok {
				conv.cookie = a.Cookie
				conv.sapisid = extractSAPISID(a.Cookie)
				conv.isLogin = true
				conv.accountID = a.ID
				conv.proxyID = a.ProxyID
			}
		}
		// 首轮带上 tools 指令：模型要靠它知道怎么吐 tool_call 围栏，续接轮不再重发
		//（指令已在服务端首轮历史里）。
		prompt, _ = messagesToPrompt(messages, tools, toolChoice)
	} else {
		prompt = formatNewTurn(messages[len(messages)-1])
	}
	if fresh {
		logf("[conv] 新会话，首轮发 %d 字节（tools=%d）", len(prompt), len(tools))
	} else {
		logf("[conv] 续接命中 turn=%d，只发 %d 字节（历史留服务端）", conv.turn, len(prompt))
	}

	res, err := streamGenerateConv(prompt, mc, conv, dt.push, rt.push)
	if err == nil {
		text := extractResponseText(res.Raw)
		var toolCalls []ToolCall
		if len(tools) > 0 {
			text, toolCalls = parseToolCalls(text)
		}
		if text != "" || len(toolCalls) > 0 {
			// 存续接状态：客户端下一轮把这段回复原样带回来时，parentKey 会命中这里。
			// 存的是解析掉围栏后的正文 —— 客户端把 assistant 消息带回来时 content 也是这段。
			convPut(convChildKey(messages, text), conv)
			return text, toolCalls, res, nil
		}
		err = fmt.Errorf("upstream returned no content frame (raw %d bytes)", len(res.Raw))
	}

	if !fresh {
		// 续接失败：这路旧会话作废，避免重试还撞同一路死会话。
		// 但注意：如果是出口限流（RateLimitError），会话本身未失效，绝不能删除！
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			convDelete(parentKey)
		}
	}

	// 故障自愈/无感迁移（Failover）：
	// 如果是限流错误（RateLimitError），说明是出口限流暂停，不能注销会话，也不能换账号！
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return "", nil, res, err
	}

	// 如果账号未固定（PinAccountID==0），且尚未向客户端吐出任何字（!dt.emitted && !rt.emitted），
	// 则自动从账号池切换下一个健康且有额度的账号，重构完整历史重新建立会话。
	if !dt.emitted && !rt.emitted && mc.PinAccountID == 0 && (browserAccountsExist() || !anonFirstEligible(mc, false)) {
		tried := map[int64]bool{}
		if conv.accountID > 0 {
			tried[conv.accountID] = true
		}
		logf("[conv] 会话遇到异常 (%v)，未向客户端吐字，触发自动切换新账号重构上下文", err)
		for attempt := 0; attempt < maxCookieTries; attempt++ {
			newAcct, ok := pickCookieAccountExcept(tried)
			if !ok {
				break
			}
			tried[newAcct.ID] = true
			logf("[conv] 会话切换至新账号 #%d 重试", newAcct.ID)
			newConv := &convState{
				cookie:    newAcct.Cookie,
				sapisid:   extractSAPISID(newAcct.Cookie),
				isLogin:   true,
				accountID: newAcct.ID,
				proxyID:   newAcct.ProxyID,
				turn:      0,
			}
			fullPrompt, _ := messagesToPrompt(messages, tools, toolChoice)
			res2, err2 := streamGenerateConv(fullPrompt, mc, newConv, dt.push, rt.push)
			if err2 != nil {
				if dt.emitted || rt.emitted {
					return "", nil, res2, err2
				}
				continue
			}
			t2 := extractResponseText(res2.Raw)
			var tc2 []ToolCall
			if len(tools) > 0 {
				t2, tc2 = parseToolCalls(t2)
			}
			if t2 == "" && len(tc2) == 0 {
				continue
			}
			convPut(convChildKey(messages, t2), newConv)
			return t2, tc2, res2, nil
		}
	}

	return "", nil, res, err
}

// formatNewTurn 把续接要发的那条新消息格式化，跟 messagesToPrompt 里各角色的写法对齐，
// 好让服务端历史里的模型认得出这是工具结果还是用户新话。
func formatNewTurn(msg map[string]interface{}) string {
	content := contentToString(msg["content"])
	if getStr(msg, "role") == "tool" {
		return fmt.Sprintf("[Tool result for %s]: %s", getStr(msg, "name"), content)
	}
	return content
}
