package app

import "fmt"

// 浏览器会话继续复用现有生成 payload，账号固定优先于面板当前选号。
func streamBrowserConversation(prompt string, mc ModelConfig, conv *convState, onDelta, onReasoning func(string)) (*StreamResult, error) {
	mc.PinAccountID = conv.accountID
	mc.KeepConversation = true
	if conv.turn > 0 {
		mc.Cont = &ConvRef{CID: conv.cid, RID: conv.rid, RCID: conv.rcid, Tok26: conv.tok26, Turn: conv.turn}
	}
	res, err := streamGenerateWithFiles(prompt, prompt, mc, nil, onDelta, onReasoning)
	if err != nil {
		return res, err
	}
	cid, rid, rcid, tok := parseConvIDs(res.Raw)
	if cid == "" {
		return res, fmt.Errorf("浏览器响应没有会话标识，无法续接")
	}
	conv.cid, conv.rid, conv.rcid, conv.tok26 = cid, rid, rcid, tok
	conv.turn++
	conv.accountID = res.AccountID
	conv.isLogin = res.AccountID > 0
	conv.proxyID = res.ProxyID
	if a := accountByID(conv.accountID); a != nil {
		conv.cookie, conv.sapisid = a.Cookie, extractSAPISID(a.Cookie)
	}
	return res, nil
}
