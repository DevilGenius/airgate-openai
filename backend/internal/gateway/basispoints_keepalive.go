package gateway

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

const basispointsKeepaliveInterval = 15 * time.Second

type basispointsFrame struct {
	data     []byte
	err      error
	terminal bool
}

// BPS withholds tool arguments until validation, and upstream comments need not
// produce translated events. Keep the downstream alive independently of that
// filtering. Insert whole SSE frames only, never bytes inside a JSON event. The
// caller's parser remains the sole owner of the SDK response writer.
type basispointsKeepaliveBody struct {
	ctx       context.Context
	cancel    context.CancelFunc
	upstream  io.ReadCloser
	frames    chan basispointsFrame
	pending   []byte
	timer     *time.Timer
	interval  time.Duration
	stopClose func() bool
	closeOnce sync.Once
	closeErr  error
	readErr   error
}

func newBasispointsKeepaliveBody(ctx context.Context, upstream io.ReadCloser, interval time.Duration) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	b := &basispointsKeepaliveBody{ctx: ctx, cancel: cancel, upstream: upstream, frames: make(chan basispointsFrame), interval: interval, timer: time.NewTimer(interval)}
	b.stopClose = context.AfterFunc(ctx, func() { _ = b.closeUpstream() })
	go b.readFrames()
	return b
}

func (b *basispointsKeepaliveBody) readFrames() {
	defer close(b.frames)
	send := func(frame basispointsFrame) bool {
		if b.ctx.Err() != nil {
			return false
		}
		select {
		case b.frames <- frame:
			return true
		case <-b.ctx.Done():
			return false
		}
	}
	scanner := bufio.NewScanner(b.upstream)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)
	var frame []byte
	terminal := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("event:")) {
			terminal = isResponsesTerminalEvent(strings.TrimSpace(string(line[len("event:"):])))
		}
		if len(line)+1 > (2*upstreamSSEMaxLineBytes+2)-len(frame) {
			send(basispointsFrame{err: errResponseTooLarge})
			return
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
		if len(line) == 0 {
			if !send(basispointsFrame{data: frame, terminal: terminal}) || terminal {
				return
			}
			frame = nil
		}
	}
	if len(frame) > 0 && !send(basispointsFrame{data: frame}) {
		return
	}
	if err := scanner.Err(); err != nil {
		send(basispointsFrame{err: err})
	}
}

func (b *basispointsKeepaliveBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if err := b.ctx.Err(); err != nil {
			return 0, err
		}
		if b.readErr != nil {
			return 0, b.readErr
		}
		var frame basispointsFrame
		var ok bool
		// Prefer a ready event or EOF over a timer firing at the same instant.
		select {
		case frame, ok = <-b.frames:
		default:
			select {
			case frame, ok = <-b.frames:
			case <-b.timer.C:
				frame, ok = basispointsFrame{data: []byte(basispointsKeepaliveFrame)}, true
			case <-b.ctx.Done():
				return 0, b.ctx.Err()
			}
		}
		if !ok {
			b.readErr = io.EOF
		} else {
			b.pending, b.readErr = frame.data, frame.err
			if frame.terminal {
				b.readErr = io.EOF
			}
		}
		if b.readErr == nil {
			b.timer.Reset(b.interval)
		} else {
			b.timer.Stop()
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *basispointsKeepaliveBody) closeUpstream() error {
	b.closeOnce.Do(func() { b.closeErr = b.upstream.Close() })
	return b.closeErr
}

func (b *basispointsKeepaliveBody) Close() error {
	b.cancel()
	b.timer.Stop()
	b.stopClose()
	return b.closeUpstream()
}
