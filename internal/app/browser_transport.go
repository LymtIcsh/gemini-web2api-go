package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// browserTransport 随一条生成任务传递，账号、档案和出口不会在子请求间丢失。
type browserTransport struct {
	ctx       context.Context
	accountID int64
	profileID string
	email     string
	proxyURL  string
	page      []byte
}

// 浏览器分块写入 pipe，继续使用既有逐行读取；取消、背压和 UTF-8 均保持字节语义。
func browserExchange(ctx context.Context, rpc *browserRPC, req browserRequest, onLine func(string), session func(browserSession) error) (int, map[string]string, []byte, int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopWorker := context.AfterFunc(rpc.ctx, cancel)
	defer stopWorker()
	reader, writer := io.Pipe()
	defer reader.Close()
	pipeCancelled := make(chan struct{})
	stopPipe := context.AfterFunc(ctx, func() {
		defer close(pipeCancelled)
		reader.CloseWithError(ctx.Err())
		writer.CloseWithError(ctx.Err())
		if abort, ok := ctx.Value(httpWriteCancellationKey{}).(func()); ok {
			abort()
		}
	})
	defer func() {
		if !stopPipe() {
			<-pipeCancelled
		}
	}()
	type bodyResult struct {
		body []byte
		ttfb int64
		err  error
	}
	done := make(chan bodyResult, 1)
	start := time.Now()
	go func() {
		var body []byte
		var ttfb int64
		var err error
		if onLine != nil {
			// 回调协作取消；返回前仍等待它退出，避免响应已返回后继续写。
			body, ttfb, err = readBody(reader, func(line string) {
				if ctx.Err() == nil {
					onLine(line)
				}
			}, start)
		} else {
			var firstByte [1]byte
			_, err = io.ReadFull(reader, firstByte[:])
			ttfb = time.Since(start).Milliseconds()
			if err == nil {
				var rest []byte
				rest, err = io.ReadAll(reader)
				body = append(firstByte[:], rest...)
			} else if err == io.EOF {
				err = nil
			}
		}
		reader.Close()
		done <- bodyResult{body, ttfb, err}
	}()
	status := 0
	headers := map[string]string{}
	err := rpc.Call(ctx, req, func(e browserEvent) error {
		switch e.Type {
		case "start":
			status = e.Status
			for k, v := range e.Headers {
				headers[strings.ToLower(k)] = v
			}
		case "chunk":
			data, err := base64.StdEncoding.DecodeString(e.DataBase64)
			if err != nil {
				return fmt.Errorf("浏览器响应块格式错误: %w", err)
			}
			_, err = writer.Write(data)
			return err
		case "end", "error":
			if e.Session != nil && session != nil {
				return session(*e.Session)
			}
		}
		return nil
	})
	writer.CloseWithError(err)
	result := <-done
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		err = result.err
	}
	if err == nil && status == 0 {
		err = fmt.Errorf("浏览器未返回 HTTP 状态")
	}
	return status, headers, result.body, result.ttfb, err
}

func (t *browserTransport) exchange(op, method, rawURL string, headers map[string]string, body []byte, onLine func(string)) (int, map[string]string, []byte, int64, error) {
	if err := t.available(); err != nil {
		return 0, nil, nil, 0, err
	}
	rpc, err := managedBrowserRPC()
	if err != nil {
		return 0, nil, nil, 0, err
	}
	ctx, cancel := browserRequestContext(t.ctx)
	defer cancel()
	req := browserRequest{AccountID: t.accountID, Op: op, ProfileID: t.profileID, ExpectedEmail: t.email, ProxyURL: t.proxyURL,
		TimeoutMs: int64(rtCfg().RequestTimeout) * 1000, URL: rawURL, Method: method, Headers: headers, BodyBase64: base64.StdEncoding.EncodeToString(body)}
	status, responseHeaders, raw, ttfb, err := browserExchange(ctx, rpc, req, onLine, func(s browserSession) error { return storeBrowserSession(t.accountID, s) })
	if loginRequired(err) {
		markBrowserLoginRequired(t.accountID, err.Error())
	}
	return status, responseHeaders, raw, ttfb, err
}

