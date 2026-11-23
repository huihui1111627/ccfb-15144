package edge

import (
	"time"

	"sdwan/internal/model"
)

// Snapshot builds the telemetry record pushed to the central controller.
func (e *Engine) Snapshot(flowBps map[model.FlowKey]float64) model.EdgeTelemetry {
	e.mu.Lock()
	defer e.mu.Unlock()

	snap := model.EdgeTelemetry{
		EdgeID: e.id, At: e.now,
		Links:        map[model.LinkType]model.LinkTelemetry{},
		PolicyVer:    e.policy.Version,
		ControllerUp: e.controllerUp,
		Events:       append([]model.FlowEvent(nil), e.events...),
	}
	for _, t := range e.order {
		tr := e.trackers[t]
		q := tr.Quality()
		snap.Links[t] = model.LinkTelemetry{
			Type:     t,
			Quality:  q,
			BpsInUse: q.Util * q.UpBps,
			BytesUp:  e.bytesUp[t],
			Cost:     float64(e.bytesUp[t]) / 1e9 * e.policy.CostPerGB[t],
			Outage:   !q.Healthy,
		}
	}
	for _, f := range e.flows {
		if !f.Active {
			continue
		}
		snap.Flows = append(snap.Flows, model.FlowTelemetry{
			Key: f.Key, App: f.App, Primary: f.Primary,
			Replica: f.Replica, Bps: flowBps[f.Key], ClassWhy: f.ClassWhy,
		})
	}
	return snap
}

// ReapFlows ends flows idle for longer than ttl (half-open/UDP timeouts etc.).
func (e *Engine) ReapFlows(ttl time.Duration) int {
	e.mu.Lock()
	var dead []model.FlowKey
	for k, f := range e.flows {
		if e.now.Sub(f.LastSeen) > ttl {
			dead = append(dead, k)
		}
	}
	e.mu.Unlock()
	for _, k := range dead {
		e.EndFlow(k)
	}
	return len(dead)
}
