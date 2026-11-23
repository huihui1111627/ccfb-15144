package edge

import (
	"testing"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/network"
)

func testLinks(t *testing.T) []*network.LinkModel {
	t.Helper()
	mk := func(typ model.LinkType, rtt, loss float64, up float64) *network.LinkModel {
		return network.NewLink(network.LinkConfig{
			ID: model.LinkID{EdgeID: "store-0001", Type: typ}, Type: typ,
			BaseRTT: rtt, BaseJit: 4, BaseLoss: loss,
			UpBps: up, DownBps: up * 2, Seed: int64(len(typ)) + 7,
		})
	}
	return []*network.LinkModel{
		mk(model.LinkMPLS, 20, 0.001, 10_000_000),
		mk(model.LinkBroadband, 40, 0.003, 100_000_000),
		mk(model.LinkCellular, 55, 0.006, 20_000_000),
	}
}

func testKey(app model.AppID, dst string, port uint16) model.FlowKey {
	return model.FlowKey{
		Proto: "tcp",
		Src:   model.Endpoint{IP: "192.168.1.10", Port: 40001},
		Dst:   model.Endpoint{IP: dst, Port: port},
	}
}

func runTicks(t *testing.T, e *Engine, keys []model.FlowKey, demands map[model.FlowKey]int64, n int) map[model.FlowKey]TickOutput {
	t.Helper()
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	last := map[model.FlowKey]TickOutput{}
	for i := 0; i < n; i++ {
		now = now.Add(time.Second)
		in := make([]TickInput, 0, len(keys))
		for _, k := range keys {
			in = append(in, TickInput{Key: k, Demand: demands[k]})
		}
		for _, o := range e.Tick(now, in) {
			last[o.Key] = o
		}
	}
	return last
}

func TestBaselineAlwaysMPLS(t *testing.T) {
	links := testLinks(t)
	e := NewEngine("store-0001", ModeBaseline, model.DefaultPolicy(), links)
	// 让专线拥塞、宽带空闲
	cfg := links[0].Config()
	cfg.UpBps = 1_000_000
	links[0].SetConfig(cfg)

	k := testKey(model.AppWeb, "172.16.4.10", 443)
	e.AdmitFlow(NewFlow{Key: k, First: true, Payload: []byte{0x16, 0x03}})
	out := runTicks(t, e, []model.FlowKey{k}, map[model.FlowKey]int64{k: 100_000}, 5)
	if out[k].Primary != model.LinkMPLS {
		t.Fatalf("baseline picked %s", out[k].Primary)
	}
}

func TestHysteresisMigrationOnOutage(t *testing.T) {
	links := testLinks(t)
	e := NewEngine("store-0001", ModeSDWAN, model.DefaultPolicy(), links)
	bb := links[1]

	// web 偏好宽带
	k := testKey(model.AppWeb, "172.16.4.10", 443)
	f := e.AdmitFlow(NewFlow{Key: k, First: true})
	if f.Primary != model.LinkBroadband {
		t.Fatalf("initial link = %s, want broadband", f.Primary)
	}

	// 宽带中断，先跑 5 秒：滞回 HoldDown=20s，不应迁移
	bb.SetOutage(true)
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		e.Tick(now, []TickInput{{Key: k, Demand: 10_000}})
	}
	if f2, _ := e.Flow(k); f2.Primary != model.LinkBroadband {
		t.Fatalf("migrated within holddown: %s", f2.Primary)
	}
	// 跑过 HoldDown 后应迁到专线
	for i := 0; i < 25; i++ {
		now = now.Add(time.Second)
		e.Tick(now, []TickInput{{Key: k, Demand: 10_000}})
	}
	if f2, _ := e.Flow(k); f2.Primary != model.LinkMPLS {
		t.Fatalf("after holddown primary = %s, want mpls", f2.Primary)
	}
}

func TestDualReplicationWhenAllLinksBad(t *testing.T) {
	links := testLinks(t)
	pol := model.DefaultPolicy()
	e := NewEngine("store-0001", ModeSDWAN, pol, links)
	// 所有链路高丢包，超过 POS 的 1% 门限
	for _, l := range links {
		cfg := l.Config()
		cfg.BaseLoss = 0.3
		l.SetConfig(cfg)
	}
	k := testKey(model.AppPOS, "10.8.0.11", 443)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	out := runTicks(t, e, []model.FlowKey{k}, map[model.FlowKey]int64{k: 5_000}, 35)
	o := out[k]
	if o.Replica == "" {
		t.Fatalf("critical flow not replicated, reason=%s links=%v", o.Why, o.Sent)
	}
	if len(o.Sent) < 2 {
		t.Fatalf("expected bytes on 2 links, got %v", o.Sent)
	}
}

func TestBulkShapedAndCheapLinkOnly(t *testing.T) {
	links := testLinks(t)
	pol := model.DefaultPolicy()
	pol.BulkCapBps = 4_000_000 // 500 KB/s
	e := NewEngine("store-0001", ModeSDWAN, pol, links)

	k := testKey(model.AppBulk, "10.10.0.5", 8443)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	demand := int64(5_000_000) // 想发 5MB/s
	out := runTicks(t, e, []model.FlowKey{k}, map[model.FlowKey]int64{k: demand}, 5)
	o := out[k]
	if o.Primary != model.LinkBroadband {
		t.Fatalf("bulk on %s, want broadband", o.Primary)
	}
	if o.Throttled <= 0 {
		t.Fatalf("bulk not throttled: %+v", o)
	}
	if o.Sent[model.LinkIndex(model.LinkMPLS)] != 0 {
		t.Fatal("bulk must not use expensive MPLS while broadband is healthy")
	}
}

func TestReceiverDedup(t *testing.T) {
	links := testLinks(t)
	e := NewEngine("store-0001", ModeSDWAN, model.DefaultPolicy(), links)
	k := testKey(model.AppPOS, "10.8.0.11", 443)
	if !e.DedupFirst(k, 1) {
		t.Fatal("first copy must pass")
	}
	if e.DedupFirst(k, 1) {
		t.Fatal("duplicate copy must be suppressed")
	}
	if !e.DedupFirst(k, 2) {
		t.Fatal("new sequence must pass")
	}
}

func TestControllerLossKeepsLastPolicy(t *testing.T) {
	links := testLinks(t)
	pol := model.DefaultPolicy()
	e := NewEngine("store-0001", ModeSDWAN, pol, links)
	e.SetControllerUp(false)
	// 失联期间照常选路
	k := testKey(model.AppVC, "10.9.0.20", 5004)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	out := runTicks(t, e, []model.FlowKey{k}, map[model.FlowKey]int64{k: 10_000}, 3)
	if out[k].Primary == "" {
		t.Fatal("edge stopped forwarding while controller is down")
	}
	if e.Policy().Version != pol.Version {
		t.Fatal("edge lost its last policy during outage")
	}
}
