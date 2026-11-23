package controller

import (
	"errors"

	"sdwan/internal/model"
)

// ErrUnreachable means the edge cannot reach the controller; it must continue
// with its last known policy.
var ErrUnreachable = errors.New("controller unreachable")

// Client abstracts the edge -> controller channel so simulation and real
// deployments share the same edge code.
type Client interface {
	Pull(edgeID string) (*model.Policy, error)
	Report(t model.EdgeTelemetry) error
}

// InProcessClient wires an edge to a controller inside one process (the
// simulation). It remembers the last policy and keeps serving the edge from
// that cache when the controller link goes down — the requirement that edges
// keep forwarding under the last policy during controller outages.
type InProcessClient struct {
	c    *Controller
	last *model.Policy
}

// NewInProcessClient seeds the client with a local fallback policy so an edge
// works even before its first successful controller contact.
func NewInProcessClient(c *Controller, fallback *model.Policy) *InProcessClient {
	return &InProcessClient{c: c, last: fallback}
}

// Pull fetches the policy; on failure the cached last policy is returned.
func (cl *InProcessClient) Pull(edgeID string) (*model.Policy, error) {
	p, err := cl.c.PullPolicy(edgeID)
	if err != nil {
		return cl.last, err
	}
	cl.last = p
	return p, nil
}

// Report pushes telemetry; errors are tolerated (edge stays autonomous).
func (cl *InProcessClient) Report(t model.EdgeTelemetry) error {
	return cl.c.Report(t)
}

// LastPolicy exposes the cached policy.
func (cl *InProcessClient) LastPolicy() *model.Policy { return cl.last }

// Agent drives the periodic edge-side controller sync.
type Agent struct {
	edgeID string
	client Client
	apply  func(*model.Policy)
	onUp   func(bool)
}

// NewAgent builds the sync agent. apply hot-reloads a new policy; onUp
// reports reachability transitions.
func NewAgent(edgeID string, client Client, apply func(*model.Policy), onUp func(bool)) *Agent {
	return &Agent{edgeID: edgeID, client: client, apply: apply, onUp: onUp}
}

// Sync performs one pull/report cycle and returns whether the controller was
// reachable. The simulation calls it once per sync interval.
func (a *Agent) Sync(t model.EdgeTelemetry) bool {
	p, err := a.client.Pull(a.edgeID)
	up := err == nil
	if up && p != nil {
		a.apply(p)
	}
	if a.onUp != nil {
		a.onUp(up)
	}
	_ = a.client.Report(t) // 失联时上报失败，边缘不受影响
	return up
}
