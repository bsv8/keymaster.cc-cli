// Package inprocapi provides a production in-process http.RoundTripper that
// routes apiclient requests to the same Daemon.Handler() without sockets.
//
// Properties:
//   - No net.Listen / net.Dial, no sockets, no environment proxy.
//   - Preserves method, path, query, JSON bodies, status codes and headers.
//   - Streams response bodies and SSE without buffering the whole response
//     (io.Pipe backpressure: handler Write blocks until the caller reads).
//   - Supports http.Flusher, request cancellation, body Close and bounded
//     cleanup. Closing the response body cancels the handler context, which
//     matches net/http server semantics where a disconnected client cancels
//     r.Context() (the daemon SSE loop already watches r.Context().Done()).
//   - A fixed virtual address (http://inproc) is the only accepted URL host;
//     any other host is rejected so requests can never escape to the real
//     network.
//
// Normal completion delivers EOF; only cancellation delivers an error. The
// reader is never closed with an error after the handler finished normally.
package inprocapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// VirtualHost is the only URL host accepted by Transport.
const VirtualHost = "inproc"

// BaseURL is the fixed virtual address used by embedded CLI sessions.
const BaseURL = "http://inproc"

// Transport routes requests to handler in-process.
type Transport struct {
	Handler http.Handler

	closed atomic.Bool
}

// Close marks the transport closed; new RoundTrips fail loudly.
// In-flight requests are unaffected: the pipe still delivers their EOF or
// cancellation error.
func (t *Transport) Close() {
	t.closed.Store(true)
}

func (t *Transport) isClosed() bool {
	if t == nil {
		return true
	}
	return t.closed.Load()
}

type responseHead struct {
	status int
	header http.Header
}

type inprocWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	sentHead    bool
	pw          *io.PipeWriter
	headCh      chan responseHead
}

func (w *inprocWriter) Header() http.Header { return w.header }

func (w *inprocWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	h := make(http.Header, len(w.header))
	for k, v := range w.header {
		h[k] = append([]string(nil), v...)
	}
	if !w.sentHead {
		w.sentHead = true
		w.headCh <- responseHead{status: status, header: h}
	}
}

func (w *inprocWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.pw.Write(b)
}

// Flush satisfies http.Flusher so SSE handlers can stream. The pipe itself
// already provides backpressure; flushing only ensures headers are sent.
func (w *inprocWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

// inprocCtx is a child of the request context that additionally becomes done
// when the caller closes the response body (client-disconnect semantics).
// Parent cancellation propagates via a watcher goroutine, so no shared
// mutable state is touched from callbacks.
type inprocCtx struct {
	parent context.Context
	done   chan struct{}
	once   sync.Once
	err    atomic.Value // error
}

func newInprocCtx(parent context.Context) *inprocCtx {
	if parent == nil {
		parent = context.Background()
	}
	c := &inprocCtx{parent: parent, done: make(chan struct{})}
	go func() {
		select {
		case <-parent.Done():
			c.cancel(parent.Err())
		case <-c.done:
		}
	}()
	return c
}

func (c *inprocCtx) cancel(err error) {
	if err == nil {
		err = context.Canceled
	}
	c.once.Do(func() {
		c.err.Store(err)
		close(c.done)
	})
}

func (c *inprocCtx) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c *inprocCtx) Done() <-chan struct{}       { return c.done }
func (c *inprocCtx) Err() error {
	if v := c.err.Load(); v != nil {
		return v.(error)
	}
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c *inprocCtx) Value(key any) any { return c.parent.Value(key) }

type cancelBody struct {
	*io.PipeReader
	cancel func(error)
	once   sync.Once
}

