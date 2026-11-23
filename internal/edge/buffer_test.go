package edge

import (
	"testing"
	"time"

	"sdwan/internal/model"
)

func TestReorderWindowExpires(t *testing.T) {
	r := newReorderBuffer(250 * time.Millisecond)
	k := model.FlowKey{Proto: "tcp", Dst: model.Endpoint{IP: "10.8.0.11", Port: 443}}
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	r.Arm(k, t0)
	if !r.Buffered(k, t0.Add(100*time.Millisecond)) {
		t.Fatal("flow should be buffered inside reorder window")
	}
	if r.Buffered(k, t0.Add(300*time.Millisecond)) {
		t.Fatal("reorder window must expire")
	}
}

func TestDeduperSlidingWindow(t *testing.T) {
	d := newDeduper(64)
	k := model.FlowKey{Proto: "udp", Dst: model.Endpoint{IP: "10.9.0.20", Port: 5004}}
	// 窗口内同一序号只放行一次
	if !d.First(k, 1) || d.First(k, 1) {
		t.Fatal("duplicate within window must be suppressed")
	}
	if !d.First(k, 2) || !d.First(k, 3) {
		t.Fatal("new sequences must pass")
	}
	// 推进超过窗口后旧序号可重新通过（窗口滑动）
	if !d.First(k, 200) {
		t.Fatal("far-future sequence must pass")
	}
}

func TestBulkShaperTokenBucket(t *testing.T) {
	s := newBulkShaper(8_000) // 1000 字节/tick
	s.refill()
	if got := s.take(400); got != 400 {
		t.Fatalf("take 400 = %d", got)
	}
	if got := s.take(1000); got != 600 {
		t.Fatalf("remaining take = %d, want 600", got)
	}
	if s.Remaining() != 0 {
		t.Fatal("shaper should be exhausted")
	}
	s.refill()
	if s.Remaining() != 1000 {
		t.Fatalf("refill remaining = %d", s.Remaining())
	}
}
