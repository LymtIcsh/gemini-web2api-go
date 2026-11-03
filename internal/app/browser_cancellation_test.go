package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func awaitBrowserResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("取消或另一账号被背压阻塞")
		return nil
	}
}

type signalledBrowserInput struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (w *signalledBrowserInput) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.WriteCloser.Write(p)
}

// 未读取 stdin 时取消不能杀共享进程；原帧完整写完后补发针对该账号的取消。
func TestBrowserCancelBlockedStdinKeepsOtherAccount(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	input := &signalledBrowserInput{WriteCloser: inW, started: make(chan struct{})}
	rpc := newBrowserRPC(input, outR, nil)
	t.Cleanup(func() { inR.Close(); outW.Close(); rpc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rpc.Call(ctx, browserRequest{AccountID: 1, Op: "request"}, nil) }()
	<-input.started
	cancel()
	if err := awaitBrowserResult(t, done); err != context.Canceled {
		t.Fatalf("取消结果: %v", err)
	}
	select {
	case <-rpc.done:
		t.Fatal("普通取消关闭了共享进程")
	default:
	}
	requests := make(chan browserRequest, 3)
	go func() {
		dec, enc := json.NewDecoder(inR), json.NewEncoder(outW)
		for i := 0; i < 3; i++ {
			var req browserRequest
			if dec.Decode(&req) != nil {
				return
			}
			requests <- req
			if req.AccountID == 2 {
				_ = enc.Encode(browserEvent{ID: req.ID, AccountID: 2, Type: "end"})
			}
		}
	}()
	go func() { done <- rpc.Call(context.Background(), browserRequest{AccountID: 2, Op: "request"}, nil) }()
	if err := awaitBrowserResult(t, done); err != nil {
		t.Fatal(err)
	}
	first := <-requests
	var cancellation browserRequest
	for i := 0; i < 2; i++ {
		if req := <-requests; req.Op == "cancel" {
			cancellation = req
		}
	}
	if first.AccountID != 1 || cancellation.TargetID != first.ID || cancellation.AccountID != 1 {
		t.Fatalf("取消归属丢失: %+v %+v", first, cancellation)
	}
}

// 慢回调协作取消后，pipe 写和 stdout 分发都应解除；回调必须在返回前退出。
func TestBrowserCancelSlowConsumerReleasesOtherAccount(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	rpc := newBrowserRPC(inW, outR, nil)
	t.Cleanup(func() { inR.Close(); outW.Close(); rpc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	started, exited := make(chan struct{}), make(chan struct{})
	go func() {
		dec, enc := json.NewDecoder(inR), json.NewEncoder(outW)
		var first browserRequest
		if dec.Decode(&first) != nil {
			return
		}
		_ = enc.Encode(browserEvent{ID: first.ID, AccountID: 1, Type: "start", Status: 200})
		_ = enc.Encode(browserEvent{ID: first.ID, AccountID: 1, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte("first\n"))})
		<-started
		_ = enc.Encode(browserEvent{ID: first.ID, AccountID: 1, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte("second\n"))})
		_ = enc.Encode(browserEvent{ID: first.ID, AccountID: 1, Type: "end"})
		for {
			var req browserRequest
			if dec.Decode(&req) != nil {
				return
			}
			if req.AccountID == 2 {
				_ = enc.Encode(browserEvent{ID: req.ID, AccountID: 2, Type: "end"})
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := browserExchange(ctx, rpc, browserRequest{AccountID: 1, Op: "request"}, func(string) { close(started); <-ctx.Done(); close(exited) }, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := awaitBrowserResult(t, done); err != context.Canceled {
		t.Fatalf("取消结果: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("返回时消费者仍在运行")
	}
	go func() { done <- rpc.Call(context.Background(), browserRequest{AccountID: 2, Op: "request"}, nil) }()
	if err := awaitBrowserResult(t, done); err != nil {
		t.Fatal(err)
	}
}

// 通用消费函数继续同步等待退出，但其取消不能阻塞 stdout 给其他账号分发。
func TestBrowserCancelBlockedCallbackReleasesStdout(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	rpc := newBrowserRPC(inW, outR, nil)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); inR.Close(); outW.Close(); rpc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	requests := make(chan browserRequest)
	cancelReceived := make(chan error, 1)
	go func() {
		dec := json.NewDecoder(inR)
		for {
			var req browserRequest
			if dec.Decode(&req) != nil {
				return
			}
			if req.Op != "cancel" {
				requests <- req
			} else {
				cancelReceived <- nil
			}
		}
	}()
	go func() {
		first := <-requests
		enc := json.NewEncoder(outW)
		_ = enc.Encode(browserEvent{ID: first.ID, AccountID: 1, Type: "chunk"})
		<-started
		_ = enc.Encode(browserEvent{ID: first.ID, AccountID: 1, Type: "end"})
		second := <-requests
		_ = enc.Encode(browserEvent{ID: second.ID, AccountID: 2, Type: "end"})
	}()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		firstDone <- rpc.Call(ctx, browserRequest{AccountID: 1, Op: "request"}, func(browserEvent) error { close(started); <-release; return nil })
	}()
	<-started
	cancel()
	go func() { secondDone <- rpc.Call(context.Background(), browserRequest{AccountID: 2, Op: "request"}, nil) }()
	if err := awaitBrowserResult(t, secondDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitBrowserResult(t, cancelReceived); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
		t.Fatal("通用消费者未退出就返回")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := awaitBrowserResult(t, firstDone); err != context.Canceled {
		t.Fatalf("取消结果: %v", err)
	}
}

// EOF 必须关闭 stdin，解除正在写入的任务；不能遗留永远等待管道的写任务。
func TestBrowserEOFReleasesBlockedWriter(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	input := &signalledBrowserInput{WriteCloser: inW, started: make(chan struct{})}
	rpc := newBrowserRPC(input, outR, nil)
	t.Cleanup(func() { inR.Close(); rpc.Close() })
	done := make(chan error, 1)
	go func() { done <- rpc.Call(context.Background(), browserRequest{AccountID: 1, Op: "request"}, nil) }()
	<-input.started
	outW.Close()
	if err := awaitBrowserResult(t, done); err == nil {
		t.Fatal("EOF 未失败")
	}
	if _, err := inW.Write([]byte("x")); err == nil {
		t.Fatal("EOF 没有关闭 stdin")
	}
	select {
	case <-rpc.writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("EOF 后写任务仍在运行")
	}
}

// 使用真实 HTTP socket 的 SSE 写背压，取消时立即设置写截止；不增加运行时超时阈值。
func TestBrowserHTTPWriteCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stop := cancelHTTPWrites(ctx, w)
		defer stop()
		w.Header().Set("Content-Type", "text/event-stream")
		close(started)
		_, err := io.WriteString(w, "data: "+strings.Repeat("x", 32*1024*1024)+"\n\n")
		done <- err
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: local\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	select {
	case err := <-done:
		t.Fatalf("未读取客户端时写入未产生背压: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := awaitBrowserResult(t, done); err == nil {
		t.Fatal("取消没有中断 HTTP 写入")
	}
}

// 子请求沿用现有配置超时，其取消也要解除 HTTP 写，而不把整个请求改为统一超时。
func TestBrowserChildCancellationReleasesHTTPWrite(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	rpc := newBrowserRPC(inW, outR, nil)
	t.Cleanup(func() { inR.Close(); outW.Close(); rpc.Close() })
	go func() {
		dec, enc := json.NewDecoder(inR), json.NewEncoder(outW)
		var req browserRequest
		if dec.Decode(&req) != nil {
			return
		}
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: 1, Type: "start", Status: 200})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: 1, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte("first\n"))})
		_ = enc.Encode(browserEvent{ID: req.ID, AccountID: 1, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte("second\n"))})
		for dec.Decode(&req) == nil {
		}
	}()
	started := make(chan struct{})
	childCancel := make(chan context.CancelFunc, 1)
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parent, stop := withHTTPWriteCancellation(r.Context(), w)
		defer stop()
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		childCancel <- cancel
		_, _, _, _, err := browserExchange(ctx, rpc, browserRequest{AccountID: 1, Op: "request"}, func(string) {
			close(started)
			_, _ = io.WriteString(w, "data: "+strings.Repeat("x", 32*1024*1024)+"\n\n")
		}, nil)
		done <- err
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: local\r\n\r\n")
	<-started
	select {
	case err := <-done:
		t.Fatalf("未产生背压: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	(<-childCancel)()
	if err := awaitBrowserResult(t, done); err != context.Canceled {
		t.Fatalf("子请求取消: %v", err)
	}
}
