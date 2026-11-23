package sim

import (
	"sync"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
	"sdwan/internal/network"
)

// ProbeJob describes one probe packet the packet engine puts on a link.
type ProbeJob struct {
	Link model.LinkType
	Seq  uint64
	Sent time.Time
}

// PacketStats counts receiver-side events for the dashboard.
type PacketStats struct {
	Delivered  int64
	Duplicates int64 // 复制发送带来的重复包（被去重丢弃）
	Reordered  int64 // 迁移窗口内的乱序包
	Lost       int64
	ProbeRTTs  []float64
}

// PacketEngine runs a small packet-level simulation in wall-clock time: real
// probe packets traverse EgressQueues, data packets are replicated/de-duped,
// and migration reorder windows are exercised end to end. It powers the live
// dashboard; the acceptance numbers come from the deterministic fluid engine.
type PacketEngine struct {
	mu sync.Mutex

	store  *network.Store
	eng    *edge.Engine
	gen    *TrafficGenerator
	queues map[model.LinkType]*network.EgressQueue
	stats  map[model.LinkType]*PacketStats
	seq    map[model.FlowKey]uint64
	maxSeq map[model.FlowKey]uint64 // 接收端已交付最大序号

	now     time.Time
	t0      time.Time
	onEvent func(ev model.FlowEvent)
	flowBps map[model.FlowKey]float64 // 最近一个 tick 每流发送速率
}

// NewPacketEngine builds a packet engine for one store. t0 sets the starting
// (simulated) wall-clock so time-of-day effects (evening broadband) show.
func NewPacketEngine(st *network.Store, hub *network.Hub, mode edge.Mode, t0 time.Time, seed int64) *PacketEngine {
	var links []*network.LinkModel
	for _, t := range st.Info.Links {
		links = append(links, st.Links[t])
	}
	pe := &PacketEngine{
		store:   st,
		eng:     edge.NewEngine(st.Info.ID, mode, model.DefaultPolicy(), links),
		gen:     NewGenerator(st, hub, seed, GeneratorConfig{LoadMul: 1}),
		queues:  map[model.LinkType]*network.EgressQueue{},
		stats:   map[model.LinkType]*PacketStats{},
		seq:     map[model.FlowKey]uint64{},
		maxSeq:  map[model.FlowKey]uint64{},
		flowBps: map[model.FlowKey]float64{},
		now:     t0, t0: t0,
	}
	for _, l := range links {
		pe.queues[l.Type()] = network.NewEgressQueue(l)
		pe.stats[l.Type()] = &PacketStats{}
	}
	return pe
}

// Engine exposes the edge engine (web fault buttons / snapshots).
func (pe *PacketEngine) Engine() *edge.Engine { return pe.eng }

// Store exposes the store (link models for fault injection).
func (pe *PacketEngine) Store() *network.Store { return pe.store }

// OnEvent registers a migration/event sink.
func (pe *PacketEngine) OnEvent(fn func(model.FlowEvent)) { pe.onEvent = fn }

// emitProbes sends one probe per link (requirement: continuous probing on
// every link). Returns the jobs so the driver can schedule their acks.
func (pe *PacketEngine) emitProbes() []ProbeJob {
	types := pe.eng.OrderedTypes(pe.store.Info.Links)
	jobs := make([]ProbeJob, 0, len(types))
	for _, t := range types {
		tr := pe.eng.Tracker(t)
		seq := tr.SendProbe(pe.now)
		p := &network.Packet{
			Probe: true, ProbeID: seq, Link: t, StoreID: pe.store.Info.ID,
			SentAt: pe.now, Bytes: 64,
		}
		if pe.queues[t].Enqueue(p, pe.now) {
			jobs = append(jobs, ProbeJob{Link: t, Seq: seq, Sent: pe.now})
		}
	}
	return jobs
}