func loginRequired(err error) bool {
	var b *browserError
	return errors.As(err, &b) && b.Code == "login_required"
}

func newBrowserTransport(parent context.Context, a *CookieAccount, proxyURL string) (*browserTransport, error) {
	if a == nil {
		return nil, fmt.Errorf("账号已删除，无法继续请求")
	}
	exists, _, b, err := accountBrowserState(a.ID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("账号已删除，无法继续请求")
	}
	if b == nil {
		return nil, err
	}
	return &browserTransport{ctx: parent, accountID: a.ID, profileID: b.ProfileID, email: b.Email, proxyURL: proxyURL}, nil
}

func (t *browserTransport) available() error {
	_, ready, b, err := accountBrowserState(t.accountID)
	if err != nil {
		return err
	}
	if b == nil || b.ProfileID != t.profileID || !strings.EqualFold(b.Email, t.email) || !ready {
		return fmt.Errorf("账号 #%d 已删除、禁用或登录失效，无法继续请求", t.accountID)
	}
	return nil
}

func browserAccountAction(ctx context.Context, a *CookieAccount, op string) (*browserSession, error) {
	if op == "close" {
		b, err := browserAccountByID(a.ID)
		if err != nil || b == nil {
			return nil, err
		}
		// 关闭只由已运行进程检查档案是否活动，不检查出口或启动浏览器依赖。
		browserManager.Lock()
		rpc := browserManager.rpc
		browserManager.Unlock()
		if rpc == nil {
			return nil, nil
		}
		select {
		case <-rpc.done:
			return nil, nil
		default:
		}
		ctx, cancel := browserRequestContext(ctx)
		defer cancel()
		return nil, rpc.Call(ctx, browserRequest{AccountID: a.ID, Op: op, ProfileID: b.ProfileID}, nil)
	}
	b, err := attachBrowserProfile(a.ID)
	if err != nil {
		return nil, err
	}
	proxyURL, err := accountProxyURL(a)
	if err != nil {
		return nil, err
	}
	rpc, err := managedBrowserRPC()
	if err != nil {
		return nil, err
	}
	ctx, cancel := browserRequestContext(ctx)
	defer cancel()
	var session *browserSession
	err = rpc.Call(ctx, browserRequest{AccountID: a.ID, Op: op, ProfileID: b.ProfileID, ExpectedEmail: b.Email, ProxyURL: proxyURL, TimeoutMs: int64(rtCfg().RequestTimeout) * 1000}, func(e browserEvent) error {
		if e.Session != nil {
			session = e.Session
			return storeBrowserSession(a.ID, *session)
		}
		return nil
	})
	if loginRequired(err) {
		markBrowserLoginRequired(a.ID, err.Error())
	}
	if err == nil && op == "confirm-login" && (session == nil || session.LoginState != "ready") {
		err = fmt.Errorf("浏览器尚未确认登录，请完成 Google 登录后再确认")
	}
	return session, err
}

// 登录浏览器使用账号已绑定的出口；已有绑定不可用时明确报错。
func accountProxyURL(a *CookieAccount) (string, error) {
	if a.ProxyID > 0 {
		for _, p := range listProxies() {
			if p.ID == a.ProxyID && proxyUsableByID(p.ID) {
				return p.URL, nil
			}
		}
		return "", fmt.Errorf("账号绑定的出口不可用，请在账号池中调整出口")
	}
	p, ok, err := acquireSlot(0)
	if !ok {
		return "", err
	}
	defer releaseSlot(p.ID)
	if p.ID > 0 {
		bindAccountProxy(a.ID, p.ID)
	}
	return p.URL, nil
}

