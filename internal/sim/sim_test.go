package sim

import (
	"testing"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
	"sdwan/internal/network"
)

func TestFluidRunsAndProducesMetrics(t *testing.T) {
	cfg := FluidConfig{
		Stores: 5, Duration: 30 * time.Minute, Seed: 7,
		LoadMul: 1, SyncEvery: 10 * time.Second,
	}
	m := NewFluid(edge.ModeSDWAN, cfg).Run()
	if m.TickCount() == 0 {
		t.Fatal("no ticks observed")
	}
	var totalSamples int64
	for _, a := range m.Snapshot().App {
		totalSamples += a.Samples
	}
	if totalSamples == 0 {
		t.Fatal("no flow samples generated")
	}
	if m.ClassRate() < 0.95 {
		t.Fatalf("classification rate %.3f too low", m.ClassRate())
	}
	// 大流量应主要走便宜链路（宽带），而非专线
	bb := m.Snapshot().Link[model.LinkBroadband].BytesUp
	mpls := m.Snapshot().Link[model.LinkMPLS].BytesUp
	if bb <= mpls {
		t.Fatalf("expected broadband (%d) to carry more than MPLS (%d)", bb, mpls)
	}
}

func TestFaultInjectionTogglesOutage(t *testing.T) {
	tp := buildSmallTopology()
	scen := &Scenario{Faults: []Fault{{
		Kind: FaultBroadbandDown, Start: time.Now(), Duration: time.Hour,
		Targets: []string{tp.Stores[0].Info.ID},
	}}}
	fr := newFaultRuntime(scen)
	changed := fr.Apply(time.Now(), tp, func(bool) {})
	if len(changed) != 1 {
		t.Fatalf("expected 1 fault activation, got %d", len(changed))
	}
	if !tp.Stores[0].Links[model.LinkBroadband].Outage() {
		t.Fatal("broadband should be down")
	}
	if tp.Stores[1].Links[model.LinkBroadband].Outage() {
		t.Fatal("non-target store must be unaffected")
	}
}

func TestFaultRestoresAfterDuration(t *testing.T) {
	tp := buildSmallTopology()
	start := time.Now()
	scen := &Scenario{Faults: []Fault{{
		Kind: FaultBroadbandDown, Start: start, Duration: time.Minute,
	}}}
	fr := newFaultRuntime(scen)
	fr.Apply(start.Add(30*time.Second), tp, func(bool) {})
	if !tp.Stores[0].Links[model.LinkBroadband].Outage() {
		t.Fatal("should be down during window")
	}
	fr.Apply(start.Add(2*time.Minute), tp, func(bool) {})
	if tp.Stores[0].Links[model.LinkBroadband].Outage() {
		t.Fatal("should recover after window")
	}
}

func TestEveningBroadbandWorse(t *testing.T) {
	tp := buildSmallTopology()
	bb := tp.Stores[0].Links[model.LinkBroadband]
	day := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 11, 23, 21, 30, 0, 0, time.UTC)
	qDay := bb.Sample(day, 0.2)
	qEve := bb.Sample(evening, 0.2)
	if qEve.RTT <= qDay.RTT || qEve.UpBps >= qDay.UpBps {
		t.Fatalf("evening broadband should be worse: day=%+v eve=%+v", qDay, qEve)
	}
}

func buildSmallTopology() *network.Topology {
	return network.BuildTopology(4, 123)
}
