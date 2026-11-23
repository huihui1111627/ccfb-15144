// Package controller implements the central SD-WAN controller: policy
// distribution, network-wide telemetry aggregation and fault visibility.
// Edges talk to it through the Client interface so the same edge engine works
// against an in-process controller (simulation) or an HTTP one (real devices).
package controller

import (
	"sync"
	"time"

	"sdwan/internal/model"
)

// Controller is the central policy + telemetry service.
type Controller struct {
	mu      sync.Mutex
	policy  *model.Policy
	last    map[string]model.EdgeTelemetry
	updated map[string]time.Time
	down    map[string]bool // 模拟控制器与某边缘之间的链路中断
	verSeq  int64
}

// New constructs a controller with the given seed policy.
func New(p *model.Policy) *Controller {
	return &Controller{
		policy:  p,
		last:    map[string]model.EdgeTelemetry{},
		updated: map[string]time.Time{},
		down:    map[string]bool{},
		verSeq:  p.Version,
	}
}

// PushPolicy stores and versions a new policy; edges pick it up on pull.
func (c *Controller) PushPolicy(p *model.Policy) *model.Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.verSeq++
	p.Version = c.verSeq
	p.UpdatedAt = time.Now()
	cp := *p
	c.policy = &cp
	return c.policy
}

// CurrentPolicy returns the active policy.
func (c *Controller) CurrentPolicy() *model.Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *c.policy
	return &cp
}

// SetLinkDown simulates loss of the controller connection for an edge.
// While down the edge gets an error on pull/report and keeps its last policy.
func (c *Controller) SetLinkDown(edgeID string, down bool) {
	c.mu.Lock()
	c.down[edgeID] = down
	c.mu.Unlock()
}

// Report ingests one edge telemetry snapshot.
func (c *Controller) Report(t model.EdgeTelemetry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down[t.EdgeID] {
		return ErrUnreachable
	}
	c.last[t.EdgeID] = t
	c.updated[t.EdgeID] = time.Now()
	return nil
}

// PullPolicy returns the current policy when the edge is reachable.
func (c *Controller) PullPolicy(edgeID string) (*model.Policy, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down[edgeID] {
		return nil, ErrUnreachable
	}
	cp := *c.policy
	return &cp, nil
}

// Snapshot returns the most recent telemetry of every edge.
func (c *Controller) Snapshot() map[string]model.EdgeTelemetry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]model.EdgeTelemetry, len(c.last))
	for k, v := range c.last {
		out[k] = v
	}
	return out
}

// Edge returns one edge's latest snapshot.
func (c *Controller) Edge(id string) (model.EdgeTelemetry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.last[id]
	return t, ok
}

// LastUpdated reports when each edge last reported.
func (c *Controller) LastUpdated() map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time, len(c.updated))
	for k, v := range c.updated {
		out[k] = v
	}
	return out
}