// 登录态浏览器保持完整 Cookie；响应参数由页面读取，不对 Cookie 字符串再做模拟轮转。
func (t *browserTransport) appPage(force bool) ([]byte, error) {
	if err := t.available(); err != nil {
		return nil, err
	}
	if !force && t.page != nil {
		return t.page, nil
	}
	status, _, raw, _, err := t.exchange("request", "GET", "https://gemini.google.com/app", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("fetch /app: HTTP %d", status)
	}
	if !snlm0eRe.Match(raw) {
		markBrowserLoginRequired(t.accountID, "Gemini 页面没有登录态")
		return nil, &browserError{Code: "login_required", Detail: "Gemini 页面没有登录态，请重新登录"}
	}
	t.page = raw
	return raw, nil
}

func (t *browserTransport) xsrf() (string, error) {
	body, err := t.appPage(false)
	if err != nil {
		return "", err
	}
	return string(snlm0eRe.FindSubmatch(body)[1]), nil
}

func (t *browserTransport) uploadTokens() (string, string, error) {
	body, err := t.appPage(false)
	if err != nil {
		return "", "", err
	}
	push, pctx := "", ""
	if m := pushIDRe.FindSubmatch(body); m != nil {
		push = string(m[1])
	}
	if m := pctxRe.FindSubmatch(body); m != nil {
		pctx = string(m[1])
	}
	return push, pctx, nil
}

func (t *browserTransport) buildLabel() string {
	if rtCfg().GeminiBLAuto {
		if m := cfb2hRe.FindSubmatch(t.page); m != nil && blValueRe.Match(m[1]) {
			return string(m[1])
		}
	}
	return rtCfg().GeminiBL
}

func transportXSRF(t *browserTransport, cookie, proxyURL string) (string, error) {
	if t != nil {
		return t.xsrf()
	}
	return getXSRF(cookie, proxyURL)
}

func firstBrowserTransport(transports []*browserTransport) *browserTransport {
	if len(transports) > 0 {
		return transports[0]
	}
	return nil
}

func transportUploadTokens(t *browserTransport, cookie, proxyURL string) (string, string, error) {
	if t != nil {
		return t.uploadTokens()
	}
	return getUploadTokens(cookie, proxyURL)
}

func transportBL(t *browserTransport, proxyURL string) string {
	if t != nil {
		return t.buildLabel()
	}
	return currentBL(proxyURL)
}

func doGeminiRequestWithTransport(t *browserTransport, endpoint, body string, headers map[string]string, proxyURL string, onLine func(string)) (int, []byte, int64, []string, error) {
	if t == nil {
		return doGeminiRequest(endpoint, body, headers, proxyURL, onLine)
	}
	status, _, raw, ttfb, err := t.exchange("request", "POST", endpoint, headers, []byte(body), onLine)
	return status, raw, ttfb, nil, err
}

func transportUploadPost(t *browserTransport, rawURL string, headers map[string]string, body []byte, proxyURL string) (int, map[string]string, []byte, error) {
	if t == nil {
		return uploadPost(rawURL, headers, body, proxyURL)
	}
	status, h, raw, _, err := t.exchange("request", "POST", rawURL, headers, body, nil)
	return status, h, raw, err
}

func transportMediaGet(t *browserTransport, rawURL string, headers map[string]string, proxyURL string) (int, map[string]string, []byte, error) {
	if t == nil {
		return mediaGetFollow(rawURL, headers, proxyURL)
	}
	status, h, raw, _, err := t.exchange("download", "GET", rawURL, headers, nil, nil)
	return status, h, raw, err
}

func transportSleep(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		time.Sleep(delay)
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sleepWithTransport(transports []*browserTransport, delay time.Duration) error {
	if t := firstBrowserTransport(transports); t != nil {
		return transportSleep(t.ctx, delay)
	}
	return transportSleep(nil, delay)
}
