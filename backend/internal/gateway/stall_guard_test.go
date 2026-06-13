package gateway

import (
	"context"
	"io"
	"testing"
	"time"
)

// idleReader 的 Read 阻塞直到 ctx 取消，模拟上游持续静默。
type idleReader struct{ ctx context.Context }

func (r *idleReader) Read(p []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r *idleReader) Close() error { return nil }

// tickReader 每 interval 返回 1 字节、共 count 次，之后 EOF，模拟持续输出的流。
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

// TestStallGuardBody_CancelsOnIdle 回归：连续静默超过 idle 时，读空闲守卫取消上游 ctx。
func TestStallGuardBody_CancelsOnIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newStallGuardBody(&idleReader{ctx: ctx}, 50*time.Millisecond, cancel)
	defer func() { _ = guard.Close() }()

	go func() {
		buf := make([]byte, 16)
		_, _ = guard.Read(buf) // 阻塞，待 watchdog 取消后返回
	}()

	select {
	case <-ctx.Done():
		// 读空闲守卫按预期触发取消
	case <-time.After(2 * time.Second):
		t.Fatal("读空闲守卫未在 idle 后触发取消")
	}
}

// TestStallGuardBody_StaysAliveWhileReading 回归：只要持续读到字节（总时长超过 idle），
// 也不应被取消——活跃长流不掐断。
func TestStallGuardBody_StaysAliveWhileReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newStallGuardBody(&tickReader{interval: 20 * time.Millisecond, count: 10}, 100*time.Millisecond, cancel)
	defer func() { _ = guard.Close() }()

	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		n, err := guard.Read(buf)
		if err != nil {
			t.Fatalf("第 %d 次读取意外出错: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("第 %d 次读取 n=%d, want 1", i, n)
		}
	}
	select {
	case <-ctx.Done():
		t.Fatal("活跃读取期间被误取消")
	default:
	}
}
