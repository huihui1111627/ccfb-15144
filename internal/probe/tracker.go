// Package probe implements continuous active link probing between SD-WAN edge
// devices (store edges and regional hubs). Probes run on every link
// independently; RTT, loss and jitter are maintained as EWMA plus a sliding
// loss window and exposed as model.Quality for the routing engine.
package probe

import (
	"sync"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/network"
)

const (
	alphaRTT   = 0.125 // EWMA 系数
	alphaJit   = 0.1
	lossWindow = 30 // 最近 30 个探测的丢包窗口
)

// Tracker maintains measured quality for one link of one edge.
type Tracker struct {
	mu sync.Mutex

	link    *network.LinkModel
	healthy bool

	ewRTT     float64
	ewJitter  float64
	window    []bool // true = 收到回包
	winHead   int
	winFilled int

	lastProbe time.Time
	probeSeq  uint64
	pending   map[uint64]time.Time // seq -> 发送时刻

	// 最近一次观测的利用率（由传输层更新）
	util float64

	// 每个 tick 缓存一次的质量视图，避免重复调用推进链路模型的 OU 噪声
	cachedAt time.Time
	cached   model.Quality
}

// NewTracker binds a tracker to a link model.
func NewTracker(link *network.LinkModel) *Tracker {
	return &Tracker{
		link:    link,
		window:  make([]bool, lossWindow),
		pending: map[uint64]time.Time{},
		healthy: true,
	}
}

// SetUtil updates the current utilization used when sampling the link.
func (t *Tracker) SetUtil(u float64) {
	t.mu.Lock()
	t.util = u
	t.mu.Unlock()
}

// SendProbe is called every probe interval. Returns the probe sequence
// number; the caller (packet engine) actually puts a probe packet on the wire.
// In fluid mode the same call drives a synthetic probe directly.
func (t *Tracker) SendProbe(now time.Time) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.probeSeq++
	t.pending[t.probeSeq] = now
	t.lastProbe = now
	return t.probeSeq
}

// TimeoutPending marks probes whose round trip has exceeded timeout as lost.
// Only probes older than timeout are considered; probes still in flight stay
// pending so they are not counted as lost prematurely.
func (t *Tracker) TimeoutPending(now time.Time, timeout time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for seq, sent := range t.pending {
		if now.Sub(sent) >= timeout {
			t.recordLocked(false, 0)
			delete(t.pending, seq)
		}
	}
}

// RecvProbeAck processes an echoed probe in packet mode.
func (t *Tracker) RecvProbeAck(seq uint64, sent time.Time, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.pending[seq]; !ok {
		return
	}
	delete(t.pending, seq)
	rtt := float64(now.Sub(sent)) / float64(time.Millisecond)
	t.recordLocked(true, rtt)
}

// SyntheticTick runs one probe round-trip in fluid mode: the link model
// directly determines delivery and draws an RTT sample. The link model is
// sampled exactly once per tick so its Ornstein-Uhlenbeck noise advances at
// the real probe cadence.
func (t *Tracker) SyntheticTick(now time.Time) {
	seq := t.SendProbe(now)
	t.mu.Lock()
	q := t.link.Sample(now, t.util)
	if !q.Healthy {
		t.recordLocked(false, 0)
		delete(t.pending, seq)
		t.recomputeLocked(now, q)
		t.mu.Unlock()
		return
	}
	// 以丢包率做一次伯努利试验（用探测序号做确定性随机种子）
	if bernoulli(q.Loss, seq) {
		t.recordLocked(false, 0)
		delete(t.pending, seq)
		t.recomputeLocked(now, q)
		t.mu.Unlock()
		return
	}
	jitter := q.Jitter
	sample := q.RTT + normSample(seq)*jitter*0.5
	if sample < 1 {
		sample = 1
	}
	t.recordLocked(true, sample)
	delete(t.pending, seq)
	t.recomputeLocked(now, q)
	t.mu.Unlock()
}

func (t *Tracker) recordLocked(ok bool, rtt float64) {
	t.window[t.winHead] = ok
	t.winHead = (t.winHead + 1) % lossWindow
	if t.winFilled < lossWindow {
		t.winFilled++
	}
	if !ok {
		return
	}
	if t.ewRTT == 0 {
		t.ewRTT = rtt
		t.ewJitter = rtt * 0.1
		return
	}
	d := rtt - t.ewRTT
	if d < 0 {
		d = -d
	}
	t.ewJitter = (1-alphaJit)*t.ewJitter + alphaJit*d
	t.ewRTT = (1-alphaRTT)*t.ewRTT + alphaRTT*rtt
}

// recomputeLocked refreshes the cached quality from the current window;
// q is the single link-model sample taken this tick.
func (t *Tracker) recomputeLocked(now time.Time, q model.Quality) {
	loss := 0.0
	if t.winFilled > 0 {
		got := 0
		for i := 0; i < t.winFilled; i++ {
			if t.window[i] {
				got++
			}
		}
		loss = float64(t.winFilled-got) / float64(t.winFilled)
	}
	allLost := t.winFilled == lossWindow && loss >= 0.999
	healthy := t.winFilled < lossWindow || !allLost
	if t.link.Outage() {
		healthy = false
	}
	if t.ewRTT > 0 {
		q.RTT = t.ewRTT
		q.Jitter = t.ewJitter
	}
	q.Loss = loss
	q.Healthy = healthy
	q.Util = t.util
	t.cached = q
	t.cachedAt = now
}

// Quality returns the current measured quality. Within one simulated tick the
// same cached view is returned to every caller; between ticks it is refreshed
// by SyntheticTick (fluid) or RecvProbeAck (packet mode).
func (t *Tracker) Quality() model.Quality {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cachedAt.IsZero() && t.cachedAt.Equal(t.lastProbe) {
		return t.cached
	}
	q := t.link.Sample(t.lastProbe, t.util)
	t.recomputeLocked(t.lastProbe, q)
	return t.cached
}

// Refresh re-samples the link model for packet mode, where probes arrive
// asynchronously rather than on the fluid tick.
func (t *Tracker) Refresh(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	q := t.link.Sample(now, t.util)
	t.recomputeLocked(now, q)
}

// Link exposes the underlying model (engine uses it for enqueue decisions).
func (t *Tracker) Link() *network.LinkModel { return t.link }
