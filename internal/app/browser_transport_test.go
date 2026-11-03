package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 真实 IPC 的字节分片必须在第二片返回前产生第一行回调，并完整保留 UTF-8。
func TestBrowserExchangeStreamsBeforeEnd(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	firstLine := make(chan struct{})
	go func() {
		var req browserRequest
		_ = json.NewDecoder(inputR).Decode(&req)
		enc := json.NewEncoder(outputW)
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "start", Status: 200, Headers: map[string]string{"content-type": "text/plain"}})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte("首行\n"))})
		<-firstLine
		last := []byte("尾行🍎\n")
		for _, part := range [][]byte{last[:4], last[4:]} {
			_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString(part)})
		}
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "end"})
	}()
	var lines []string
	status, _, raw, _, err := browserExchange(context.Background(), rpc, browserRequest{AccountID: 1, Op: "request"}, func(s string) {
		lines = append(lines, s)
		if s == "首行" {
			close(firstLine)
		}
	}, nil)
	if err != nil || status != 200 || string(raw) != "首行\n尾行🍎\n" || len(lines) != 2 || lines[1] != "尾行🍎" {
		t.Fatalf("流式字节受损: %d %q %v %v", status, raw, lines, err)
	}
}

// 下游读取遇到取消时，不能等待上游继续发送内容。
func TestBrowserExchangeReportsCancellation(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		dec := json.NewDecoder(inputR)
		var req browserRequest
		_ = dec.Decode(&req)
		_ = json.NewEncoder(outputW).Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "start", Status: 200})
		cancel()
		_ = dec.Decode(&req)
	}()
	_, _, _, _, err := browserExchange(ctx, rpc, browserRequest{AccountID: 1, Op: "request"}, nil, nil)
	if err != context.Canceled {
		t.Fatalf("取消未传播: %v", err)
	}
}

// 错误页面需要把真实状态返回给 API，不能以浏览器操作完成当成生成成功。
func TestBrowserExchangePreservesFailureStatus(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	go func() {
		var req browserRequest
		_ = json.NewDecoder(inputR).Decode(&req)
		enc := json.NewEncoder(outputW)
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "start", Status: 403})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte("denied"))})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "end"})
	}()
	status, _, body, _, err := browserExchange(context.Background(), rpc, browserRequest{AccountID: 1, Op: "request"}, nil, nil)
	if err != nil || status != 403 || !strings.Contains(string(body), "denied") {
		t.Fatalf("失败被吞: %d %q %v", status, body, err)
	}
}

// 视频是二进制而非按行消息，不能被生成帧 Scanner 的长度上限截断。
func TestBrowserExchangeLargeBinaryHasNoLineLimit(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	data := bytes.Repeat([]byte{0xab}, 9*1024*1024)
	go func() {
		var req browserRequest
		_ = json.NewDecoder(inputR).Decode(&req)
		enc := json.NewEncoder(outputW)
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "start", Status: 200})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString(data)})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: req.AccountID, Type: "end"})
	}()
	_, _, body, _, err := browserExchange(context.Background(), rpc, browserRequest{AccountID: 1, Op: "download"}, nil, nil)
	if err != nil || len(body) != 9*1024*1024 || body[0] != 0xab || body[len(body)-1] != 0xab {
		t.Fatalf("大媒体被截断: %d %v", len(body), err)
	}
}