// Tick advances one virtual second of packet-level simulation. Delivery of
// queued packets is modeled synchronously with the link's delay distribution;
// in the live demo 1 virtual second maps to 1 wall-clock second.
func (pe *PacketEngine) Tick(now time.Time) {
	pe.mu.Lock()
	pe.now = now
	// 1) 持续探测
	jobs := pe.emitProbes()

	// 2) 生成流量并分类、选路
	demands := pe.gen.Step(now)
	tickBytes := map[model.FlowKey]int64{}
	for _, d := range demands {
		// 只为仍处于 pending 的流做行为归类
		if existing, ok := pe.eng.Flow(d.Key); ok && existing.ClassWhy == "pending" {
			pe.eng.PromoteByBehavior(d.Key, edge.BehaviorObs{
				Packets: d.PktCount, Bytes: d.Bytes,
				MeanPkt: float64(d.MeanPkt), IntervalS: d.IntervalS,
			})
		}
		f := pe.eng.AdmitFlow(edge.NewFlow{
			Key: d.Key, First: d.First, Payload: d.Payload,
			Truth: d.App, RateBps: float64(d.Bytes) * 8,
		})
		primary, replica := pe.eng.RouteExisting(now, f.Key)
		nPackets := d.PktCount
		if nPackets <= 0 {
			nPackets = 1
		}
		size := d.MeanPkt
		if size <= 0 {
			size = max1i(int(d.Bytes) / max1i(nPackets))
		}
		for i := 0; i < nPackets; i++ {
			pe.seq[d.Key]++
			seq := pe.seq[d.Key]
			if pe.sendOne(d, primary, seq, size, false) {
				tickBytes[d.Key] += int64(size)
			}
			if replica != "" && pe.sendOne(d, replica, seq, size, true) {
				tickBytes[d.Key] += int64(size)
			}
		}
		if d.Ending {
			pe.eng.EndFlow(d.Key)
			delete(pe.flowBps, d.Key)
		}
	}
	// 本 tick 每流速率（bps）
	pe.flowBps = map[model.FlowKey]float64{}
	for k, n := range tickBytes {
		pe.flowBps[k] = float64(n) * 8
	}

	// 3) 排空各链路队列，按链路模型交付/丢弃
	for t, q := range pe.queues {
		tr := pe.eng.Tracker(t)
		for {
			p := q.Dequeue()
			if p == nil {
				break
			}
			link := tr.Link()
			if link.Outage() {
				pe.stats[t].Lost++
				continue
			}
			delay := link.OneWayDelay(now, q.Util())
			arrival := now.Add(delay)
			if p.Probe {
				// 回包再走半程，总 RTT 约 2 倍单程
				pe.deliverProbe(t, p.ProbeID, p.SentAt, arrival.Add(delay))
				continue
			}
			pe.deliverData(t, p, arrival)
		}
		tr.SetUtil(q.Util())
	}

	// 4) 探测超时处理
	for _, t := range pe.eng.OrderedTypes(pe.store.Info.Links) {
		pe.eng.Tracker(t).TimeoutPending(now, 3*time.Second)
	}

	// 5) 事件外抛
	evs := pe.eng.DrainEvents()
	pe.mu.Unlock()
	for _, ev := range evs {
		if pe.onEvent != nil {
			pe.onEvent(ev)
		}
	}
	_ = jobs
}

func (pe *PacketEngine) sendOne(d FlowDemand, link model.LinkType, seq uint64, size int, replica bool) bool {
	p := &network.Packet{
		Flow: d.Key, App: d.App, Seq: seq, Bytes: size,
		Proto: d.Key.Proto, First: d.First, Payload: nil,
		Link: link, StoreID: pe.store.Info.ID, Replica: replica,
	}
	if !pe.queues[link].Enqueue(p, pe.now) {
		pe.stats[link].Lost++
		return false
	}
	pe.eng.AccountBytes(link, int64(size))
	return true
}

// FlowBps returns the most recent per-flow sending rate in bps.
func (pe *PacketEngine) FlowBps() map[model.FlowKey]float64 {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	out := make(map[model.FlowKey]float64, len(pe.flowBps))
	for k, v := range pe.flowBps {
		out[k] = v
	}
	return out
}

func (pe *PacketEngine) deliverProbe(t model.LinkType, seq uint64, sent, arrival time.Time) {
	// 到达即回调（演示模式下 arrival 仅用于展示，测量以虚拟 now 为准）
	pe.eng.Tracker(t).RecvProbeAck(seq, sent, arrival)
	st := pe.stats[t]
	st.ProbeRTTs = append(st.ProbeRTTs, float64(arrival.Sub(sent))/float64(time.Millisecond))
	if len(st.ProbeRTTs) > 120 {
		st.ProbeRTTs = st.ProbeRTTs[len(st.ProbeRTTs)-120:]
	}
}

func (pe *PacketEngine) deliverData(t model.LinkType, p *network.Packet, arrival time.Time) {
	st := pe.stats[t]
	// 接收端去重：双链路复制时第二个副本被丢弃
	if !pe.eng.DedupFirst(p.Flow, p.Seq) {
		st.Duplicates++
		return
	}
	// 乱序检查（迁移窗口内容易出现）
	prev := pe.maxSeq[p.Flow]
	if p.Seq < prev && pe.eng.ReorderBuffered(p.Flow, arrival) {
		st.Reordered++
	}
	if p.Seq > prev {
		pe.maxSeq[p.Flow] = p.Seq
	}
	st.Delivered++
}

// Stats returns per-link receiver counters.
func (pe *PacketEngine) Stats() map[model.LinkType]PacketStats {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	out := make(map[model.LinkType]PacketStats, len(pe.stats))
	for k, v := range pe.stats {
		out[k] = *v
	}
	return out
}

func max1i(x int) int {
	if x < 1 {
		return 1
	}
	return x
}
