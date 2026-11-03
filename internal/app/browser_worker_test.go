package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 协议在真实管道里乱序返回时，必须按请求和账号同时匹配，不能串号。
func TestBrowserRPCMultiplexesAndChecksAccount(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	go func() {
		dec := json.NewDecoder(inputR)
		enc := json.NewEncoder(outputW)
		var requests [2]browserRequest
		for i := range requests {
			if dec.Decode(&requests[i]) != nil {
				return
			}
		}
		for i := 1; i >= 0; i-- {
			r := requests[i]
			_ = enc.Encode(browserEvent{ID: r.ID, AccountID: r.AccountID, Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte(r.URL))})
			_ = enc.Encode(browserEvent{ID: r.ID, AccountID: r.AccountID, Type: "end"})
		}
	}()
	var wg sync.WaitGroup
	for id := int64(1); id <= 2; id++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			want := "request-" + strings.Repeat("x", int(id))
			got := ""
			err := rpc.Call(context.Background(), browserRequest{AccountID: id, Op: "request", URL: want}, func(e browserEvent) error {
				b, err := base64.StdEncoding.DecodeString(e.DataBase64)
				if e.Type == "chunk" {
					got += string(b)
				}
				return err
			})
			if err != nil || got != want {
				t.Errorf("请求串号: %q != %q, %v", got, want, err)
			}
		}(id)
	}
	wg.Wait()
}

// 下游取消要向工作进程发送目标 ID，不能让已经取消的浏览器请求继续生成。
func TestBrowserRPCCancelSendsTarget(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan browserRequest, 1)
	go func() {
		dec := json.NewDecoder(inputR)
		var first, second browserRequest
		if dec.Decode(&first) != nil {
			return
		}
		cancel()
		if dec.Decode(&second) == nil && second.TargetID == first.ID {
			got <- second
		}
	}()
	err := rpc.Call(ctx, browserRequest{AccountID: 7, Op: "request"}, nil)
	if err != context.Canceled {
		t.Fatalf("取消结果: %v", err)
	}
	c := <-got
	if c.Op != "cancel" || c.AccountID != 7 {
		t.Fatalf("取消了错误账号: %+v", c)
	}
}

// 进程退出必须终止等待，避免后台工作进程崩溃后所有 API 请求永久挂起。
func TestBrowserRPCWorkerExitFailsPending(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); rpc.Close() })
	go func() {
		var req browserRequest
		_ = json.NewDecoder(inputR).Decode(&req)
		outputW.Close()
	}()
	if err := rpc.Call(context.Background(), browserRequest{AccountID: 1, Op: "refresh"}, nil); err == nil {
		t.Fatal("进程退出未报告错误")
	}
}

func TestBrowserRPCRejectsWrongAccount(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	go func() {
		var req browserRequest
		_ = json.NewDecoder(inputR).Decode(&req)
		_ = json.NewEncoder(outputW).Encode(browserEvent{ID: req.ID, AccountID: 99, Type: "end"})
	}()
	if err := rpc.Call(context.Background(), browserRequest{AccountID: 1, Op: "refresh"}, nil); err == nil {
		t.Fatal("错误账号事件被接受")
	}
}

// EOF 必须释放进程一次；清理回调重入 Close 与并发故障不能死锁或重复释放。
func TestBrowserRPCFailureStopsOnce(t *testing.T) {
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	var stops atomic.Int32
	stopped := make(chan struct{})
	var rpc *browserRPC
	rpc = newBrowserRPC(inputW, outputR, func() {
		if stops.Add(1) == 1 {
			rpc.Close()
			close(stopped)
		}
	})
	t.Cleanup(func() { inputR.Close(); outputW.Close(); rpc.Close() })
	outputW.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("EOF 未完成进程清理，或清理回调重入发生死锁")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rpc.fail(io.ErrUnexpectedEOF)
			rpc.Close()
		}()
	}
	wg.Wait()
	if got := stops.Load(); got != 1 {
		t.Fatalf("进程清理执行了%d次，期望1次", got)
	}
}
