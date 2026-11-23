package network

import (
	"sync"
	"time"

	"sdwan/internal/model"
)

// Packet is one simulated IP packet (or a coarse packet-batch unit in
// realtime demo mode).
type Packet struct {
	Flow    model.FlowKey
	App     model.AppID // 由边缘分类器填入
	Seq     uint64      // 流内序号，用于乱序重排与去重
	Bytes   int
	Proto   string
	Payload []byte // 仅首包携带用于签名识别
	First   bool
	Probe   bool // 链路探测包
	ProbeID uint64
	SentAt  time.Time // 发送时刻（虚拟时间）
	Link    model.LinkType
	StoreID string
	ToHub   bool
	// 复制发送时标记副本，接收端据此去重
	Replica bool
}

// EgressQueue is one link's bounded FIFO with tail drop. In packet mode the
// edge enqueues packets here; the engine drains them at link rate.
type EgressQueue struct {
	mu       sync.Mutex
	link     *LinkModel
	pkts     []*Packet
	maxBytes int
	bytes    int
	dropped  int64
	enqueued int64
}

// NewEgressQueue builds a queue sized to ~150 ms of link capacity.
func NewEgressQueue(link *LinkModel) *EgressQueue {
	capBps := link.Config().UpBps
	maxBytes := int(capBps * 0.15 / 8)
	if maxBytes < 64_000 {
		maxBytes = 64_000
	}
	return &EgressQueue{link: link, maxBytes: maxBytes}
}

// Enqueue adds a packet, tail-dropping when the queue is full. Returns false
// on drop.
func (q *EgressQueue) Enqueue(p *Packet, now time.Time) bool {
	if q.link.Outage() || q.link.RandomDrop(now, q.Util()) {
		q.mu.Lock()
		q.dropped++
		q.mu.Unlock()
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.bytes+p.Bytes > q.maxBytes {
		q.dropped++
		return false
	}
	q.pkts = append(q.pkts, p)
	q.bytes += p.Bytes
	q.enqueued++
	return true
}

// Dequeue removes and returns the head packet, or nil.
func (q *EgressQueue) Dequeue() *Packet {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pkts) == 0 {
		return nil
	}
	p := q.pkts[0]
	q.pkts = q.pkts[1:]
	q.bytes -= p.Bytes
	return p
}

// Util returns current queue fill relative to the byte limit.
func (q *EgressQueue) Util() float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return float64(q.bytes) / float64(q.maxBytes)
}

// Bytes queued.
func (q *EgressQueue) Bytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

// Stats returns cumulative counters.
func (q *EgressQueue) Stats() (enqueued, dropped int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.enqueued, q.dropped
}
