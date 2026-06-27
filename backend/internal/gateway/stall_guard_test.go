package gateway

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type idleReader struct{ ctx context.Context }

func (r *idleReader) Read(_ []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *idleReader) Close() error { return nil }

type tickReader struct {
	interval time.Duration
	count    int
	done     int
}

func (r *tickReader) Read(p []byte) (int, error) {
	if r.done >= r.count {
		return 0, io.EOF
	}
	time.Sleep(r.interval)
	r.done++
	if len(p) > 0 {
		p[0] = 'x'
	}
	return 1, nil
}

func (r *tickReader) Close() error { return nil }

type closeOnlyReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *closeOnlyReader) Read(_ []byte) (int, error) {
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *closeOnlyReader) Close() error {
	r.once.Do(func() {
		close(r.closed)
	})
	return nil
}

func TestStallGuardBody_CancelsOnIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	guard := newStallGuardBody(&idleReader{ctx: ctx}, 50*time.Millisecond, cancel)
	defer func() { _ = guard.Close() }()

	go func() {
		buf := make([]byte, 16)
		_, _ = guard.Read(buf)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stall guard did not cancel an idle stream")
	}
}

func TestStallGuardBody_ClosesIdleReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &closeOnlyReader{closed: make(chan struct{})}
	guard := newStallGuardBody(reader, 50*time.Millisecond, cancel)
	defer func() { _ = guard.Close() }()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := guard.Read(buf)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected read to return an error after idle close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stall guard did not close an idle reader")
	}
	if ctx.Err() == nil {
		t.Fatal("stall guard did not cancel context on idle")
	}
}

func TestStallGuardBody_StaysAliveWhileReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	guard := newStallGuardBody(&tickReader{interval: 20 * time.Millisecond, count: 10}, 100*time.Millisecond, cancel)
	defer func() { _ = guard.Close() }()

	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		n, err := guard.Read(buf)
		if err != nil {
			t.Fatalf("read %d returned error: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("read %d returned n=%d, want 1", i, n)
		}
	}

	select {
	case <-ctx.Done():
		t.Fatal("stall guard canceled an active stream")
	default:
	}
}
