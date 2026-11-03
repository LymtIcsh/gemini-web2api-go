package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type browserRequest struct {
	ID            string            `json:"id"`
	AccountID     int64             `json:"account_id"`
	Op            string            `json:"op"`
	ProfileID     string            `json:"profile_id,omitempty"`
	ExpectedEmail string            `json:"expected_email,omitempty"`
	ProxyURL      string            `json:"proxy_url,omitempty"`
	TimeoutMs     int64             `json:"timeout_ms,omitempty"`
	URL           string            `json:"url,omitempty"`
	Method        string            `json:"method,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	BodyBase64    string            `json:"body_base64,omitempty"`
	TargetID      string            `json:"target_id,omitempty"`
}

type browserEvent struct {
	ID         string            `json:"id"`
	AccountID  int64             `json:"account_id"`
	Type       string            `json:"type"`
	Status     int               `json:"status,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	DataBase64 string            `json:"data_base64,omitempty"`
	Session    *browserSession   `json:"session,omitempty"`
	Quota      *quotaObservation `json:"quota,omitempty"`
	Code       string            `json:"code,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type browserError struct{ Code, Detail string }

func (e *browserError) Error() string { return e.Code + ": " + e.Detail }

type browserPending struct {
	accountID int64
	events    chan browserEvent
	done      chan struct{}
	once      sync.Once
}

type browserWrite struct {
	ctx        context.Context
	req        browserRequest
	done       chan error
	cancelOnce sync.Once
}

// browserRPC 用请求 ID 和账号 ID 双重归属事件，并让读管道等待消费以保留背压。
type browserRPC struct {
	input         io.WriteCloser
	output        io.ReadCloser
	stop          func()
	writes        chan *browserWrite
	wakeCancel    chan struct{}
	cancellations map[string]browserRequest
	ctx           context.Context
	cancel        context.CancelFunc
	writerDone    chan struct{}
	mu            sync.Mutex
	pending       map[string]*browserPending
	done          <-chan struct{}
	err           error
	once          sync.Once
}

func newBrowserRPC(input io.WriteCloser, output io.ReadCloser, stop func()) *browserRPC {
	ctx, cancel := context.WithCancel(context.Background())
	r := &browserRPC{input: input, output: output, stop: stop, pending: map[string]*browserPending{},
		writes: make(chan *browserWrite), wakeCancel: make(chan struct{}, 1), cancellations: map[string]browserRequest{},
		ctx: ctx, cancel: cancel, done: ctx.Done(), writerDone: make(chan struct{})}
	go r.readEvents()
	go r.writeRequests()
	return r
}

func (r *browserRPC) readEvents() {
	d := json.NewDecoder(r.output)
	for {
		var e browserEvent
		if err := d.Decode(&e); err != nil {
			r.fail(fmt.Errorf("浏览器工作进程断开: %w", err))
			return
		}
		r.mu.Lock()
		p := r.pending[e.ID]
		r.mu.Unlock()
		if p == nil {
			continue
		}
		if p.accountID != e.AccountID {
			e = browserEvent{ID: e.ID, AccountID: p.accountID, Type: "error", Code: "account_mismatch", Error: "浏览器事件账号与请求不一致"}
		}
		select {
		case p.events <- e:
		case <-p.done:
		case <-r.done:
			return
		}
	}
}

func (r *browserRPC) fail(err error) {
	failed := false
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.cancellations = nil
		r.mu.Unlock()
		r.cancel()
		// EOF/关闭拥有共享管道的生命周期，解除未完成的整帧写入。
		r.input.Close()
		r.output.Close()
		failed = true
	})
	// 获胜者在 once 完成后释放进程，允许清理回调重入 Close，避免递归死锁。
	if failed && r.stop != nil {
		r.stop()
	}
}

func (r *browserRPC) writeFrame(req browserRequest) error {
	if err := json.NewEncoder(r.input).Encode(req); err != nil {
		r.fail(err)
		return err
	}
	return nil
}

func (r *browserRPC) requestCancel(w *browserWrite) {
	w.cancelOnce.Do(func() {
		r.mu.Lock()
		if r.cancellations != nil {
			r.cancellations[w.req.ID] = browserRequest{ID: uuid.NewString(), AccountID: w.req.AccountID, Op: "cancel", TargetID: w.req.ID}
		}
		r.mu.Unlock()
		select {
		case r.wakeCancel <- struct{}{}:
		default:
		}
	})
}

// 一个写任务归工作进程所有。已开始的 JSON 帧完整写完，取消不截断其他账号的协议。
func (r *browserRPC) writeRequests() {
	defer close(r.writerDone)
	for {
		r.mu.Lock()
		var cancellation browserRequest
		for id, req := range r.cancellations {
			cancellation = req
			delete(r.cancellations, id)
			break
		}
		r.mu.Unlock()
		if cancellation.ID != "" {
			if r.writeFrame(cancellation) != nil {
				return
			}
			continue
		}
		select {
		case <-r.done:
			return
		case <-r.wakeCancel:
		case w := <-r.writes:
			if err := w.ctx.Err(); err != nil {
				w.done <- err
				continue
			}
			err := r.writeFrame(w.req)
			w.done <- err
			if err != nil {
				return
			}
			if w.ctx.Err() != nil {
				r.requestCancel(w)
			}
		}
	}
}

func (r *browserRPC) Call(ctx context.Context, req browserRequest, consume func(browserEvent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req.ID = uuid.NewString()
	p := &browserPending{accountID: req.AccountID, events: make(chan browserEvent), done: make(chan struct{})}
	finish := func() { p.once.Do(func() { close(p.done) }) }
	stopCancel := context.AfterFunc(ctx, finish)
	r.mu.Lock()
	r.pending[req.ID] = p
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, req.ID)
		r.mu.Unlock()
		stopCancel()
		finish()
	}()
	select {
	case <-r.done:
		return r.failure()
	default:
	}
	w := &browserWrite{ctx: ctx, req: req, done: make(chan error, 1)}
	select {
	case r.writes <- w:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return r.failure()
	}
	stopRequestCancel := context.AfterFunc(ctx, func() { r.requestCancel(w) })
	defer stopRequestCancel()
	select {
	case err := <-w.done:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		r.requestCancel(w)
		return ctx.Err()
	case <-r.done:
		return r.failure()
	}
	for {
		select {
		case <-ctx.Done():
			r.requestCancel(w)
			return ctx.Err()
		case <-r.done:
			return r.failure()
		case e := <-p.events:
			if err := ctx.Err(); err != nil {
				r.requestCancel(w)
				return err
			}
			if e.Type == "error" {
				if e.Session != nil && consume != nil {
					if err := consume(e); err != nil {
						return err
					}
				}
				return &browserError{Code: e.Code, Detail: e.Error}
			}
			if consume != nil {
				if err := consume(e); err != nil {
					r.requestCancel(w)
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				r.requestCancel(w)
				return err
			}
			if e.Type == "end" {
				return nil
			}
		}
	}
}

func (r *browserRPC) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *browserRPC) Close() {
	r.fail(errors.New("浏览器工作进程已关闭"))
}

func browserRequestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout := rtCfg().RequestTimeout; timeout > 0 {
		return context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	}
	return context.WithCancel(parent)
}

var browserManager struct {
	sync.Mutex
	rpc *browserRPC
}

// 工作进程只在浏览器账号需要操作时启动，旧 Cookie 账号可继续使用原传输。
func managedBrowserRPC() (*browserRPC, error) {
	browserManager.Lock()
	defer browserManager.Unlock()
	if browserManager.rpc != nil {
		select {
		case <-browserManager.rpc.done:
			browserManager.rpc.Close()
			browserManager.rpc = nil
		default:
			return browserManager.rpc, nil
		}
	}
	node := os.Getenv("BROWSER_NODE")
	if node == "" {
		node = "node"
	}
	path := os.Getenv("BROWSER_WORKER_PATH")
	if path == "" {
		path = filepath.Join("browser-worker", "worker.mjs")
		if _, err := os.Stat(path); err != nil {
			exe, _ := os.Executable()
			path = filepath.Join(filepath.Dir(exe), "browser-worker", "worker.mjs")
		}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("浏览器工作进程缺失，请安装 browser-worker: %w", err)
	}
	cmd := exec.Command(node, path)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if os.Getenv("BROWSER_PROFILE_ROOT") == "" {
		root, err := filepath.Abs(filepath.Join(filepath.Dir(cfg.DBPath), "browser-profiles"))
		if err != nil {
			return nil, err
		}
		cmd.Env = append(cmd.Env, "BROWSER_PROFILE_ROOT="+root)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	stop, err := startBrowserProcess(cmd)
	if err != nil {
		in.Close()
		out.Close()
		return nil, fmt.Errorf("浏览器工作进程启动失败: %w", err)
	}
	r := newBrowserRPC(in, out, stop)
	browserManager.rpc = r
	go func() { r.fail(fmt.Errorf("浏览器工作进程退出: %v", cmd.Wait())) }()
	return r, nil
}

func closeBrowserWorker() {
	browserManager.Lock()
	r := browserManager.rpc
	browserManager.rpc = nil
	browserManager.Unlock()
	if r != nil {
		ctx, cancel := browserRequestContext(context.Background())
		defer cancel()
		_ = r.Call(ctx, browserRequest{Op: "shutdown"}, nil)
		r.Close()
	}
}
