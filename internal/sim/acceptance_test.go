package sim

import (
	"testing"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
)

// TestAcceptanceInvariants runs a short but fault-bearing comparison and checks
// the headline acceptance invariants hold end to end.
func TestAcceptanceInvariants(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cfg := FluidConfig{
		Stores: 12, Duration: 12 * time.Hour, Seed: 2026,
		LoadMul: 1, SyncEvery: 10 * time.Second,
	}

	base := NewFluid(edge.ModeBaseline, cfg)
	base.scen = acceptanceFaults(t0)
	base.fr = newFaultRuntime(base.scen)
	base.Run()

	sd := NewFluid(edge.ModeSDWAN, cfg)
	sd.scen = acceptanceFaults(t0)
	sd.fr = newFaultRuntime(sd.scen)
	sd.Run()

	bm, sm := base.Metrics().Snapshot(), sd.Metrics().Snapshot()

	// 1. 关键应用达标率：SD-WAN 不低于基线
	for _, app := range []model.AppID{model.AppPOS, model.AppVC} {
		if sm.App[app].SLARate() < bm.App[app].SLARate() {
			t.Fatalf("app %s SLA regressed: base=%.3f sdwan=%.3f",
				app, bm.App[app].SLARate(), sm.App[app].SLARate())
		}
	}

	// 2. 专线用量：SD-WAN 显著下降
	bmpls := bm.Link[model.LinkMPLS].BytesUp
	smpls := sm.Link[model.LinkMPLS].BytesUp
	if bmpls <= 0 || smpls >= bmpls {
		t.Fatalf("MPLS usage not reduced: base=%d sdwan=%d", bmpls, smpls)
	}

	// 3. 确实发生了流迁移
	if sm.Migrations == 0 {
		t.Fatal("no migrations recorded in SD-WAN mode")
	}

	// 4. 大流量主要走便宜链路
	if sm.Link[model.LinkBroadband].BytesUp <= sm.Link[model.LinkMPLS].BytesUp {
		t.Fatal("bulk/cheap traffic not steered to broadband")
	}

	// 5. 识别准确率高
	if sm.ClassRate() < 0.95 {
		t.Fatalf("classification rate too low: %.3f", sm.ClassRate())
	}
}

func acceptanceFaults(t0 time.Time) *Scenario {
	return &Scenario{Faults: []Fault{
		{Kind: FaultMPLSCongestion, Start: t0.Add(3 * time.Hour), Duration: 2 * time.Hour, CapMult: 0.2},
		{Kind: FaultBroadbandDown, Start: t0.Add(6 * time.Hour), Duration: time.Hour,
			Targets: []string{"store-0001", "store-0005"}},
		{Kind: FaultControllerDown, Start: t0.Add(9 * time.Hour), Duration: time.Hour},
	}}
}
