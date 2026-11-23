package edge

import (
	"time"

	"sdwan/internal/model"
)

// deduper suppresses duplicate packets arriving from dual-link replication,
// keyed by flow + sequence number. It keeps a bounded bitmap-style window of
// the most recent sequences per flow.
type deduper struct {
	cap  int
	seen map[model.FlowKey]*dedupWindow
}

type dedupWindow struct {
	base uint64 // 窗口最早序号
	bits []uint64
}

func newDeduper(perFlowCap int) *deduper {
	return &deduper{cap: perFlowCap, seen: map[model.FlowKey]*dedupWindow{}}
}

// First returns true if this is the first copy of (flow, seq); the second
// copy arriving on the replica link is dropped.
func (d *deduper) First(k model.FlowKey, seq uint64) bool {
	w, ok := d.seen[k]
	if !ok {
		w = &dedupWindow{base: seq, bits: make([]uint64, (d.cap+63)/64)}
		d.seen[k] = w
	}
	if seq < w.base {
		// 旧窗口之外的迟到包，按新包处理（流已推进）
		return true
	}
	off := seq - w.base
	if off >= uint64(d.cap) {
		// 滑动窗口
		newBase := seq - uint64(d.cap)/2
		shift := newBase - w.base
		w.base = newBase
		if shift >= uint64(len(w.bits)*64) {
			for i := range w.bits {
				w.bits[i] = 0
			}
		} else {
			bitShift := shift
			wordShift := bitShift / 64
			bitShift %= 64
			n := len(w.bits)
			for i := 0; i < n; i++ {
				v := uint64(0)
				if i+int(wordShift) < n {
					v = w.bits[i+int(wordShift)] >> bitShift
				}
				w.bits[i] = v
			}
		}
		off = seq - w.base
	}
	word, bit := off/64, off%64
	mask := uint64(1) << bit
	if w.bits[word]&mask != 0 {
		return false
	}
	w.bits[word] |= mask
	return true
}

// DropFlow frees dedup state when a flow ends.
func (d *deduper) DropFlow(k model.FlowKey) { delete(d.seen, k) }

// reorderBuffer holds packets of a migrated non-realtime flow for the
// migration window so an older link's in-flight packets can be delivered in
// sequence before the new link's stream starts.
type reorderBuffer struct {
	window time.Duration
	// flow -> 迁移起点时间；过期后直接放行
	until map[model.FlowKey]time.Time
}

func newReorderBuffer(w time.Duration) *reorderBuffer {
	return &reorderBuffer{window: w, until: map[model.FlowKey]time.Time{}}
}

// Arm starts a reorder window for a flow.
func (r *reorderBuffer) Arm(k model.FlowKey, now time.Time) {
	r.until[k] = now.Add(r.window)
}

// Buffered reports whether packets of this flow should currently be held for
// ordering. The packet engine uses it to delay new-link packets until the
// window expires; fluid mode only records the metric.
func (r *reorderBuffer) Buffered(k model.FlowKey, now time.Time) bool {
	u, ok := r.until[k]
	if !ok {
		return false
	}
	if now.After(u) {
		delete(r.until, k)
		return false
	}
	return true
}

// bulkShaper is an edge-global token bucket limiting bulk traffic so backups
// and updates can never starve interactive apps, even on the cheap link.
type bulkShaper struct {
	capBps float64
	// 当前 tick 剩余可发字节
	quota int64
}

func newBulkShaper(capBps float64) *bulkShaper {
	return &bulkShaper{capBps: capBps, quota: int64(capBps / 8)}
}

func (s *bulkShaper) setCap(capBps float64) {
	s.capBps = capBps
}

// refill is called once per simulated second.
func (s *bulkShaper) refill() {
	s.quota = int64(s.capBps / 8)
}

// take reserves n bytes for a bulk flow; returns the amount actually allowed
// (0 means the flow is throttled this tick).
func (s *bulkShaper) take(n int64) int64 {
	if n <= s.quota {
		s.quota -= n
		return n
	}
	allowed := s.quota
	s.quota = 0
	return allowed
}

// Remaining reports this tick's remaining bulk bytes.
func (s *bulkShaper) Remaining() int64 { return s.quota }
