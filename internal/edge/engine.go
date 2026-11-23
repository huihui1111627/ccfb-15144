// Package edge implements the SD-WAN edge device forwarding engine:
// application-aware link selection, hysteresis-driven flow migration with
// reorder control, dual-link replication with receiver de-duplication and bulk
// traffic shaping on cheap links. The same engine drives both the fluid
// acceptance engine and the packet-level demo engine.
package edge

import (
	"sync"
	"time"

	"sdwan/internal/classify"
	"sdwan/internal/model"
	"sdwan/internal/network"
	"sdwan/internal/probe"
)

// Mode controls baseline (all-MPLS) vs full SD-WAN behavior.
type Mode int

const (
	ModeBaseline Mode = iota // 对照组：全部走专线
	ModeSDWAN                // 实验组：按质量选路
)

// Engine is one edge device's decision plane.
type Engine struct {
	mu sync.Mutex

	id     string
	mode   Mode
	policy *model.Policy
	cls    *classify.Classifier

	trackers map[model.LinkType]*probe.Tracker
	order    []model.LinkType // 本机存在的链路，固定顺序
	shaper   *bulkShaper

	flows map[model.FlowKey]*model.FlowState
	// scratch is the per-engine, per-tick reusable routing scoreboard; it is
	// only touched under mu so concurrent access is impossible.
	scratch scoreBoard
	// 每秒链路上的已分配字节（流体引擎用），每 tick 清空
	tickBytes map[model.LinkType]int64
	// 累计上行字节 / 费用
	bytesUp map[model.LinkType]int64

	events    []model.FlowEvent
	maxEvents int

	controllerUp bool
	offlineTicks int

	now time.Time

	// 接收端：去重 + 重排
	dedup   *deduper
	reorder *reorderBuffer
}

// NewEngine constructs an edge over the given link models.
func NewEngine(id string, mode Mode, policy *model.Policy, links []*network.LinkModel) *Engine {
	e := &Engine{
		id:        id,
		mode:      mode,
		policy:    policy,
		cls:       classify.New(policy),
		trackers:  map[model.LinkType]*probe.Tracker{},
		flows:     map[model.FlowKey]*model.FlowState{},
		tickBytes: map[model.LinkType]int64{},
		bytesUp:   map[model.LinkType]int64{},
		maxEvents: 500,
		shaper:    newBulkShaper(policy.BulkCapBps),
		dedup:     newDeduper(1 << 16),
		reorder:   newReorderBuffer(policy.ReorderWindow),
	}
	for _, l := range links {
		t := probe.NewTracker(l)
		e.trackers[l.Type()] = t
		e.order = append(e.order, l.Type())
		e.bytesUp[l.Type()] = 0
	}
	return e
}

// ID returns the edge identifier.
func (e *Engine) ID() string { return e.id }

// Classifier exposes the classifier (the packet engine feeds DNS/first packets).
func (e *Engine) Classifier() *classify.Classifier { return e.cls }

// SetControllerUp is driven by the (simulated) controller connection state.
func (e *Engine) SetControllerUp(up bool) {
	e.mu.Lock()
	e.controllerUp = up
	if !up {
		e.offlineTicks++
	} else {
		e.offlineTicks = 0
	}
	e.mu.Unlock()
}

// ControllerUp reports the current connection state.
func (e *Engine) ControllerUp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.controllerUp
}

// Policy returns the active policy. When the controller is unreachable the
// edge keeps operating with the last pushed policy (this one).
func (e *Engine) Policy() *model.Policy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.policy
}

// UpdatePolicy hot-applies a newly pushed policy.
func (e *Engine) UpdatePolicy(p *model.Policy) {
	e.mu.Lock()
	e.policy = p
	e.shaper.setCap(p.BulkCapBps)
	e.reorder.window = p.ReorderWindow
	e.mu.Unlock()
	e.cls.UpdatePolicy(p)
}

// Tracker returns the probe tracker for a link.
func (e *Engine) Tracker(t model.LinkType) *probe.Tracker {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.trackers[t]
}

// Trackers returns all trackers.
func (e *Engine) Trackers() map[model.LinkType]*probe.Tracker {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[model.LinkType]*probe.Tracker, len(e.trackers))
	for k, v := range e.trackers {
		out[k] = v
	}
	return out
}

// Now returns last tick's virtual time.
func (e *Engine) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

// DrainEvents returns and clears queued flow events.
func (e *Engine) DrainEvents() []model.FlowEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.events
	e.events = nil
	return out
}

func (e *Engine) pushEvent(ev model.FlowEvent) {
	ev.At = e.now
	ev.EdgeID = e.id
	if len(e.events) >= e.maxEvents {
		e.events = e.events[1:]
	}
	e.events = append(e.events, ev)
}

// Flow looks up a cached flow state.
func (e *Engine) Flow(k model.FlowKey) (*model.FlowState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.flows[k]
	return f, ok
}

// Flows snapshots active flow states.
func (e *Engine) Flows() []model.FlowState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]model.FlowState, 0, len(e.flows))
	for _, f := range e.flows {
		if f.Active {
			out = append(out, *f)
		}
	}
	return out
}

// EndFlow terminates a flow: its classification cache and forwarding state
// are released.
func (e *Engine) EndFlow(k model.FlowKey) {
	e.mu.Lock()
	f, ok := e.flows[k]
	if ok {
		e.pushEvent(model.FlowEvent{Flow: k, App: f.App, To: f.Primary, Reason: "flow-end"})
		delete(e.flows, k)
	}
	e.mu.Unlock()
	e.cls.End(k)
}

// ObserveDNS forwards a DNS observation to the classifier.
func (e *Engine) ObserveDNS(domain, ip string) { e.cls.ObserveDNS(domain, ip) }

// BytesUp returns cumulative uplink bytes per link.
func (e *Engine) BytesUp() map[model.LinkType]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[model.LinkType]int64, len(e.bytesUp))
	for k, v := range e.bytesUp {
		out[k] = v
	}
	return out
}

// addBytes accounts traffic and link cost.
func (e *Engine) addBytes(t model.LinkType, n int64) {
	e.tickBytes[t] += n
	e.bytesUp[t] += n
}

// TickBytes returns bytes allocated on each link during the current tick and
// resets the counters (called by the engines once per simulated second).
func (e *Engine) TickBytes() map[model.LinkType]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.tickBytes
	e.tickBytes = map[model.LinkType]int64{}
	return out
}
