package edge

import (
	"testing"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/network"
)

// BenchmarkTick measures the per-edge per-tick cost with a realistic mix of
// ~10 active flows, the load one store carries in the acceptance simulation.
func BenchmarkTick(b *testing.B) {
	links := benchLinks()
	e := NewEngine("store-0001", ModeSDWAN, model.DefaultPolicy(), links)
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	apps := []struct {
		app  model.AppID
		dst  string
		port uint16
	}{
		{model.AppPOS, "10.8.0.11", 443},
		{model.AppPOS, "100.64.10.7", 7000},
		{model.AppVC, "10.9.0.20", 5004},
		{model.AppVC, "10.9.0.21", 5060},
		{model.AppBulk, "10.10.0.5", 8443},
		{model.AppBulk, "10.10.2.9", 443},
		{model.AppWeb, "172.16.4.10", 443},
		{model.AppWeb, "172.16.4.11", 80},
		{model.AppGeneral, "198.51.100.23", 9201},
		{model.AppGeneral, "203.0.113.77", 6123},
	}
	var keys []model.FlowKey
	for i, a := range apps {
		k := testKey(a.app, a.dst, a.port)
		k.Src.Port = uint16(40000 + i)
		keys = append(keys, k)
		e.AdmitFlow(NewFlow{Key: k, First: true})
	}

	in := make([]TickInput, len(keys))
	for i, k := range keys {
		in[i] = TickInput{Key: k, Demand: 50_000}
	}
	// 预热，让探测窗口填满
	for i := 0; i < 35; i++ {
		now = now.Add(time.Second)
		e.Tick(now, in)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		now = now.Add(time.Second)
		e.Tick(now, in)
	}
}

func benchLinks() []*network.LinkModel {
	mk := func(typ model.LinkType, rtt, loss, up float64) *network.LinkModel {
		return network.NewLink(network.LinkConfig{
			ID: model.LinkID{EdgeID: "store-0001", Type: typ}, Type: typ,
			BaseRTT: rtt, BaseJit: 4, BaseLoss: loss,
			UpBps: up, DownBps: up * 2, Seed: int64(model.LinkIndex(typ)) + 7,
		})
	}
	return []*network.LinkModel{
		mk(model.LinkMPLS, 20, 0.001, 10_000_000),
		mk(model.LinkBroadband, 40, 0.003, 100_000_000),
		mk(model.LinkCellular, 55, 0.006, 20_000_000),
	}
}