func (b *cancelBody) Close() error {
	b.once.Do(func() {
		if b.cancel != nil {
			b.cancel(context.Canceled)
		}
	})
	return b.PipeReader.Close()
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.Handler == nil {
		return nil, errors.New("inproc: 缺少后台 handler")
	}
	if t.isClosed() {
		return nil, errors.New("inproc: transport 已关闭")
	}
	if req == nil || req.URL == nil {
		return nil, errors.New("inproc: 请求非法")
	}
	if req.URL.Host != VirtualHost {
		return nil, fmt.Errorf("inproc: 非法主机 %q，拒绝送往真实网络", req.URL.Host)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("inproc: 非法协议 %q", req.URL.Scheme)
	}
	parent := req.Context()
	if parent == nil {
		parent = context.Background()
	}

	ictx := newInprocCtx(parent)
	req2 := req.Clone(ictx)
	// The daemon guard only accepts loopback hosts; the virtual URL host
	// stays in req.URL for routing safety while r.Host carries loopback.
	req2.Host = "127.0.0.1"
	req2.RemoteAddr = "127.0.0.1"
	req2.RequestURI = ""

	pr, pw := io.Pipe()
	headCh := make(chan responseHead, 1)
	rw := &inprocWriter{header: make(http.Header), pw: pw, headCh: headCh}
	done := make(chan struct{})
	// completed marks a normal handler finish. The post-headers watcher
	// below closes the reader with an error only while the handler has
	// not completed, so a normal EOF is never truncated.
	var completed atomic.Bool

	cancel := func(err error) { ictx.cancel(err) }

	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				if !rw.sentHead {
					h := make(http.Header)
					h.Set("Content-Type", "text/plain; charset=utf-8")
					select {
					case headCh <- responseHead{status: http.StatusInternalServerError, header: h}:
						rw.sentHead = true
						rw.wroteHeader = true
						rw.status = http.StatusInternalServerError
					default:
					}
				}
				pw.CloseWithError(fmt.Errorf("inproc: 后台异常: %v", r))
				return
			}
		}()
		t.Handler.ServeHTTP(rw, req2)
		if !rw.sentHead {
			h := make(http.Header, len(rw.header))
			for k, v := range rw.header {
				h[k] = append([]string(nil), v...)
			}
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			select {
			case headCh <- responseHead{status: status, header: h}:
			default:
			}
		}
		// Deliver EOF on normal completion, the context error on
		// cancellation. The pipe carries exactly one terminal signal, so
		// the reader can never see a spurious error after a normal EOF.
		if err := ictx.Err(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		completed.Store(true)
		_ = pw.Close()
	}()

	// watchParent unblocks a post-headers reader when the request context
	// ends (Ctrl-C / timeout). It never fires on the success path: the
	// handler sets completed before closing, and done closes right after.
	watchParent := func() {
		go func() {
			select {
			case <-parent.Done():
				if !completed.Load() {
					_ = pr.CloseWithError(parent.Err())
				}
			case <-done:
			}
		}()
	}

	select {
	case head := <-headCh:
		body := &cancelBody{PipeReader: pr, cancel: cancel}
		watchParent()
		resp := &http.Response{
			Status:     fmt.Sprintf("%d %s", head.status, http.StatusText(head.status)),
			StatusCode: head.status,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     head.header,
			Body:       body,
			// Unknown streaming length; SSE and JSON both work with -1.
			ContentLength: -1,
			Request:       req,
		}
		return resp, nil
	case <-parent.Done():
		// Request cancelled before headers (probe timeout, Ctrl-C).
		// Close the reader so a blocked handler Write fails fast. The
		// handler goroutine still exits on its own via ictx.
		_ = pr.Close()
		cancel(parent.Err())
		return nil, parent.Err()
	case <-done:
		// Handler exited before headers could be read (typically an
		// immediate error). Try to salvage the head it sent.
		select {
		case head := <-headCh:
			body := &cancelBody{PipeReader: pr, cancel: cancel}
			watchParent()
			resp := &http.Response{
				Status:     fmt.Sprintf("%d %s", head.status, http.StatusText(head.status)),
				StatusCode: head.status,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     head.header,
				Body:       body,
				ContentLength: -1,
				Request:       req,
			}
			return resp, nil
		default:
			cancel(context.Canceled)
			_ = pr.Close()
			return nil, errors.New("inproc: 后台未返回响应")
		}
	}
}
