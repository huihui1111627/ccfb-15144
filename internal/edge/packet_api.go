package edge

import (
	"time"

	"sdwan/internal/classify"
	"sdwan/internal/model"
)

// RouteExisting runs the full per-tick decision for an already-admitted flow
// (hysteresis migration + replication control) and returns the primary and
// optional replica link. Used by the packet-level engine.
func (e *Engine) RouteExisting(now time.Time, k model.FlowKey) (primary, replica model.LinkType) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
	f, ok := e.flows[k]
	if !ok {
		return "", ""
	}
	d := e.chooseLink(f)
	if e.reevaluate(f, d) {
		prof := e.policy.Profiles[f.App]
		if prof == nil || !prof.Realtime {
			e.reorder.Arm(f.Key, now)
		}
	}
	f.Migrating = false
	return f.Primary, f.Replica
}

// AccountBytes records uplink bytes on a link (packet engine).
func (e *Engine) AccountBytes(t model.LinkType, n int64) {
	e.mu.Lock()
	e.addBytes(t, n)
	e.mu.Unlock()
}

// DedupFirst is the receiver-side de-duplication check for replicated flows.
func (e *Engine) DedupFirst(k model.FlowKey, seq uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dedup.First(k, seq)
}

// ReorderBuffered reports whether a flow is still inside its migration reorder
// window at time now.
func (e *Engine) ReorderBuffered(k model.FlowKey, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reorder.Buffered(k, now)
}

// SetNow updates the engine's virtual time without running a full tick.
func (e *Engine) SetNow(now time.Time) {
	e.mu.Lock()
	e.now = now
	e.mu.Unlock()
}

// OrderedTypes returns the local link types in the device's stable order,
// filtered to those present on this store.
func (e *Engine) OrderedTypes(have []model.LinkType) []model.LinkType {
	e.mu.Lock()
	defer e.mu.Unlock()
	exist := map[model.LinkType]bool{}
	for _, t := range e.order {
		exist[t] = true
	}
	out := make([]model.LinkType, 0, len(have))
	for _, t := range have {
		if exist[t] {
			out = append(out, t)
		}
	}
	return out
}

// BehaviorObs re-export so the sim packet engine can stay decoupled if needed.
type BehaviorObs = classify.BehaviorObs
